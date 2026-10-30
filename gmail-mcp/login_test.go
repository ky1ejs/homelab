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
	email        string
	verified     bool
	scope        string
	refresh      string
	grant        string // "invalid_grant" makes the token endpoint refuse
	revokeStatus int    // non-zero: the revoke endpoint answers with this

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
			if g.revokeStatus != 0 {
				w.WriteHeader(g.revokeStatus)
				return
			}
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

// Google writes "email" back as its full URL; that is the normal case.
func TestFinishAcceptsGoogleScopeSpelling(t *testing.T) {
	g := newFakeGoogle(t)
	g.scope = "https://www.googleapis.com/auth/gmail.compose openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/gmail.readonly"
	l, _ := testLogin(t, g)
	if err := l.finish(context.Background(), "code"); err != nil {
		t.Fatal(err)
	}
}

// Every refusal after the code exchange revokes the grant Google just issued,
// and writes nothing.
func TestFinishRefuses(t *testing.T) {
	cases := map[string]struct {
		change  func(*fakeGoogle, *login)
		revoked string
	}{
		"other account":  {func(g *fakeGoogle, _ *login) { g.email = "someone@example.com" }, "refresh-1"},
		"unverified":     {func(g *fakeGoogle, _ *login) { g.verified = false }, "refresh-1"},
		"compose untick": {func(g *fakeGoogle, _ *login) { g.scope = "openid email https://www.googleapis.com/auth/gmail.readonly" }, "refresh-1"},
		"extra scope": {func(g *fakeGoogle, _ *login) {
			g.scope = strings.Join(googleScopes, " ") + " https://www.googleapis.com/auth/drive"
		}, "refresh-1"},
		"no refresh": {func(g *fakeGoogle, _ *login) { g.refresh = "" }, "g-access"},
		"save fails": {func(_ *fakeGoogle, l *login) {
			// A file where the token's directory should be.
			blocker := filepath.Join(filepath.Dir(l.store.path), "blocker")
			_ = os.WriteFile(blocker, nil, 0o600)
			l.store.path = filepath.Join(blocker, "token.json")
		}, "refresh-1"},
	}
	for name, c := range cases {
		g := newFakeGoogle(t)
		l, path := testLogin(t, g)
		c.change(g, l)
		err := l.finish(context.Background(), "code")
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "has been revoked") {
			t.Errorf("%s: err = %v, want it to say the grant was revoked", name, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a token file was written", name)
		}
		if len(g.revoked) != 1 || g.revoked[0] != c.revoked {
			t.Errorf("%s: revoked = %v, want [%s]", name, g.revoked, c.revoked)
		}
	}
}

// A revoke Google refuses is reported, not assumed.
func TestFinishReportsFailedRevoke(t *testing.T) {
	g := newFakeGoogle(t)
	g.email, g.revokeStatus = "someone@example.com", http.StatusBadRequest
	l, _ := testLogin(t, g)
	err := l.finish(context.Background(), "code")
	if err == nil || strings.Contains(err.Error(), "has been revoked") || !strings.Contains(err.Error(), "myaccount.google.com/permissions") {
		t.Errorf("err = %v, want a failed revoke and where to remove the grant", err)
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

	// A 401 from Gmail drops the cache before expiry.
	a.Invalidate()
	if _, err := a.Get(ctx); err != nil || len(g.forms) != 2 {
		t.Errorf("after Invalidate: %v, %d refreshes, want 2", err, len(g.forms))
	}

	a.expiry = time.Now() // force a refresh
	g.grant = "invalid_grant"
	for range 3 {
		if _, err := a.Get(ctx); !errors.Is(err, errGoogleRevoked) {
			t.Errorf("err = %v, want errGoogleRevoked", err)
		}
	}
	if len(g.forms) != 3 {
		t.Errorf("a dead refresh token was tried %d times, want once", len(g.forms)-2)
	}
	// The file stays: invalid_grant is not always permanent, and a new
	// -login overwrites it.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the token file was removed: %v", err)
	}

	// A new -login writes a different refresh token, which is tried.
	g.grant = ""
	if err := store.Save("refresh-2", "scope"); err != nil {
		t.Fatal(err)
	}
	if tok, err := a.Get(ctx); err != nil || tok != "g-access" {
		t.Errorf("after a new login: %q, %v", tok, err)
	}
}
