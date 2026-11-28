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
	"net/url"
	"strings"
	"time"
)

// `daily-brief gmail-login`: the one-time Google consent that produces the
// refresh token, run by hand with `docker compose run`.
//
// Paste-back only. The redirect goes to a loopback address that nothing
// listens on; the browser shows a page that cannot load, and its address
// carries the code. Pasting that address back finishes the login. The OAuth
// client is a Google "Desktop app" client, which accepts any loopback
// redirect, so no redirect is registered anywhere and this container never
// listens on a port.

const loginRedirect = "http://127.0.0.1:8766/callback"

type login struct {
	oauth    *googleOAuth
	store    *tokenStore
	account  string
	state    string
	verifier string
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newLogin(oauth *googleOAuth, store *tokenStore, account string) *login {
	return &login{oauth: oauth, store: store, account: account, state: randomString(24), verifier: randomString(48)}
}

func (l *login) authURL() string {
	sum := sha256.Sum256([]byte(l.verifier))
	q := url.Values{
		"client_id":             {l.oauth.clientID},
		"redirect_uri":          {loginRedirect},
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

// pasted reads the code out of the redirect address copied from the browser.
func (l *login) pasted(line string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(line))
	if err != nil || u.RawQuery == "" {
		return "", errors.New("that is not the redirect address; paste the whole URL from the address bar")
	}
	q := u.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(l.state)) != 1 {
		return "", errors.New("that address is from another login; use the one this login opened")
	}
	if e := q.Get("error"); e != "" {
		return "", fmt.Errorf("google refused consent: %s", truncate(e, 60))
	}
	if q.Get("code") == "" {
		return "", errors.New("the address carries no code")
	}
	return q.Get("code"), nil
}

// finish exchanges the code and refuses anything but the configured account
// with exactly the scopes asked for. Google's consent screen lets you untick a
// scope, and an account picker makes the wrong mailbox one click away, so both
// are checked rather than assumed.
//
// Every refusal after the exchange revokes the grant: by then Google has
// issued it, and a grant nobody holds is still a live way to send as that
// mailbox. This is safe only because this stack has a Google Cloud project of
// its own; see gmail.go.
func (l *login) finish(ctx context.Context, code string) error {
	t, err := l.oauth.exchange(ctx, code, l.verifier, loginRedirect)
	if err != nil {
		return err
	}
	refuse := func(reason string) error {
		if rerr := l.oauth.revoke(ctx, firstNonEmpty(t.RefreshToken, t.AccessToken)); rerr != nil {
			return fmt.Errorf("%s. Revoking that grant failed (%v): remove it at myaccount.google.com/connections yourself", reason, rerr)
		}
		return fmt.Errorf("%s; that grant has been revoked", reason)
	}
	email, verified, err := l.oauth.email(ctx, t.AccessToken)
	if err != nil {
		return refuse(fmt.Sprintf("could not check which account signed in (%v)", err))
	}
	if !verified || !strings.EqualFold(email, l.account) {
		return refuse(fmt.Sprintf("signed in as %q, but GMAIL_ACCOUNT is %q", truncate(email, 80), l.account))
	}
	granted := map[string]bool{}
	for _, s := range strings.Fields(t.Scope) {
		granted[s] = true
	}
	if !granted["https://www.googleapis.com/auth/gmail.send"] {
		return refuse("consent did not include sending mail; run gmail-login again and leave the box ticked")
	}
	for s := range granted {
		if !allowedScope(s) {
			return refuse(fmt.Sprintf("Google granted %s, which this stack never asks for; remove the app at myaccount.google.com/connections and run gmail-login again", truncate(s, 80)))
		}
	}
	if t.RefreshToken == "" {
		return refuse("google returned no refresh token; remove the app at myaccount.google.com/connections and run gmail-login again")
	}
	if err := l.store.Save(t.RefreshToken, t.Scope); err != nil {
		return refuse(fmt.Sprintf("could not save the token (%v)", err))
	}
	return nil
}

// allowedScope is googleScopes as Google writes them back: "email" can come
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

func runLogin(account string, oauth *googleOAuth, store *tokenStore, stdin io.Reader, out io.Writer) error {
	l := newLogin(oauth, store, account)
	fmt.Fprintf(out, "Signing in %s to send the daily brief.\n\nOpen this in a browser:\n\n  %s\n\n", account, l.authURL())
	fmt.Fprintf(out, "After consent the browser goes to %s and shows a page that cannot load.\n", loginRedirect)
	fmt.Fprintln(out, "Paste that page's full address here:")

	codes := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 0, 8<<10), 64<<10)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "" {
				continue
			}
			code, err := l.pasted(sc.Text())
			if err != nil {
				fmt.Fprintln(out, err)
				continue
			}
			codes <- code
			return
		}
		close(codes)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	select {
	case code, ok := <-codes:
		if !ok {
			return errors.New("input ended before a redirect address was pasted")
		}
		if err := l.finish(ctx, code); err != nil {
			return err
		}
	case <-ctx.Done():
		return errors.New("timed out waiting for the redirect address")
	}
	fmt.Fprintf(out, "\nSaved a sealed token for %s to %s.\n", account, store.path)
	return nil
}
