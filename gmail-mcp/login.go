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
// that only the operator can reach, and it never needs to run again unless
// Google revokes the token. The server process then only refreshes.
//
// The OAuth client is a Google "Desktop app" client for exactly this reason:
// Google allows any loopback port for those, and no public redirect URI is
// registered anywhere.
//
// The usual route is paste-back: the browser lands on the loopback redirect,
// which fails to load on the Mac, but its address carries the code, and
// pasting that address into this terminal finishes the login. The NAS's sshd
// refuses -L, so the route where an SSH port forward carries the redirect to
// the listener below is the exception, kept for a host that allows it.

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
// with exactly the Gmail scopes asked for. Google's consent screen lets you
// untick individual scopes, and an account picker makes the wrong mailbox one
// click away, so both are checked rather than assumed.
//
// Every refusal after the exchange revokes the grant: by then Google has
// issued it, and a grant nobody holds is still a live way into the mailbox.
func (l *login) finish(ctx context.Context, code string) error {
	t, err := l.oauth.exchange(ctx, code, l.verifier, l.redirectURI)
	if err != nil {
		return err
	}
	refuse := func(reason string) error {
		if rerr := l.oauth.revoke(ctx, firstNonEmpty(t.RefreshToken, t.AccessToken)); rerr != nil {
			return fmt.Errorf("%s. Revoking that grant failed (%v): remove gmail-mcp at myaccount.google.com/permissions yourself", reason, rerr)
		}
		return fmt.Errorf("%s; that grant has been revoked", reason)
	}
	email, verified, err := l.oauth.email(ctx, t.AccessToken)
	if err != nil {
		return refuse(fmt.Sprintf("could not check which account signed in (%v)", err))
	}
	if !verified || !strings.EqualFold(email, l.account) {
		return refuse(fmt.Sprintf("signed in as %q, but GMAIL_ACCOUNT is %q", email, l.account))
	}
	granted := map[string]bool{}
	for _, s := range strings.Fields(t.Scope) {
		granted[s] = true
	}
	for _, s := range googleScopes {
		if strings.HasPrefix(s, "https://") && !granted[s] {
			return refuse(fmt.Sprintf("consent did not include %s; run -login again and leave every box ticked", s))
		}
	}
	// Google can hand back more than was asked for when the client has
	// earlier grants (include_granted_scopes, or an old consent). The sealed
	// token should carry these scopes and no others.
	for s := range granted {
		if !allowedScope(s) {
			return refuse(fmt.Sprintf("Google granted %s, which this server never asks for; remove gmail-mcp at myaccount.google.com/permissions and run -login again", s))
		}
	}
	if t.RefreshToken == "" {
		return refuse("google returned no refresh token; remove gmail-mcp at myaccount.google.com/permissions and run -login again")
	}
	if err := l.store.Save(t.RefreshToken, t.Scope); err != nil {
		return refuse(fmt.Sprintf("could not save the token (%v)", err))
	}
	return nil
}

// allowedScope is googleScopes as Google writes them back: "email" comes
// back as its full URL.
func allowedScope(s string) bool {
	if s == "https://www.googleapis.com/auth/userinfo.email" {
		return true
	}
	for _, g := range googleScopes {
		if s == g {
			return true
		}
	}
	return false
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

	fmt.Fprintf(out, "Signing in %s.\n\nOpen this in a browser:\n\n  %s\n\n", cfg.account, l.authURL())
	fmt.Fprintf(out, "After consent the browser goes to %s and most likely shows a page that cannot load.\n", cfg.loginRedirect)
	fmt.Fprintln(out, "Paste that page's full address here. (With a port forward to this container, it finishes on its own.)")

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
