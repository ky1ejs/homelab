package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func qpReader(r io.Reader) io.Reader { return quotedprintable.NewReader(r) }

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestParseKey(t *testing.T) {
	if _, err := parseKey(strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := parseKey(base64.StdEncoding.EncodeToString(testKey())); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "short", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := parseKey(bad); err == nil {
			t.Errorf("parseKey(%q) accepted", bad)
		}
	}
}

func TestTokenStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &tokenStore{path: filepath.Join(dir, "state", "google-token.json"), key: testKey(), account: "You@Example.com"}
	if _, err := s.Load(); !errors.Is(err, errNoToken) {
		t.Fatalf("empty store: %v", err)
	}
	if err := s.Save("refresh-123", "openid email https://www.googleapis.com/auth/gmail.send"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", fi.Mode(), err)
	}
	raw, _ := os.ReadFile(s.path)
	if strings.Contains(string(raw), "refresh-123") {
		t.Fatal("token stored in the clear")
	}
	got, err := s.Load()
	if err != nil || got != "refresh-123" {
		t.Fatalf("Load = %q, %v", got, err)
	}

	other := *s
	other.account = "someone@else.com"
	if _, err := other.Load(); !errors.Is(err, errTokenAccount) {
		t.Fatalf("other account: %v", err)
	}
	wrong := *s
	wrong.key = make([]byte, 32)
	if _, err := wrong.Load(); err == nil || !strings.Contains(err.Error(), "BRIEF_TOKEN_KEY") {
		t.Fatalf("wrong key: %v", err)
	}
	// Relabelling the account in the file does not move the token to it.
	var f tokenFile
	_ = json.Unmarshal(raw, &f)
	f.Account = "someone@else.com"
	b, _ := json.Marshal(f)
	_ = os.WriteFile(s.path, b, 0o600)
	if _, err := other.Load(); err == nil {
		t.Fatal("token opened under another account's name")
	}
}

// fakeGoogle is the token, userinfo, revoke and send endpoints.
type fakeGoogle struct {
	mu       sync.Mutex
	srv      *httptest.Server
	scope    string
	email    string
	verified bool
	refresh  string
	invalid  bool
	revoked  []string
	sent     [][]byte
	forms    []url.Values
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	g := &fakeGoogle{scope: "openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/gmail.send", email: "you@example.com", verified: true, refresh: "rt-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		g.mu.Lock()
		defer g.mu.Unlock()
		g.forms = append(g.forms, r.PostForm)
		if g.invalid {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(googleToken{AccessToken: "at-1", ExpiresIn: 3600, RefreshToken: g.refresh, Scope: g.scope})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"email": g.email, "email_verified": g.verified})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		g.mu.Lock()
		g.revoked = append(g.revoked, r.PostForm.Get("token"))
		g.mu.Unlock()
	})
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct{ Raw string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, err := base64.URLEncoding.DecodeString(body.Raw)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.sent = append(g.sent, raw)
		g.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"m-1"}`))
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// with changes the fake under its lock, since its handlers run on the
// server's goroutines.
func (g *fakeGoogle) with(f func(*fakeGoogle)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g)
}

func (g *fakeGoogle) oauth() *googleOAuth {
	o := newGoogleOAuth("cid", "secret", g.srv.Client())
	o.tokenURL, o.userinfoURL, o.revokeURL, o.sendURL = g.srv.URL+"/token", g.srv.URL+"/userinfo", g.srv.URL+"/revoke", g.srv.URL+"/send"
	return o
}

func TestGmailMailerSends(t *testing.T) {
	g := newFakeGoogle(t)
	store := &tokenStore{path: filepath.Join(t.TempDir(), "t.json"), key: testKey(), account: "you@example.com"}
	if err := store.Save("rt-1", ""); err != nil {
		t.Fatal(err)
	}
	m := &gmailMailer{oauth: g.oauth(), store: store}
	id, err := m.Send(context.Background(), []byte("Subject: hi\r\n\r\nbody"))
	if err != nil || id != "m-1" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	if string(g.sent[0]) != "Subject: hi\r\n\r\nbody" {
		t.Fatalf("sent %q", g.sent[0])
	}
	if f := g.forms[0]; f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "rt-1" {
		t.Fatalf("token form %v", f)
	}

	g.with(func(g *fakeGoogle) { g.invalid = true })
	if _, err := m.Send(context.Background(), []byte("x")); !errors.Is(err, errGoogleRevoked) {
		t.Fatalf("revoked token: %v", err)
	}
}
