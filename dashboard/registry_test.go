package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseImageRef(t *testing.T) {
	cases := []struct {
		in                          string
		registry, repo, tag, digest string
	}{
		// What this repo actually runs.
		{"ghcr.io/ky1ejs/homelab/vault-mcp:latest", "ghcr.io", "ky1ejs/homelab/vault-mcp", "latest", ""},
		{"ghcr.io/ky1ejs/homelab/obsidian-vault:latest", "ghcr.io", "ky1ejs/homelab/obsidian-vault", "latest", ""},
		{
			"tailscale/tailscale:v1.102.2@sha256:321ce041508c19079b57a28b6666c8d81ab0b08accc0a2585b3ab663d557ac24",
			"docker.io", "tailscale/tailscale", "v1.102.2",
			"sha256:321ce041508c19079b57a28b6666c8d81ab0b08accc0a2585b3ab663d557ac24",
		},
		// The single-element case that a naive split gets wrong by reading
		// "alpine" as a registry host.
		{"alpine:3.22", "docker.io", "library/alpine", "3.22", ""},
		{"alpine", "docker.io", "library/alpine", "latest", ""},
		// A registry with a port: the colon must not be read as a tag separator.
		{"registry.local:5000/team/app:v2", "registry.local:5000", "team/app", "v2", ""},
		{"registry.local:5000/team/app", "registry.local:5000", "team/app", "latest", ""},
		{"localhost/app:dev", "localhost", "app", "dev", ""},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseImageRef(tc.in)
			if err != nil {
				t.Fatalf("parseImageRef(%q): %v", tc.in, err)
			}
			if got.Registry != tc.registry || got.Repo != tc.repo || got.Tag != tc.tag || got.Digest != tc.digest {
				t.Errorf("got %+v, want registry=%s repo=%s tag=%s digest=%s",
					got, tc.registry, tc.repo, tc.tag, tc.digest)
			}
		})
	}

	if _, err := parseImageRef("   "); err == nil {
		t.Error("an empty reference parsed")
	}
}

func TestApiHost(t *testing.T) {
	// Docker Hub is named docker.io in a reference but answers on another host.
	if h := (imageRef{Registry: "docker.io"}).apiHost(); h != "registry-1.docker.io" {
		t.Errorf("docker.io apiHost = %q", h)
	}
	if h := (imageRef{Registry: "ghcr.io"}).apiHost(); h != "ghcr.io" {
		t.Errorf("ghcr.io apiHost = %q", h)
	}
}

func TestParseChallenge(t *testing.T) {
	realm, service := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:o/r:pull"`)
	if realm != "https://ghcr.io/token" || service != "ghcr.io" {
		t.Fatalf("realm=%q service=%q", realm, service)
	}

	// A scope containing a comma inside quotes must not split the header.
	realm, _ = parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:a/b:pull,push"`)
	if realm != "https://auth.docker.io/token" {
		t.Fatalf("realm=%q with a quoted comma in scope", realm)
	}

	if r, _ := parseChallenge("Basic realm=\"x\""); r != "x" {
		// Not a case we act on, but it must not panic or hang.
		t.Logf("non-bearer challenge parsed to realm=%q", r)
	}
}

