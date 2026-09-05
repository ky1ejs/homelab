package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"time"
)

// A one-pixel PNG and a minimal PDF, so the sniff has something real to agree
// or disagree with. Written out rather than fetched, obviously.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89,
}

var pdfBytes = []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")

func newTestFetcher(t *testing.T, hosts ...string) *fetcher {
	t.Helper()
	return newFetcher(10*time.Second, 1<<20, hosts)
}

// serve starts an httptest server and returns a fetcher wired to trust it.
//
// httptest listens on 127.0.0.1, which the dial guard refuses on purpose, so
// the test client swaps in a dialer that reaches the test server directly. That
// keeps every OTHER rule under test — scheme, host list, sniffing, size,
// overwrite — while not pretending loopback is routable. The guard itself is
// tested separately against routableIP, where it belongs.
func serve(t *testing.T, h http.Handler, hosts ...string) (*fetcher, string) {
	t.Helper()
	// A TLS server, because the scheme rule is real and https:// against a
	// plain one fails at the transport before any rule under test runs.
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "https://")
	f := newTestFetcher(t, hosts...)

	// srv.Client() already trusts the test certificate. Swap in a dialer that
	// sends every hostname to the test server, so the base URL below is a real
	// https fetch of a name the guard would otherwise have to resolve.
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	f.client = &http.Client{
		Timeout:       10 * time.Second,
		Transport:     tr,
		CheckRedirect: f.client.CheckRedirect,
	}
	// example.com, because that is what httptest's certificate is valid for.
	return f, "https://example.com"
}

func TestFetchSavesAnImage(t *testing.T) {
	f, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(pngBytes)
	}))
	dst := filepath.Join(t.TempDir(), "flies", "adams.png")

	res, err := f.Fetch(context.Background(), base+"/adams.png", dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png", res.ContentType)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("destination not written: %v", err)
	}
	if !bytes.Equal(got, pngBytes) {
		t.Errorf("file contents differ from what was served")
	}
	if res.Bytes != int64(len(pngBytes)) {
		t.Errorf("reported %d bytes, wrote %d", res.Bytes, len(pngBytes))
	}
}

// The whole point of the tool: a server claiming image/png while sending
// something else does not get to decide what lands in the vault.
func TestFetchRefusesWhenBytesDisagreeWithExtension(t *testing.T) {
	f, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("<!doctype html><html><body>not an image</body></html>"))
	}))
	dst := filepath.Join(t.TempDir(), "adams.png")

	_, err := f.Fetch(context.Background(), base+"/adams.png", dst)
	if err == nil {
		t.Fatal("expected a refusal for HTML served as a PNG")
	}
	if !errors.Is(err, ErrFetch) {
		t.Errorf("error = %v, want an ErrFetch", err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a refused fetch left a file behind")
	}
}

// A refused download must not leave its temp file either. Nothing else cleans
// these up, and they sit in a directory the sweeper reaps by folder age.
func TestFetchLeavesNoTempFileOnRefusal(t *testing.T) {
	f, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<!doctype html>this is not a png at all, not even close"))
	}))
	dir := t.TempDir()

	if _, err := f.Fetch(context.Background(), base+"/x.png", filepath.Join(dir, "x.png")); err == nil {
		t.Fatal("expected a refusal")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left behind: %s", e.Name())
	}
}

func TestFetchRefusesOversizedFile(t *testing.T) {
	big := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0}, 4096)...)
	f, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length, so the cheap pre-check cannot fire and the
		// LimitReader is what has to catch this.
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Write(big)
	}))
	f.maxBytes = 1024
	dst := filepath.Join(t.TempDir(), "big.png")

	if _, err := f.Fetch(context.Background(), base+"/big.png", dst); err == nil {
		t.Fatal("expected a refusal for a file over the cap")
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Error("an oversized fetch still landed")
	}
}

