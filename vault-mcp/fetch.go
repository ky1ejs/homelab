package main

// fetch_attachment: download a URL straight to a file, and never let the bytes
// reach a model.
//
// WHY THIS EXISTS AT ALL. WebFetch cannot produce a file. It fetches a page,
// converts it to Markdown and runs a prompt against it with a small model, so
// what comes back is text — Claude never sees the response body. There is no
// setting that makes it save a PNG. Bash is denied on every surface here, so
// there is no curl either, and Claude Code's Write tool produces text while
// Read cannot open an image. Before this tool, no combination of permissions
// could put a photograph in the vault. That was a capability gap wearing a
// security policy's clothes, and enabling the web tools would not have closed
// it. See obsidian-vault/DECISIONS.md#fetching-attachments.
//
// WHY IT IS SAFER THAN THE WEB TOOLS IT SITS BESIDE. The bytes go from socket
// to disk. The caller gets a filename, a size and a content type, and never one
// byte of the body. A page that carries an injection can only deliver it to a
// model that reads the page; this tool reads it into a file instead. That is
// the quarantine the dual-LLM pattern describes, achieved by plumbing rather
// than by asking a filter to be right every time.
//
// WHAT IT REFUSES, and why each one is here rather than left to the caller:
//
//   - Plain HTTP. An attachment fetched over a channel anyone on the path can
//     rewrite is a file of unknown provenance with a trustworthy-looking name.
//   - Private, loopback, link-local and multicast addresses, checked at CONNECT
//     time rather than by parsing the hostname. The research container sits on
//     the NAS's network with the QNAP admin interface, the other stacks and the
//     dashboard's Docker socket proxy on it. Hostname parsing is not enough:
//     DNS can answer 127.0.0.1, and can answer differently on the second lookup
//     than the first. Checking the address the dialer actually reached closes
//     both, including on every redirect hop.
//   - Anything whose sniffed type disagrees with the destination extension. The
//     server's own Content-Type is a claim by the party we are guarding
//     against; the first 512 bytes are evidence.
//   - SVG, alone among the image types move_file will relocate. An SVG is a
//     document that can carry script, and this is the one tool that brings a
//     file in from outside. Fetch a PNG; move an SVG you made yourself.
//   - Overwriting. Same rule as move_file, for the same reason: overwrite is
//     the delete path wearing a different name.
//   - Anything the vault's own path rules refuse — dotted folders, AGENTS.md,
//     CLAUDE.md, an escape through a symlink. This tool reuses writablePath
//     rather than restating it, so the two can never drift.
//
// WHAT IT DOES NOT DEFEND AGAINST. A file the operator asked for, from a host
// the operator allowed, is fetched. This tool is not a malware scanner and does
// not pretend to be one. Its promise is narrower and worth stating exactly: the
// download cannot reach inside the network, cannot land outside the tree it was
// pointed at, cannot masquerade as a type it is not, and cannot say anything to
// the model that fetched it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrFetch is the class the tool layer turns into a message the model can act
// on. Wrapped rather than returned bare so the reason survives to the log.
var ErrFetch = errors.New("fetch refused")

// fetchableTypes maps a destination extension to the content type the bytes
// must actually sniff as.
//
// Deliberately SMALLER than attachmentExts in vault.go, and the gap is not an
// oversight. That list says what may be MOVED inside the vault, where the file
// is already yours; this one says what may be BROUGHT IN, where it is not. So:
// no SVG (script in a document), no .canvas (an Obsidian control file), and
// nothing whose type Go cannot sniff with confidence — a tiff or a heic would
// have to be trusted on the server's word, which is the thing being checked.
//
// The values are what http.DetectContentType returns, not what the server sent.
var fetchableTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".pdf":  "application/pdf",
}

// fetcher holds the one HTTP client this server ever uses to reach the open
// internet. Built once: the dial guard below is the security boundary, and a
// client constructed per call is a client somebody eventually constructs
// without it.
type fetcher struct {
	client   *http.Client
	maxBytes int64
	hosts    []string // empty means any host
}

func newFetcher(timeout time.Duration, maxBytes int64, hosts []string) *fetcher {
	f := &fetcher{maxBytes: maxBytes, hosts: hosts}

	// Control runs after the address is resolved and before the socket
	// connects, which is the only place that sees what we are ACTUALLY talking
	// to. A check on u.Host would be checking a name; this checks the peer.
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: cannot parse %q", ErrFetch, address)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("%w: %q is not an IP", ErrFetch, host)
			}
			if !routableIP(ip) {
				return fmt.Errorf("%w: %s is not a public address", ErrFetch, ip)
			}
			return nil
		},
	}

	f.client = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			DisableKeepAlives:     true,
			Proxy:                 nil, // never inherit a proxy we did not configure
		},
		// Redirects are followed, but every hop is re-checked. An open
		// redirector on an allowed host is otherwise a way to reach anywhere,
		// and http:// after an https:// start is a downgrade that silently
		// undoes the scheme rule.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("%w: too many redirects", ErrFetch)
			}
			return f.allowURL(req.URL)
		},
	}
	return f
}

