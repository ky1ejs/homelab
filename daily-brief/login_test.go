package main

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestLogin(t *testing.T) (*login, *fakeGoogle, *tokenStore) {
	g := newFakeGoogle(t)
	store := &tokenStore{path: filepath.Join(t.TempDir(), "google-token.json"), key: testKey(), account: "you@example.com"}
	return newLogin(g.oauth(), store, "you@example.com"), g, store
}

func TestAuthURL(t *testing.T) {
	l, _, _ := newTestLogin(t)
	u, err := url.Parse(l.authURL())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("scope") != "openid email https://www.googleapis.com/auth/gmail.send" {
		t.Fatalf("scope %q", q.Get("scope"))
	}
	for k, want := range map[string]string{
		"redirect_uri": loginRedirect, "access_type": "offline", "prompt": "consent",
		"login_hint": "you@example.com", "code_challenge_method": "S256", "state": l.state,
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Get("include_granted_scopes") != "" {
		t.Error("asks for earlier grants' scopes")
	}
}

func TestPasted(t *testing.T) {
	l, _, _ := newTestLogin(t)
	code, err := l.pasted(loginRedirect + "?state=" + l.state + "&code=abc")
	if err != nil || code != "abc" {
		t.Fatalf("pasted = %q, %v", code, err)
	}
	for _, bad := range []string{
		"not a url at all",
		loginRedirect + "?state=wrong&code=abc",
		loginRedirect + "?state=" + l.state + "&error=access_denied",
		loginRedirect + "?state=" + l.state,
	} {
		if _, err := l.pasted(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestFinishSavesToken(t *testing.T) {
	l, g, store := newTestLogin(t)
	if err := l.finish(context.Background(), "code"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(); err != nil || got != "rt-1" {
		t.Fatalf("stored %q, %v", got, err)
	}
	if f := g.forms[0]; f.Get("code_verifier") != l.verifier || f.Get("redirect_uri") != loginRedirect {
		t.Fatalf("exchange form %v", f)
	}
	if len(g.revoked) != 0 {
		t.Fatal("revoked a good grant")
	}
}

func TestFinishRefusesAndRevokes(t *testing.T) {
	cases := map[string]func(*fakeGoogle){
		"other account": func(g *fakeGoogle) { g.email = "someone@else.com" },
		"unverified":    func(g *fakeGoogle) { g.verified = false },
		"no send scope": func(g *fakeGoogle) { g.scope = "openid email" },
		"extra scope":   func(g *fakeGoogle) { g.scope += " https://www.googleapis.com/auth/gmail.readonly" },
		"no refresh":    func(g *fakeGoogle) { g.refresh = "" },
	}
	for name, mutate := range cases {
		l, g, store := newTestLogin(t)
		g.with(mutate)
		err := l.finish(context.Background(), "code")
		if err == nil || !strings.Contains(err.Error(), "revoked") {
			t.Errorf("%s: got %v", name, err)
		}
		if len(g.revoked) != 1 {
			t.Errorf("%s: %d revokes, want 1", name, len(g.revoked))
		}
		if _, err := store.Load(); err == nil {
			t.Errorf("%s: token saved", name)
		}
	}
}

// syncBuffer is a bytes.Buffer safe to write from runLogin's goroutines
// while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunLogin(t *testing.T) {
	g := newFakeGoogle(t)
	store := &tokenStore{path: filepath.Join(t.TempDir(), "google-token.json"), key: testKey(), account: "you@example.com"}
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	defer pw.Close()
	done := make(chan error, 1)
	go func() { done <- runLogin("you@example.com", g.oauth(), store, pr, out) }()

	// Read the state out of the printed URL, then paste a wrong address and
	// the right one.
	var state string
	for deadline := time.Now().Add(5 * time.Second); state == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, f := range strings.Fields(out.String()) {
			if u, err := url.Parse(f); err == nil && u.Query().Get("state") != "" {
				state = u.Query().Get("state")
			}
		}
	}
	if state == "" {
		t.Fatalf("no auth URL printed:\n%s", out.String())
	}
	if _, err := pw.Write([]byte("garbage\n" + loginRedirect + "?state=" + state + "&code=abc\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mustContain(t, out.String(), "that is not the redirect address")
	mustContain(t, out.String(), "Saved a sealed token")
	if got, err := store.Load(); err != nil || got != "rt-1" {
		t.Fatalf("stored %q, %v", got, err)
	}
}
