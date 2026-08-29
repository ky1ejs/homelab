// "Is there a newer image than the one we are running?"
//
// Answered by comparing two digests: the one the running container was pulled
// at (RepoDigests, from the Docker API) and the one the tag resolves to now (a
// manifest HEAD against the registry). Digests rather than tags or labels
// because `:latest` is not a version -- the whole point of the question is that
// the tag has not changed and the thing behind it has.
//
// This lives in the WEB role, not the agent. The check is an outbound HTTPS
// call to a third party, and there is no reason to make it from the process
// holding the Docker socket.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// imageRef is a parsed image reference.
type imageRef struct {
	Registry string // ghcr.io
	Repo     string // ky1ejs/homelab/vault-mcp
	Tag      string // latest
	Digest   string // sha256:... when the reference pins one
}

// Pinned references name an exact image, so there is nothing to check: the
// answer is always "you are running what you asked for". The Tailscale sidecar
// is pinned this way on purpose, and reporting it as "up to date" would be a
// lie -- upstream may well have moved. "pinned" says the true thing.
func (r imageRef) Pinned() bool { return r.Digest != "" }

// parseImageRef handles the forms this repo actually uses, and degrades rather
// than guessing on anything else.
//
//	ghcr.io/ky1ejs/homelab/vault-mcp:latest
//	tailscale/tailscale:v1.102.2@sha256:321c...
//	alpine/git:v2.54.0@sha256:3b44...
func parseImageRef(s string) (imageRef, error) {
	var r imageRef
	s = strings.TrimSpace(s)
	if s == "" {
		return r, fmt.Errorf("empty image reference")
	}

	if name, digest, ok := strings.Cut(s, "@"); ok {
		r.Digest = digest
		s = name
	}

	// The registry is the first path element only if it looks like a host: it
	// contains a dot or a colon, or it is literally "localhost". Without that
	// rule "tailscale/tailscale" parses as registry "tailscale".
	rest := s
	first, remainder, hasSlash := strings.Cut(s, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry = first
		rest = remainder
	} else {
		r.Registry = "docker.io"
	}

	// Split the tag off the last path element, so a port in the registry host
	// cannot be mistaken for one.
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		r.Repo, r.Tag = rest[:i], rest[i+1:]
	} else {
		r.Repo, r.Tag = rest, "latest"
	}

	if r.Repo == "" {
		return r, fmt.Errorf("no repository in %q", s)
	}
	// Docker Hub's official images live under library/.
	if r.Registry == "docker.io" && !strings.Contains(r.Repo, "/") {
		r.Repo = "library/" + r.Repo
	}
	return r, nil
}

// apiHost is where the v2 API lives, which is not the name in the reference for
// Docker Hub.
func (r imageRef) apiHost() string {
	if r.Registry == "docker.io" {
		return "registry-1.docker.io"
	}
	return r.Registry
}

// --- lookups ----------------------------------------------------------------

// Accept every manifest media type, including the index forms. A multi-arch
// image answers with an index, and asking only for the v2 manifest gets either
// a 404 or -- worse -- a per-architecture digest that will never equal the one
// the daemon recorded.
const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

type registryClient struct {
	http *http.Client
	ttl  time.Duration
	// scheme is "https" everywhere except in the tests, which point it at an
	// httptest server. It is a field rather than a constant only so the digest
	// negotiation -- challenge, token, retry -- can be exercised for real
	// instead of mocked out at a higher level.
	scheme string

	mu    sync.Mutex
	cache map[string]cachedDigest
}

type cachedDigest struct {
	digest string
	err    error
	at     time.Time
}

func newRegistryClient(ttl time.Duration) *registryClient {
	return &registryClient{
		http:   &http.Client{Timeout: 15 * time.Second},
		ttl:    ttl,
		scheme: "https",
		cache:  map[string]cachedDigest{},
	}
}

