package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGoogle stands in for Google's token, userinfo and revoke endpoints.
type fakeGoogle struct {
	*httptest.Server
	email    string
	verified bool
	scope    string
	refresh  string
	grant    string // "invalid_grant" makes the token endpoint refuse

	mu      sync.Mutex
	revoked []string
	forms   []url.Values
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	g := &fakeGoogle{
		email: "me@example.com", verified: true, refresh: "refresh-1",
		scope: strings.Join(googleScopes, " "),
	}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		g.mu.Lock()
		defer g.mu.Unlock()
		switch r.URL.Path {
		case "/token":
			g.forms = append(g.forms, r.PostForm)
			if g.grant != "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": g.grant})
				return
			}
			_ = json.NewEncoder(w).Encode(googleToken{AccessToken: "g-access", ExpiresIn: 3600, RefreshToken: g.refresh, Scope: g.scope})
		case "/userinfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": g.email, "email_verified": g.verified})
		case "/revoke":
			g.revoked = append(g.revoked, r.PostForm.Get("token"))
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGoogle) oauth() *googleOAuth {
	o := newGoogleOAuth("client-id", "client-secret", g.Client())
	o.tokenURL, o.userinfoURL, o.revokeURL = g.URL+"/token", g.URL+"/userinfo", g.URL+"/revoke"
	return o
}

func testLogin(t *testing.T, g *fakeGoogle) (*login, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "google-token.json")
	store := &tokenStore{path: path, key: testKey(t), account: "me@example.com"}
	return newLogin(g.oauth(), store, "me@example.com", "http://127.0.0.1:8765/callback"), path
}

func TestAuthURL(t *testing.T) {
	l, _ := testLogin(t, newFakeGoogle(t))
	u, err := url.Parse(l.authURL())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "accounts.google.com" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("access_type") != "offline" || q.Get("login_hint") != "me@example.com" || q.Get("state") != l.state {
		t.Errorf("auth url = %s", u)
	}
	if strings.Contains(u.String(), l.verifier) {
		t.Error("the PKCE verifier leaked into the auth URL")
	}
}

func TestAcceptChecksState(t *testing.T) {
	l, _ := testLogin(t, newFakeGoogle(t))
	if err := l.accept(url.Values{"state": {"forged"}, "code": {"c"}}); err == nil {
		t.Error("forged state accepted")
	}
	select {
	case <-l.result:
		t.Error("a forged redirect delivered a code")
	default:
	}
	if err := l.pasted("http://127.0.0.1:8765/callback?state=" + url.QueryEscape(l.state) + "&code=c1"); err != nil {
		t.Fatal(err)
	}
	if r := <-l.result; r.code != "c1" {
		t.Errorf("code = %q", r.code)
	}
}

func TestCallbackHandler(t *testing.T) {
	l, _ := testLogin(t, newFakeGoogle(t))
	rec := httptest.NewRecorder()
	l.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/callback?state="+url.QueryEscape(l.state)+"&error=access_denied", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
	if r := <-l.result; r.err == nil {
		t.Error("refused consent delivered no error")
	}
}

func TestFinishSavesTheConfiguredAccount(t *testing.T) {
	g := newFakeGoogle(t)
	l, path := testLogin(t, g)
	if err := l.finish(context.Background(), "code"); err != nil {
		t.Fatal(err)
	}
	if v := g.forms[0].Get("code_verifier"); v != l.verifier {
		t.Errorf("verifier sent = %q", v)
	}
	got, err := l.store.Load()
	if err != nil || got != "refresh-1" {
		t.Errorf("stored = %q, %v (%s)", got, err, path)
	}
}

func TestFinishRefuses(t *testing.T) {
	cases := map[string]func(*fakeGoogle){
		"other account":  func(g *fakeGoogle) { g.email = "someone@example.com" },
		"unverified":     func(g *fakeGoogle) { g.verified = false },
		"compose untick": func(g *fakeGoogle) { g.scope = "openid email https://www.googleapis.com/auth/gmail.readonly" },
		"no refresh":     func(g *fakeGoogle) { g.refresh = "" },
	}
	for name, change := range cases {
		g := newFakeGoogle(t)
		change(g)
		l, path := testLogin(t, g)
		if err := l.finish(context.Background(), "code"); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a token file was written", name)
		}
		if name == "other account" || name == "unverified" {
			if len(g.revoked) != 1 || g.revoked[0] != "refresh-1" {
				t.Errorf("%s: revoked = %v, want the new grant revoked", name, g.revoked)
			}
		}
	}
}

func TestAccessTokensRefreshAndRevoke(t *testing.T) {
	g := newFakeGoogle(t)
	path := filepath.Join(t.TempDir(), "google-token.json")
	store := &tokenStore{path: path, key: testKey(t), account: "me@example.com"}
	if err := store.Save("refresh-1", "scope"); err != nil {
		t.Fatal(err)
	}
	a := &accessTokens{oauth: g.oauth(), store: store}
	ctx := context.Background()

	for range 3 {
		if tok, err := a.Get(ctx); err != nil || tok != "g-access" {
			t.Fatalf("Get = %q, %v", tok, err)
		}
	}
	if len(g.forms) != 1 {
		t.Errorf("refreshed %d times, want 1 (cached)", len(g.forms))
	}

	a.expiry = time.Now() // force a refresh
	g.grant = "invalid_grant"
	if _, err := a.Get(ctx); !errors.Is(err, errGoogleRevoked) {
		t.Errorf("err = %v, want errGoogleRevoked", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a revoked token was left on disk")
	}
	if _, err := a.Get(ctx); !errors.Is(err, errNoToken) {
		t.Errorf("after revoke: err = %v, want errNoToken", err)
	}
}