func TestFetchRefusesToOverwrite(t *testing.T) {
	f, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(pngBytes)
	}))
	dir := t.TempDir()
	dst := filepath.Join(dir, "taken.png")
	if err := os.WriteFile(dst, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Fetch(context.Background(), base+"/x.png", dst); err == nil {
		t.Fatal("expected a refusal rather than an overwrite")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "mine" {
		t.Errorf("the existing file was replaced: %q", got)
	}
}

// A dangling symlink is "occupied": rename(2) would replace it silently, which
// is how a fetch could be steered into writing through a link.
func TestFetchRefusesDanglingSymlinkDestination(t *testing.T) {
	f, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(pngBytes)
	}))
	dir := t.TempDir()
	dst := filepath.Join(dir, "link.png")
	if err := os.Symlink(filepath.Join(dir, "nowhere.png"), dst); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := f.Fetch(context.Background(), base+"/x.png", dst); err == nil {
		t.Fatal("expected a refusal for a symlink destination")
	}
}

func TestFetchRefusesNonFetchableExtension(t *testing.T) {
	f := newTestFetcher(t)
	for _, ext := range []string{".svg", ".canvas", ".base", ".md", ".sh", ".mp4", ".heic"} {
		dst := filepath.Join(t.TempDir(), "x"+ext)
		_, err := f.Fetch(context.Background(), "https://example.test/x"+ext, dst)
		if err == nil {
			t.Errorf("%s: expected a refusal", ext)
			continue
		}
		if !strings.Contains(err.Error(), "not a fetchable type") {
			t.Errorf("%s: error = %v, want a type refusal", ext, err)
		}
	}
}

func TestFetchRequiresHTTPS(t *testing.T) {
	f := newTestFetcher(t)
	for _, u := range []string{
		"http://example.test/x.png",
		"file:///etc/passwd",
		"ftp://example.test/x.png",
		"data:image/png;base64,AAAA",
	} {
		if _, err := f.Fetch(context.Background(), u, filepath.Join(t.TempDir(), "x.png")); err == nil {
			t.Errorf("%s: expected a refusal", u)
		}
	}
}

// The guard that keeps a fetch from becoming a probe of the home network. The
// QNAP admin interface, the other stacks and the dashboard's socket proxy are
// all reachable from the research container.
func TestRoutableIPRefusesEverythingLocal(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.5.5.5", "::1",
		"10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254",                        // cloud metadata
		"::ffff:127.0.0.1", "::ffff:192.168.1.1", // IPv4-mapped IPv6
		"100.64.0.1", "100.100.100.100", // CGNAT, and the tailnet
		"fc00::1", "fd00::1", // IPv6 unique-local
		"fe80::1",   // IPv6 link-local
		"224.0.0.1", // multicast
		"0.0.0.0", "::",
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test bug: %q is not an IP", s)
		}
		if routableIP(ip) {
			t.Errorf("%s was allowed and must not be", s)
		}
	}

	for _, s := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700::1111"} {
		ip := net.ParseIP(s)
		if !routableIP(ip) {
			t.Errorf("%s was refused and should be reachable", s)
		}
	}
}

// The dial guard is wired into the real client, not just available to it. This
// is the test that would fail if someone rebuilt the transport without Control.
func TestFetchRefusesLoopbackThroughTheRealDialer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(pngBytes)
	}))
	defer srv.Close()

	f := newTestFetcher(t)
	// The real client, real dialer. https:// against the plain test server
	// would fail at TLS anyway, so point at the port with the scheme the rule
	// demands and assert on WHICH refusal comes back.
	_, err := f.Fetch(context.Background(),
		"https://"+strings.TrimPrefix(srv.URL, "http://")+"/x.png",
		filepath.Join(t.TempDir(), "x.png"))
	if err == nil {
		t.Fatal("expected a refusal reaching loopback")
	}
	// Deliberately generic: the guard's own message names the resolved address,
	// and TestFetchRefusalDoesNotLeakTheResolvedAddress asserts that never
	// reaches the model. What matters here is that the DIAL GUARD is what
	// refused, not the type check or the scheme rule.
	if !strings.Contains(err.Error(), "not on the public internet") {
		t.Errorf("error = %v, want the dial guard's refusal", err)
	}
}

