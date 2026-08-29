// Who is allowed to press the buttons.
//
// This stack is reachable on the LAN and, through the NAS's own Tailscale node,
// from the tailnet -- but not from the internet. Neither of those paths carries
// an identity the dashboard can use: a LAN address proves nothing, and the host
// Tailscale node forwards to a published port without adding identity headers
// the way `tailscale serve` would. So there is a shared secret, and it gates
// exactly the actions that change something.
//
// Reading is not gated. Anyone already on the LAN can run `docker ps` on the NAS
// if they can reach it at all, so a login wall in front of the status page would
// buy nothing and would mean typing a token to answer "is vault-sync up".
// Deploying and restarting are a different matter, and those need the token.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "homelab_dash"
	// The header that makes a cross-site form post impossible. SameSite=Strict
	// already stops the cookie riding along, but a custom header cannot be set
	// by a plain form at all, so the two together leave no simple-request path.
	csrfHeader = "X-Homelab-Action"
)

type authenticator struct {
	// token is the shared secret from DASH_TOKEN. Empty means read-only mode:
	// the dashboard still shows everything, and every mutating action is
	// refused with an explanation rather than a 403 that looks like a bug.
	token string
	ttl   time.Duration
}

func newAuthenticator(token string, ttl time.Duration) *authenticator {
	return &authenticator{token: strings.TrimSpace(token), ttl: ttl}
}

func (a *authenticator) ReadOnly() bool { return a.token == "" }

// Login checks a submitted token in constant time.
func (a *authenticator) Login(submitted string) error {
	if a.ReadOnly() {
		return errors.New("this dashboard is running read-only: DASH_TOKEN is not set")
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(submitted)), []byte(a.token)) != 1 {
		return errors.New("that token is not right")
	}
	return nil
}

// issue mints a cookie value that expires on its own.
//
// The cookie carries a signature, not the token: an HttpOnly cookie should not
// be readable off a backed-up browser profile and replayable as the header form
// of the same credential. It is still a bearer credential, but a time-limited
// one that cannot be pasted into `curl -H Authorization`.
func (a *authenticator) issue(now time.Time) string {
	exp := now.Add(a.ttl).Unix()
	return fmt.Sprintf("%d.%s", exp, a.sign(exp))
}

func (a *authenticator) sign(exp int64) string {
	mac := hmac.New(sha256.New, []byte(a.token))
	fmt.Fprintf(mac, "homelab-dash-session:%d", exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// valid reports whether a cookie value is one we issued and has not expired.
func (a *authenticator) valid(value string, now time.Time) bool {
	if a.ReadOnly() {
		return false
	}
	expStr, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	if now.Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(a.sign(exp)))
}

// Authorized reports whether this request may change something.
//
// Two ways in, and both are required to be deliberate:
//
//   - the session cookie, plus the CSRF header, for the browser;
//   - an Authorization: Bearer header, for curl and for anything scripted.
//
// The bearer path skips the CSRF header check because a request carrying an
// Authorization header is already not something a cross-site form can produce.
func (a *authenticator) Authorized(r *http.Request) bool {
	if a.ReadOnly() {
		return false
	}

	// Require the scheme rather than trimming it: see the same note in agent.go.
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && bearer != "" {
		return subtle.ConstantTimeCompare([]byte(bearer), []byte(a.token)) == 1
	}

	if r.Header.Get(csrfHeader) == "" {
		return false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return a.valid(c.Value, time.Now())
}

// SetCookie writes the session. Secure is deliberately NOT set: this stack is
// served over plain HTTP on the LAN and over the tailnet, and a Secure cookie
// would simply never be stored, turning login into a silent no-op. The traffic
// it protects never leaves the house or the tailnet -- see
// README.md#trust-boundary for why that is judged acceptable here and would not
// be for vault-mcp.
func (a *authenticator) SetCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.issue(time.Now()),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(a.ttl.Seconds()),
	})
}

func (a *authenticator) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// SignedIn is the read-only question the template asks, so the page can show
// either the login box or the action buttons.
func (a *authenticator) SignedIn(r *http.Request) bool {
	if a.ReadOnly() {
		return false
	}
	c, err := r.Cookie(sessionCookie)
	return err == nil && a.valid(c.Value, time.Now())
}
