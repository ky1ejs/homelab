package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// `gmail-mcp -login`: the one-time Google consent that produces the refresh
// token.
//
// This is the reason the internet-facing server has no Google OAuth endpoints
// at all. The consent runs once, interactively, against a loopback redirect
// that only the operator can reach -- over an SSH port forward from the Mac --
// and it never needs to run again unless Google revokes the token. The server
// process then only refreshes.
//
// The OAuth client is a Google "Desktop app" client for exactly this reason:
// Google allows any loopback port for those, and no public redirect URI is
// registered anywhere.
//
// If the port forward is not set up, the browser lands on a page that fails
// to load. Its address still carries the code, and pasting that address into
// this terminal finishes the login the same way.

type login struct {
	oauth       *googleOAuth
	store       *tokenStore
	account     string
	redirectURI string

	state    string
	verifier string
	result   chan loginResult
}

type loginResult struct {
	code string
	err  error
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newLogin(oauth *googleOAuth, store *tokenStore, account, redirectURI string) *login {
	return &login{
		oauth: oauth, store: store, account: account, redirectURI: redirectURI,
		state: randomString(24), verifier: randomString(48),
		result: make(chan loginResult, 1),
	}
}

func (l *login) authURL() string {
	sum := sha256.Sum256([]byte(l.verifier))
	q := url.Values{
		"client_id":             {l.oauth.clientID},
		"redirect_uri":          {l.redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(googleScopes, " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"}, // always return a refresh token, even on a repeat login
		"login_hint":            {l.account},
		"state":                 {l.state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	return googleAuthURL + "?" + q.Encode()
}

// accept checks one redirect's query and delivers the code at most once.
func (l *login) accept(q url.Values) error {
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(l.state)) != 1 {
		return errors.New("state does not match this login; start again")
	}
	var r loginResult
	if e := q.Get("error"); e != "" {
		r.err = fmt.Errorf("google refused consent: %s", e)
	} else if q.Get("code") == "" {
		r.err = errors.New("redirect carried no code")
	} else {
		r.code = q.Get("code")
	}
	select {
	case l.result <- r:
	default: // already have one; ignore repeats
	}
	return r.err
}

func (l *login) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/callback" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := l.accept(r.URL.Query()); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "gmail-mcp login failed:", err)
		return
	}
	fmt.Fprintln(w, "gmail-mcp: got it. You can close this tab and go back to the terminal.")
}

// pasted accepts the full redirect address copied out of the browser.
func (l *login) pasted(line string) error {
	u, err := url.Parse(strings.TrimSpace(line))
	if err != nil || u.RawQuery == "" {
		return errors.New("that is not the redirect address; paste the whole URL from the address bar")
	}
	return l.accept(u.Query())
}

// finish exchanges the code and refuses anything but the configured account
// with both Gmail scopes granted. Google's consent screen lets you untick
// individual scopes, and an account picker makes the wrong mailbox one click
// away, so both are checked rather than assumed.
func (l *login) finish(ctx context.Context, code string) error {
	t, err := l.oauth.exchange(ctx, code, l.verifier, l.redirectURI)
	if err != nil {
		return err
	}
	email, verified, err := l.oauth.email(ctx, t.AccessToken)
	if err != nil {
		return err
	}
	if !verified || !strings.EqualFold(email, l.account) {
		l.oauth.revoke(ctx, firstNonEmpty(t.RefreshToken, t.AccessToken))
		return fmt.Errorf("signed in as %q, but GMAIL_ACCOUNT is %q; that grant has been revoked", email, l.account)
	}
	granted := map[string]bool{}
	for _, s := range strings.Fields(t.Scope) {
		granted[s] = true
	}
	for _, s := range googleScopes {
		if strings.HasPrefix(s, "https://") && !granted[s] {
			return fmt.Errorf("consent did not include %s; run -login again and leave every box ticked", s)
		}
	}
	if t.RefreshToken == "" {
		return errors.New("google returned no refresh token; remove the app at myaccount.google.com/permissions and run -login again")
	}
	return l.store.Save(t.RefreshToken, t.Scope)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func runLogin(cfg *config, oauth *googleOAuth, store *tokenStore, stdin io.Reader, out io.Writer) error {
	l := newLogin(oauth, store, cfg.account, cfg.loginRedirect)

	ln, err := net.Listen("tcp", cfg.loginAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.loginAddr, err)
	}
	srv := &http.Server{Handler: l, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	fmt.Fprintf(out, "Signing in %s.\n\nOpen this in a browser on the machine forwarding %s:\n\n  %s\n\n",
		cfg.account, cfg.loginRedirect, l.authURL())
	fmt.Fprintln(out, "Waiting for the redirect. If the browser shows a page that cannot load, paste its full address here.")

	go func() {
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "" {
				continue
			}
			if err := l.pasted(sc.Text()); err != nil {
				fmt.Fprintln(out, err)
				continue
			}
			return
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	select {
	case r := <-l.result:
		if r.err != nil {
			return r.err
		}
		if err := l.finish(ctx, r.code); err != nil {
			return err
		}
	case <-ctx.Done():
		return errors.New("timed out waiting for the Google redirect")
	}
	fmt.Fprintf(out, "\nSaved a sealed token for %s to %s.\n", cfg.account, store.path)
	return nil
}