func TestAllowURLHostList(t *testing.T) {
	f := newTestFetcher(t, "example.com", "flies.org")

	allowed := []string{
		"https://example.com/a.png",
		"https://cdn.example.com/a.png",
		"https://deep.cdn.example.com/a.png",
		"https://FLIES.ORG/a.png", // case folded
	}
	for _, u := range allowed {
		parsed, _ := parseForTest(t, u)
		if err := f.allowURL(parsed); err != nil {
			t.Errorf("%s: refused, want allowed (%v)", u, err)
		}
	}

	// The suffix rule matches on a dot boundary, so a lookalike registered by
	// somebody else does not inherit the rule.
	refused := []string{
		"https://notexample.com/a.png",
		"https://example.com.evil.net/a.png",
		"https://evil.net/a.png?x=example.com",
	}
	for _, u := range refused {
		parsed, _ := parseForTest(t, u)
		if err := f.allowURL(parsed); err == nil {
			t.Errorf("%s: allowed, want refused", u)
		}
	}
}

func TestAllowURLEmptyListAllowsAnyPublicHost(t *testing.T) {
	f := newTestFetcher(t)
	parsed, _ := parseForTest(t, "https://anything.example/a.png")
	if err := f.allowURL(parsed); err != nil {
		t.Errorf("empty host list should allow any https host, got %v", err)
	}
}

// A redirect is re-checked, so an open redirector on an allowed host is not a
// way to reach a host that is not.
func TestFetchRechecksRedirectTargets(t *testing.T) {
	f, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "http://example.test/plain.png", http.StatusFound)
			return
		}
		w.Write(pngBytes)
	}))

	_, err := f.Fetch(context.Background(), base+"/hop", filepath.Join(t.TempDir(), "x.png"))
	if err == nil {
		t.Fatal("expected a refusal following a redirect to http")
	}
	if !strings.Contains(err.Error(), "not https") {
		t.Errorf("error = %v, want the scheme refusal on the redirect hop", err)
	}
}

func TestFetchRefusesNonOKStatus(t *testing.T) {
	f, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	if _, err := f.Fetch(context.Background(), base+"/x.png", filepath.Join(t.TempDir(), "x.png")); err == nil {
		t.Fatal("expected a refusal for a 404")
	}
}

func TestFetchSavesAPDF(t *testing.T) {
	f, base := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(pdfBytes)
	}))
	dst := filepath.Join(t.TempDir(), "hatch-chart.pdf")
	res, err := f.Fetch(context.Background(), base+"/x.pdf", dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.ContentType != "application/pdf" {
		t.Errorf("content type = %q, want application/pdf", res.ContentType)
	}
}

// MCP_FETCH is meaningless without -stdio and must not be quietly dropped: the
// operator who set it was trying to give an internet-facing endpoint a fetch.
func TestFetchConfigRejectedOnTheHTTPSurface(t *testing.T) {
	t.Setenv("MCP_FETCH", "1")
	t.Setenv("OAUTH_ISSUER", "")
	t.Setenv("MCP_ALLOW_NO_AUTH", "1")

	if _, err := loadConfig(false); err == nil {
		t.Fatal("MCP_FETCH=1 without -stdio should be fatal")
	}
	if _, err := loadConfig(true); err != nil {
		t.Fatalf("MCP_FETCH=1 with -stdio should load: %v", err)
	}
}

func TestFetchHostsParsedFromEnv(t *testing.T) {
	t.Setenv("MCP_FETCH", "1")
	t.Setenv("FETCH_ALLOW_HOSTS", " Example.COM , .flies.org ,, ")

	cfg, err := loadConfig(true)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"example.com", "flies.org"}
	if fmt.Sprint(cfg.fetchHosts) != fmt.Sprint(want) {
		t.Errorf("hosts = %v, want %v", cfg.fetchHosts, want)
	}
}

func TestFetchDefaultsAreUnrestrictedHosts(t *testing.T) {
	t.Setenv("MCP_FETCH", "1")
	cfg, err := loadConfig(true)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.fetchHosts) != 0 {
		t.Errorf("hosts = %v, want empty (any public host) by default", cfg.fetchHosts)
	}
	if cfg.fetchMaxBytes != 26214400 {
		t.Errorf("max bytes = %d, want 25 MiB", cfg.fetchMaxBytes)
	}
}

