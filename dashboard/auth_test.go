package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadOnlyModeRefusesEverything(t *testing.T) {
	a := newAuthenticator("", time.Hour)

	if !a.ReadOnly() {
		t.Fatal("an empty DASH_TOKEN should mean read-only")
	}
	if err := a.Login("anything"); err == nil {
		t.Error("Login succeeded with no token configured")
	}
	// The empty string must not authenticate against the empty token.
	if err := a.Login(""); err == nil {
		t.Error("Login succeeded with an empty submission against an empty token")
	}
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.Header.Set(csrfHeader, "1")
	req.Header.Set("Authorization", "Bearer ")
	if a.Authorized(req) {
		t.Error("Authorized in read-only mode")
	}
}

func TestLogin(t *testing.T) {
	a := newAuthenticator("s3cret", time.Hour)

	if err := a.Login("s3cret"); err != nil {
		t.Errorf("the right token was rejected: %v", err)
	}
	// Whitespace around a token pasted from a password manager is not the
	// user's mistake to pay for.
	if err := a.Login("  s3cret\n"); err != nil {
		t.Errorf("a padded token was rejected: %v", err)
	}
	for _, wrong := range []string{"", "s3cre", "s3crett", "S3CRET"} {
		if err := a.Login(wrong); err == nil {
			t.Errorf("Login accepted %q", wrong)
		}
	}
}

func TestSessionCookieRoundTripAndExpiry(t *testing.T) {
	a := newAuthenticator("s3cret", time.Hour)
	now := time.Now()

	v := a.issue(now)
	if strings.Contains(v, "s3cret") {
		t.Fatal("the cookie contains the raw token")
	}
	if !a.valid(v, now) {
		t.Fatal("a freshly issued cookie is not valid")
	}
	if a.valid(v, now.Add(2*time.Hour)) {
		t.Error("an expired cookie is still valid")
	}
}

func TestForgedCookiesAreRejected(t *testing.T) {
	a := newAuthenticator("s3cret", time.Hour)
	now := time.Now()
	good := a.issue(now)
	exp, sig, _ := strings.Cut(good, ".")

	forged := []string{
		"",
		"nonsense",
		exp,       // no signature
		exp + ".", // empty signature
		exp + "." + strings.Repeat("0", len(sig)), // wrong signature
		"99999999999." + sig,                      // expiry moved, signature kept
		"notanumber." + sig,
	}
	for _, v := range forged {
		if a.valid(v, now) {
			t.Errorf("accepted forged cookie %q", v)
		}
	}

	// A cookie signed with a different token must not validate here -- that is
	// what makes rotating DASH_TOKEN actually log everyone out.
	other := newAuthenticator("different", time.Hour)
	if a.valid(other.issue(now), now) {
		t.Error("a cookie from another token validated")
	}
}

// The CSRF header is load-bearing, not decoration: without it a page on another
// origin could post a form that deploys a stack.
func TestAuthorizedRequiresTheCSRFHeader(t *testing.T) {
	a := newAuthenticator("s3cret", time.Hour)
	rec := httptest.NewRecorder()
	a.SetCookie(rec)
	cookie := rec.Result().Cookies()[0]

	withCookie := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/action", nil)
		r.AddCookie(cookie)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}

	if a.Authorized(withCookie(nil)) {
		t.Error("authorized on a cookie alone, with no CSRF header")
	}
	if !a.Authorized(withCookie(map[string]string{csrfHeader: "1"})) {
		t.Error("cookie plus CSRF header was refused")
	}

	// No cookie at all, header present: still no.
	bare := httptest.NewRequest(http.MethodPost, "/action", nil)
	bare.Header.Set(csrfHeader, "1")
	if a.Authorized(bare) {
		t.Error("authorized with a CSRF header and no session")
	}
}

// The bearer path is for curl and scripts, and skips the CSRF header because a
// request carrying an Authorization header is not something a form can produce.
func TestAuthorizedViaBearer(t *testing.T) {
	a := newAuthenticator("s3cret", time.Hour)

	r := httptest.NewRequest(http.MethodPost, "/action", nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	if !a.Authorized(r) {
		t.Error("a correct bearer token was refused")
	}

	for _, h := range []string{"Bearer wrong", "s3cret", "Basic s3cret", "Bearer "} {
		r := httptest.NewRequest(http.MethodPost, "/action", nil)
		r.Header.Set("Authorization", h)
		if a.Authorized(r) {
			t.Errorf("authorized with Authorization=%q", h)
		}
	}
}

func TestCookieAttributes(t *testing.T) {
	a := newAuthenticator("s3cret", time.Hour)
	rec := httptest.NewRecorder()
	a.SetCookie(rec)

	c := rec.Result().Cookies()[0]
	if !c.HttpOnly {
		t.Error("the session cookie is readable from JavaScript")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie is not SameSite=Strict")
	}
}
