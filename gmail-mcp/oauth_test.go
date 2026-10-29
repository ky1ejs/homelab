package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testResource = "https://gmail-mcp.example.ts.net/mcp"

// testVerifier reaches the stand-in through the production egress client,
// allowing only the stand-in's host, over the stand-in's TLS.
func testVerifier(as *fakeAS) *oauthVerifier {
	u, _ := url.Parse(as.URL)
	client := egressClient(map[string]bool{u.Hostname(): true}, as.Client().Transport.(*http.Transport))
	return newOAuthVerifier(as.URL, testResource, []string{"user_kyle"}, []string{testClient}, client, quietLogger())
}

func TestVerify(t *testing.T) {
	as := newFakeAS(t)
	o := testVerifier(as)
	ctx := context.Background()
	hour := time.Now().Add(time.Hour)

	if sub, err := o.verify(ctx, as.mint(t, "user_kyle", testResource, hour)); err != nil || sub != "user_kyle" {
		t.Fatalf("good token: %q, %v", sub, err)
	}
	cases := map[string]struct {
		token string
		want  error
	}{
		// The shared-tenant case: a token minted for the vault must not open mail.
		"vault audience": {as.mint(t, "user_kyle", "https://vault-mcp.example.ts.net/mcp", hour), errNoAudience},
		"other subject":  {as.mint(t, "user_someone", testResource, hour), errSubjectNotAllowed},
		"expired":        {as.mint(t, "user_kyle", testResource, time.Now().Add(-time.Minute)), nil},
		"not a jwt":      {"abc.def.ghi", nil},
		"another issuer": {newFakeAS(t).mint(t, "user_kyle", testResource, hour), nil},
		// Any client registered on the tenant can get a token for this
		// resource; only the claude.ai connector's should open mail.
		"other client": {as.mintClaims(t, map[string]any{"client_id": "client_someone_else"}), errClientNotAllowed},
		"no client":    {as.mintClaims(t, map[string]any{"client_id": nil}), errClientNotAllowed},
		"no audience":  {as.mintClaims(t, map[string]any{"aud": nil}), errNoAudience},
	}
	// RFC 7519 allows aud as a bare string; that must still pass.
	if _, err := o.verify(ctx, as.mintClaims(t, map[string]any{"aud": testResource})); err != nil {
		t.Errorf("string audience: %v", err)
	}
	for name, c := range cases {
		_, err := o.verify(ctx, c.token)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

// A token that could never pass costs no request to the authorization server.
func TestVerifyRefusesBeforeFetching(t *testing.T) {
	as := newFakeAS(t)
	o := testVerifier(as)
	for _, tok := range []string{
		as.mintClaims(t, map[string]any{"sub": "user_someone"}),
		as.mintClaims(t, map[string]any{"aud": []string{"https://vault-mcp.example.ts.net/mcp"}}),
		as.mintClaims(t, map[string]any{"iss": "https://elsewhere.example.com"}),
		"abc.def.ghi",
	} {
		if _, err := o.verify(context.Background(), tok); err == nil {
			t.Error("accepted")
		}
	}
	if n := as.hits.Load(); n != 0 {
		t.Errorf("authorization server hit %d times", n)
	}
}

// A failed discovery is remembered briefly, so a burst of requests while the
// tenant is down does not become a burst of fetches.
func TestDiscoveryFailureIsCached(t *testing.T) {
	as := newFakeAS(t)
	as.down.Store(true)
	o := testVerifier(as)
	tok := as.mint(t, "user_kyle", testResource, time.Now().Add(time.Hour))
	for range 3 {
		if _, err := o.verify(context.Background(), tok); err == nil {
			t.Fatal("accepted with discovery down")
		}
	}
	if n := as.hits.Load(); n != 1 {
		t.Errorf("discovery fetched %d times, want 1", n)
	}
}

// The key URL is whatever discovery says, so it is the egress list, not the
// discovery document, that decides where the verifier may go.
func TestJWKSOffTheAllowListRefused(t *testing.T) {
	as := newFakeAS(t)
	as.jwksURI = strings.Replace(as.URL, "127.0.0.1", "localhost", 1) + "/jwks"
	o := testVerifier(as)
	_, err := o.verify(context.Background(), as.mint(t, "user_kyle", testResource, time.Now().Add(time.Hour)))
	if err == nil || !strings.Contains(err.Error(), errEgress.Error()) {
		t.Errorf("err = %v, want an egress refusal", err)
	}
}

func testServer(t *testing.T, as *fakeAS, g *fakeGmail) *httptest.Server {
	t.Helper()
	s := &server{oauth: testVerifier(as), tools: &tools{gmail: g.client(), log: quietLogger()}, log: quietLogger()}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestUnauthenticatedGetsChallenge(t *testing.T) {
	as := newFakeAS(t)
	srv := testServer(t, as, newFakeGmail(t))
	for _, auth := range []string{"", "Bearer nonsense", "Basic dXNlcjpwYXNz"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d", auth, resp.StatusCode)
		}
		want := `Bearer resource_metadata="` + testResource + resourceMetadataPath + `"`
		if got := resp.Header.Get("WWW-Authenticate"); got != want {
			t.Errorf("auth %q: challenge %q, want %q", auth, got, want)
		}
	}
}

func TestResourceMetadata(t *testing.T) {
	as := newFakeAS(t)
	srv := testServer(t, as, newFakeGmail(t))
	for _, path := range []string{resourceMetadataPath, resourceMetadataPath + "/mcp"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
		if m.Resource != testResource || len(m.Servers) != 1 || m.Servers[0] != as.URL {
			t.Errorf("%s: %+v", path, m)
		}
	}
}