// The full negotiation: unauthorized, challenge, token, retry, digest.
func TestDigestNegotiation(t *testing.T) {
	const want = "sha256:deadbeef"
	var tokenCalls, manifestCalls int

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls++
			// The scope must be rebuilt from the reference, not echoed from the
			// challenge -- assert the server sees the repository we asked about.
			if got := r.URL.Query().Get("scope"); got != "repository:o/r:pull" {
				t.Errorf("scope = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"tok"}`))
		case "/v2/o/r/manifests/latest":
			manifestCalls++
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("Www-Authenticate",
					`Bearer realm="`+srv.URL+`/token",service="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "index.v1+json") {
				t.Errorf("Accept did not offer an index type: %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Docker-Content-Digest", want)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rc := newRegistryClient(time.Minute)
	rc.scheme = "http"

	ref, err := parseImageRef(strings.TrimPrefix(srv.URL, "http://") + "/o/r:latest")
	if err != nil {
		t.Fatal(err)
	}

	got, err := rc.Digest(context.Background(), ref)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}

	// Cached: a second look must not go back out to the registry.
	if _, err := rc.Digest(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 1 || manifestCalls != 2 {
		t.Errorf("token calls = %d (want 1), manifest calls = %d (want 2, i.e. the second lookup was cached)",
			tokenCalls, manifestCalls)
	}
}

// Failures are cached too, so a NAS with no upstream does not spend the HTTP
// timeout on every page load.
func TestFailuresAreCached(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	rc := newRegistryClient(time.Minute)
	rc.scheme = "http"
	ref, _ := parseImageRef(strings.TrimPrefix(srv.URL, "http://") + "/o/r:latest")

	for i := 0; i < 3; i++ {
		if _, err := rc.Digest(context.Background(), ref); err == nil {
			t.Fatal("expected an error")
		}
	}
	if calls != 1 {
		t.Errorf("registry was called %d times, want 1", calls)
	}
}

func TestUpdateFor(t *testing.T) {
	rc := newRegistryClient(time.Minute)

	t.Run("pinned images are never chased", func(t *testing.T) {
		u := rc.updateFor(context.Background(),
			"tailscale/tailscale:v1.102.2@sha256:321c", "sha256:321c")
		if u.State != UpdatePinned {
			t.Fatalf("state = %s, want pinned", u.State)
		}
	})

	t.Run("no running digest is unknown, not up to date", func(t *testing.T) {
		u := rc.updateFor(context.Background(), "ghcr.io/o/r:latest", "")
		if u.State != UpdateUnknown {
			t.Fatalf("state = %s, want unknown", u.State)
		}
		if u.Err == "" {
			t.Error("no explanation was given")
		}
	})

	t.Run("an unparseable reference is unknown", func(t *testing.T) {
		if u := rc.updateFor(context.Background(), "", "sha256:abc"); u.State != UpdateUnknown {
			t.Fatalf("state = %s, want unknown", u.State)
		}
	})
}

func TestUpdateForComparesDigests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:newer")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rc := newRegistryClient(time.Minute)
	rc.scheme = "http"
	image := strings.TrimPrefix(srv.URL, "http://") + "/o/r:latest"

	if u := rc.updateFor(context.Background(), image, "sha256:older"); u.State != UpdateAvailable {
		t.Errorf("state = %s (%s), want available", u.State, u.Err)
	}
	if u := rc.updateFor(context.Background(), image, "sha256:newer"); u.State != UpdateCurrent {
		t.Errorf("state = %s (%s), want current", u.State, u.Err)
	}
}

// A cancelled lookup says nothing about the registry, so it must not be cached.
//
// Caching it meant one reload during a page load pinned every badge to
// "unknown: context canceled" for a full TTL while GHCR was perfectly
// reachable -- and because a cache hit is instant, nothing ever retried.
// TestFailuresAreCached does not reach this: it only exercises a 500.
func TestCancellationIsNotCached(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Docker-Content-Digest", "sha256:real")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rc := newRegistryClient(time.Minute)
	rc.scheme = "http"
	ref, _ := parseImageRef(strings.TrimPrefix(srv.URL, "http://") + "/o/r:latest")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rc.Digest(cancelled, ref); err == nil {
		t.Fatal("a cancelled lookup returned no error")
	}

	// The very next attempt, with a live context, must reach the registry
	// rather than being served the cancellation.
	digest, err := rc.Digest(context.Background(), ref)
	if err != nil {
		t.Fatalf("the cancellation was cached and served to a later caller: %v", err)
	}
	if digest != "sha256:real" {
		t.Fatalf("digest = %q, want the real one", digest)
	}
}
