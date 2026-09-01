// The other half of the trust boundary, and the half that changed shape.
//
// Under DASH_TOKEN these tests asserted things about a shared secret. There is
// no secret any more: identity is a header that `tailscale serve` adds, so what
// has to be asserted is that the header is honoured only under the conditions
// that make it mean anything, and that the permissive settings are impossible to
// arrive at by leaving something blank.
//
// The other half of that premise -- that nothing but Serve can reach the
// listener to set the header itself -- is asserted in agent_test.go, because it
// is the agent that checks it. Neither file is sufficient alone.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustAuth(t *testing.T, allowed string, anyLogin, readOnly bool) *authenticator {
	t.Helper()
	a, err := newAuthenticator(allowed, anyLogin, readOnly)
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}
	return a
}

// req builds a request as `tailscale serve` would deliver it.
func req(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/action", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func signedIn(login string) map[string]string {
	return map[string]string{identityHeader: login, csrfHeader: "1"}
}

// THE ONE THAT MATTERS MOST. An empty allow list must be a startup failure, not
// "everybody". vault-mcp shipped an open vault on a public hostname because an
// empty value was the permissive one, and this is the same variable shape
// adopted so that cannot happen twice.
func TestBlankAllowListRefusesToStart(t *testing.T) {
	if _, err := newAuthenticator("", false, false); err == nil {
		t.Fatal("newAuthenticator accepted an empty DASH_ALLOWED_LOGINS")
	}
	if _, err := newAuthenticator("   ,  , ", false, false); err == nil {
		t.Fatal("newAuthenticator accepted an allow list of only separators and whitespace")
	}

	// The two ways to mean it are both explicit.
	if _, err := newAuthenticator("", true, false); err != nil {
		t.Errorf("DASH_ALLOW_ANY_TAILNET_USER was refused: %v", err)
	}
	if _, err := newAuthenticator("", false, true); err != nil {
		t.Errorf("DASH_READ_ONLY was refused: %v", err)
	}
}

func TestOnlyAllowedLoginsMayAct(t *testing.T) {
	a := mustAuth(t, "kyle@example.com, someone@example.com", false, false)

	if login, err := a.Authorize(req(signedIn("kyle@example.com"))); err != nil || login != "kyle@example.com" {
		t.Fatalf("Authorize(allowed) = %q, %v", login, err)
	}
	if _, err := a.Authorize(req(signedIn("stranger@example.com"))); err == nil {
		t.Fatal("a login that is not on the allow list was authorized")
	}
	// ...but the page still knows who they are, so it can say so.
	if login, err := a.Identify(req(signedIn("stranger@example.com"))); err != nil || login != "stranger@example.com" {
		t.Fatalf("Identify hid a login that is merely not allowed: %q, %v", login, err)
	}
}

// Tailnet logins are email-shaped, and a capital letter in .env should be a
// typo rather than a lockout.
func TestLoginComparisonIsCaseFolded(t *testing.T) {
	a := mustAuth(t, "Kyle@Example.com", false, false)
	if _, err := a.Authorize(req(signedIn("kyle@example.com"))); err != nil {
		t.Fatalf("a case-different login was refused: %v", err)
	}
}

// A request with no identity header did not come through `tailscale serve`, or
// came from a tagged node, which has no user. Either way there is nobody to
// authorize.
func TestMissingIdentityIsRefused(t *testing.T) {
	a := mustAuth(t, "kyle@example.com", false, false)

	for _, h := range []map[string]string{
		{csrfHeader: "1"},
		{identityHeader: "", csrfHeader: "1"},
		{identityHeader: "   ", csrfHeader: "1"},
	} {
		if _, err := a.Authorize(req(h)); err == nil {
			t.Errorf("Authorize accepted a request with no usable identity: %v", h)
		}
	}
}