// The destination goes through the vault's path rules, so fetch reaches exactly
// what move_file reaches. If that wiring drifts, fetch becomes the way around
// the deny list writablePath exists to enforce.
//
// This calls the TOOL HANDLER, not vault.go, and the distinction is the whole
// point. An earlier version of this test called resolveRef and writablePath
// directly, which only re-tested vault_test.go's territory: deleting the
// writablePath call from fetchAttachment left the entire suite green and made
// ".claude/x.png" a legal fetch destination, because resolveRef alone does not
// refuse dotted components — that is writablePath's job alone.
//
// Verified the other way too: with the guard removed this test fails.
func TestFetchAttachmentRefusesDeniedDestinations(t *testing.T) {
	for _, bad := range []string{
		".claude/x.png",
		".claude/settings.json.png",
		".obsidian/x.png",
		"notes/.hidden/x.png",
		"../escape.png",
		"/absolute.png",
		// NOT "AGENTS.png": the deny is on AGENTS.md and CLAUDE.md exactly, and
		// an image called AGENTS.png is an ordinary attachment. Included here in
		// a first draft, which failed — correctly.
	} {
		t.Run(bad, func(t *testing.T) {
			s := newSurface(t, true)
			s.cfg.fetch = true
			// A fetcher whose dial guard refuses everything: if the handler ever
			// reaches the network for a denied path, that is itself the bug, and
			// no test should depend on an outbound connection to prove a path
			// rule.
			s.fetch = newFetcher(time.Second, 1<<20, nil)

			res, _, err := s.fetchAttachment(context.Background(), nil, fetchInput{
				URL:  "https://example.com/x.png",
				Path: bad,
			})
			if err != nil {
				t.Fatalf("%s: unexpected protocol error: %v", bad, err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("%s: accepted as a fetch destination and must not be", bad)
			}

			// Assert it was refused BY THE PATH RULES, not by anything else.
			// Accepting any error is how the previous version of this test
			// passed while the guard was deleted: the handler fell through to
			// the network, the fetch failed for an unrelated reason, and the
			// test read that as success.
			msg := ""
			if tc, ok := res.Content[0].(*mcp.TextContent); ok {
				msg = tc.Text
			}
			if !strings.Contains(msg, "Not allowed here") && !strings.Contains(msg, "outside the vault") {
				t.Errorf("%s: refused, but not by the path rules: %q", bad, msg)
			}

			// Nothing may land, whatever the failure mode.
			if _, statErr := os.Stat(filepath.Join(s.vault.root, filepath.FromSlash(bad))); statErr == nil {
				t.Errorf("%s: a file appeared at a denied path", bad)
			}
		})
	}
}

// The Read-deny globs in vault-research-settings.json are literal lowercase, so
// a fetched "adams.PNG" would be readable while every Go check case-folded and
// let it through. The equivalence is enforced on this side, where it is testable.
func TestFetchRefusesNonLowercaseExtension(t *testing.T) {
	f := newTestFetcher(t)
	for _, name := range []string{"x.PNG", "x.Png", "x.JPG", "x.PDF"} {
		_, err := f.Fetch(context.Background(), "https://example.com/"+name, filepath.Join(t.TempDir(), name))
		if err == nil {
			t.Errorf("%s: accepted, and the Read denies that keep it unreadable are lowercase", name)
			continue
		}
		if !strings.Contains(err.Error(), "lowercase") {
			t.Errorf("%s: error = %v, want the lowercase refusal", name, err)
		}
	}
	// The lowercase spelling still works, or the rule has broken the feature.
	if _, err := f.Fetch(context.Background(), "https://example.com/x.png", filepath.Join(t.TempDir(), "x.png")); err == nil {
		t.Error("expected a network refusal, not a type refusal")
	} else if strings.Contains(err.Error(), "lowercase") || strings.Contains(err.Error(), "fetchable type") {
		t.Errorf("lowercase .png was refused as a type: %v", err)
	}
}

// A refusal from the dial guard must not tell the model which address a name
// resolved to. That would make this an internal-address oracle on the surface
// whose promise is that a fetch says nothing back.
func TestFetchRefusalDoesNotLeakTheResolvedAddress(t *testing.T) {
	f := newTestFetcher(t)
	_, err := f.Fetch(context.Background(), "https://localhost/x.png", filepath.Join(t.TempDir(), "x.png"))
	if err == nil {
		t.Fatal("expected a refusal reaching loopback")
	}
	for _, leak := range []string{"127.0.0.1", "::1", "dial tcp"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the model-facing refusal contains %q: %v", leak, err)
		}
	}
}

func parseForTest(t *testing.T, raw string) (*url.URL, error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u, nil
}