// Digest resolves a tag to the digest it currently points at.
//
// Cached, because every browser tab refresh would otherwise be a round trip to
// GHCR per stack, and the answer changes when CI publishes -- minutes-scale, not
// seconds-scale. Failures are cached too, with the same TTL: a NAS whose
// upstream is down should not spend fifteen seconds of every page load
// rediscovering that.
func (rc *registryClient) Digest(ctx context.Context, ref imageRef) (string, error) {
	key := ref.apiHost() + "/" + ref.Repo + ":" + ref.Tag

	rc.mu.Lock()
	if c, ok := rc.cache[key]; ok && time.Since(c.at) < rc.ttl {
		rc.mu.Unlock()
		return c.digest, c.err
	}
	rc.mu.Unlock()

	digest, err := rc.fetchDigest(ctx, ref)

	rc.mu.Lock()
	rc.cache[key] = cachedDigest{digest: digest, err: err, at: time.Now()}
	rc.mu.Unlock()

	return digest, err
}

func (rc *registryClient) fetchDigest(ctx context.Context, ref imageRef) (string, error) {
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", rc.scheme, ref.apiHost(), ref.Repo, ref.Tag)

	resp, err := rc.manifestHead(ctx, url, "")
	if err != nil {
		return "", err
	}
	// Anonymous pull of a public image still needs a bearer token; the registry
	// says where to get one. Both registries this repo pulls from are public, so
	// no credential is involved -- see the root README on why the images are
	// public in the first place.
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()

		token, err := rc.token(ctx, challenge, ref)
		if err != nil {
			return "", err
		}
		if resp, err = rc.manifestHead(ctx, url, token); err != nil {
			return "", err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", fmt.Errorf("%s: no Docker-Content-Digest header", url)
	}
	return d, nil
}

func (rc *registryClient) manifestHead(ctx context.Context, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return rc.http.Do(req)
}

func (rc *registryClient) token(ctx context.Context, challenge string, ref imageRef) (string, error) {
	realm, service := parseChallenge(challenge)
	if realm == "" {
		return "", fmt.Errorf("registry did not say where to get a token: %q", challenge)
	}

	url := fmt.Sprintf("%s?scope=repository:%s:pull", realm, ref.Repo)
	if service != "" {
		url += "&service=" + service
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := rc.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint: %s", resp.Status)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if body.Token != "" {
		return body.Token, nil
	}
	// GHCR answers with access_token; Docker Hub with token. Accept either.
	return body.AccessToken, nil
}

// parseChallenge pulls realm and service out of a WWW-Authenticate header:
//
//	Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="..."
//
// The scope is deliberately ignored and rebuilt from the reference: taking the
// registry's word for which repository we are asking about would let a
// redirect widen the request.
func parseChallenge(h string) (realm, service string) {
	h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "Bearer"))
	for _, part := range splitParams(h) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.TrimSpace(k) {
		case "realm":
			realm = v
		case "service":
			service = v
		}
	}
	return realm, service
}

// splitParams splits on commas that are not inside quotes.
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// --- the comparison ---------------------------------------------------------

// updateFor decides what to say about one stack.
//
// runningDigest is the digest of the image the container is ACTUALLY on, taken
// from the container rather than from .env: a stack whose .env was edited but
// never deployed is exactly the drift worth showing, and reading the intended
// image twice would hide it.
func (rc *registryClient) updateFor(ctx context.Context, image, runningDigest string) *UpdateStatus {
	u := &UpdateStatus{State: UpdateUnknown, Running: runningDigest, CheckedAt: time.Now().UTC()}

	ref, err := parseImageRef(image)
	if err != nil {
		u.Err = err.Error()
		return u
	}
	if ref.Pinned() {
		u.State = UpdatePinned
		u.Latest = ref.Digest
		return u
	}
	if runningDigest == "" {
		u.Err = "the running image has no registry digest (built locally, or never pulled)"
		return u
	}

	latest, err := rc.Digest(ctx, ref)
	if err != nil {
		u.Err = err.Error()
		return u
	}
	u.Latest = latest

	if latest == runningDigest {
		u.State = UpdateCurrent
	} else {
		u.State = UpdateAvailable
	}
	return u
}