// routableIP is the connect-time guard. Everything that is not plainly a public
// unicast address is refused, including the shapes that are easy to forget:
// IPv4-mapped IPv6 (::ffff:127.0.0.1), the cloud metadata address, and the
// unspecified address that some resolvers hand back for a name with no records.
func routableIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsLoopback(),
		ip.IsPrivate(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(),
		ip.IsUnspecified():
		return false
	}
	// 100.64.0.0/10, carrier-grade NAT, which is also Tailscale's range. The
	// NAS is on a tailnet; a fetch must not be a way to reach the rest of it.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	// 169.254.169.254 is covered by IsLinkLocalUnicast above. IPv6 unique-local
	// (fc00::/7) is not private per IsPrivate on every Go version, so check it.
	if len(ip) == net.IPv6len && ip.To4() == nil && (ip[0]&0xfe) == 0xfc {
		return false
	}
	return true
}

// allowURL applies the rules that are properties of the URL rather than of the
// peer: scheme, and the operator's host list when there is one.
func (f *fetcher) allowURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("%w: %s is not https", ErrFetch, u.Scheme)
	}
	if len(f.hosts) == 0 {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range f.hosts {
		// Suffix match on a dot boundary only, so "evil-example.com" cannot
		// pass a rule written for "example.com".
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not in FETCH_ALLOW_HOSTS", ErrFetch, host)
}

// fetchResult is what the caller is told. Note what is absent: the body.
type fetchResult struct {
	Rel         string
	Bytes       int64
	ContentType string
}

// Fetch downloads src into dst, which has already been resolved and checked by
// the vault. It returns without ever handing the body to its caller.
func (f *fetcher) Fetch(ctx context.Context, src string, dst string) (*fetchResult, error) {
	u, err := url.Parse(strings.TrimSpace(src))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot parse the URL", ErrFetch)
	}
	if err := f.allowURL(u); err != nil {
		return nil, err
	}

	want, ok := fetchableTypes[strings.ToLower(filepath.Ext(dst))]
	if !ok {
		return nil, fmt.Errorf("%w: %s is not a fetchable type", ErrFetch, filepath.Ext(dst))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	// A plausible UA, because a fair number of sites serve an error page to a
	// blank one and an error page is not an image — the sniff would reject it
	// and the operator would be debugging the wrong layer.
	req.Header.Set("User-Agent", "vault-mcp/"+version+" (+homelab attachment fetch)")
	req.Header.Set("Accept", "image/*,application/pdf")

	resp, err := f.client.Do(req)
	if err != nil {
		// The dial guard's refusals arrive here wrapped in url.Error. Unwrap so
		// "not a public address" reaches the operator instead of a generic
		// transport failure.
		if errors.Is(err, ErrFetch) {
			return nil, errors.Unwrap(err)
		}
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: the server returned %d", ErrFetch, resp.StatusCode)
	}
	// Cheap pre-check. The real limit is the LimitReader below, because
	// Content-Length is another claim by the same party.
	if resp.ContentLength > f.maxBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrFetch, resp.ContentLength, f.maxBytes)
	}

	// Temp-then-rename, in the destination directory so the rename is on one
	// filesystem and therefore atomic. *.tmp matches what snapshot.sh excludes,
	// so a torn download is never committed even when the destination is inside
	// a snapshotted tree.
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: cannot create %s", ErrFetch, dir)
	}
	tmp, err := os.CreateTemp(dir, ".fetch-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("%w: cannot write into %s", ErrFetch, dir)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename below has succeeded
	}()

	// Read the sniff window first, so the type is settled before the bulk of an
	// unwanted file is written to disk.
	head := make([]byte, 512)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	head = head[:n]

	got := http.DetectContentType(head)
	if i := strings.IndexByte(got, ';'); i >= 0 {
		got = strings.TrimSpace(got[:i])
	}
	if got != want {
		return nil, fmt.Errorf("%w: the bytes are %s, not %s — check the URL points at the file and not the page around it",
			ErrFetch, got, want)
	}

	written, err := io.Copy(tmp, io.MultiReader(
		strings.NewReader(string(head)),
		// +1 so a file exactly at the cap is distinguishable from one over it.
		io.LimitReader(resp.Body, f.maxBytes-int64(n)+1),
	))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	if written > f.maxBytes {
		return nil, fmt.Errorf("%w: larger than the %d byte limit", ErrFetch, f.maxBytes)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}

	// Re-check occupancy immediately before the rename, the same way Move does.
	// A dangling symlink counts as occupied: rename(2) would replace it without
	// a word. This is a narrow race either way, and losing it silently
	// overwrites a file, which is the one outcome this server never allows.
	if _, err := os.Lstat(dst); err == nil {
		return nil, fmt.Errorf("%w: something is already at that path", ErrFetch)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}

	return &fetchResult{Bytes: written, ContentType: got}, nil
}