// Funnel traffic reaches the listener by the same loopback path Serve does and
// carries NO identity headers, so it would otherwise arrive looking merely
// unidentified. Refusing it by name is what keeps "this stack is not on a
// Funnel" true rather than merely intended.
func TestFunnelRequestsAreRefusedEvenWhenOtherwiseValid(t *testing.T) {
	a := mustAuth(t, "kyle@example.com", true, false)

	h := signedIn("kyle@example.com")
	h[funnelHeader] = "true"

	if _, err := a.Identify(req(h)); err == nil {
		t.Fatal("a funnelled request was identified")
	}
	if _, err := a.Authorize(req(h)); err == nil {
		t.Fatal("a funnelled request was authorized")
	}
	// Even with DASH_ALLOW_ANY_TAILNET_USER, which is the most permissive this
	// can be configured to be. A funnelled request is not on the tailnet.
	if _, err := a.Authorize(req(h)); err == nil || !strings.Contains(err.Error(), "Funnel") {
		t.Fatalf("the refusal does not name the Funnel: %v", err)
	}
}

// THE ONLY CSRF DEFENCE LEFT. There is no cookie any more, so SameSite protects
// nothing: the proxy attaches identity to a cross-origin request as readily as
// to a same-origin one. A form post cannot set a custom header, and a fetch that
// does triggers a preflight this server never answers.
func TestMutatingActionsRequireTheActionHeader(t *testing.T) {
	a := mustAuth(t, "kyle@example.com", false, false)

	noHeader := req(map[string]string{identityHeader: "kyle@example.com"})
	if _, err := a.Authorize(noHeader); err == nil {
		t.Fatal("Authorize accepted a request with no " + csrfHeader)
	}

	// ...but the page itself does not need it. Identifying who is looking is not
	// the same question as whether they may press a button.
	if login, err := a.Identify(noHeader); err != nil || login != "kyle@example.com" {
		t.Fatalf("Identify required the action header: %q, %v", login, err)
	}
}

// Read-only refuses everything with an explanation, and still says who you are
// so the page can show it.
func TestReadOnlyRefusesEveryActionButStillIdentifies(t *testing.T) {
	a := mustAuth(t, "", false, true)
	if !a.ReadOnly() {
		t.Fatal("ReadOnly() is false with DASH_READ_ONLY set")
	}

	if _, err := a.Authorize(req(signedIn("anyone@example.com"))); err == nil {
		t.Fatal("a read-only dashboard authorized an action")
	}
	if login, err := a.Identify(req(signedIn("anyone@example.com"))); err != nil || login != "anyone@example.com" {
		t.Fatalf("read-only stopped identifying the caller: %q, %v", login, err)
	}
}

func TestAllowAnyTailnetUserHonoursEveryLogin(t *testing.T) {
	a := mustAuth(t, "", true, false)
	if _, err := a.Authorize(req(signedIn("whoever@example.com"))); err != nil {
		t.Fatalf("DASH_ALLOW_ANY_TAILNET_USER refused a tailnet login: %v", err)
	}
	// It still requires there to BE a login. "Any tailnet user" is not "anyone".
	if _, err := a.Authorize(req(map[string]string{csrfHeader: "1"})); err == nil {
		t.Fatal("DASH_ALLOW_ANY_TAILNET_USER authorized a request with no identity at all")
	}
}

// There is no way to present a bearer token any more, and no cookie to replay.
// Anything that looks like the old credential must be ignored rather than
// quietly accepted as a second scheme nobody designed -- that is the class of
// bug vault-mcp's removed auth paths were about.
func TestOldCredentialFormsAreNotHonoured(t *testing.T) {
	a := mustAuth(t, "kyle@example.com", false, false)

	for _, h := range []map[string]string{
		{"Authorization": "Bearer kyle@example.com", csrfHeader: "1"},
		{"Cookie": "homelab_dash=kyle@example.com", csrfHeader: "1"},
		{"X-Forwarded-User": "kyle@example.com", csrfHeader: "1"},
	} {
		if _, err := a.Authorize(req(h)); err == nil {
			t.Errorf("Authorize honoured a credential that is not the identity header: %v", h)
		}
	}
}
