package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A stand-in authorization server: discovery document plus a JWKS, which is all
// go-oidc needs to verify a token. Signature and issuer checks come from the
// library; the audience check is ours, and is the one worth testing.
type fakeAS struct {
	*httptest.Server
	key *rsa.PrivateKey
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	as := &fakeAS{key: key}
	mux := http.NewServeMux()
	as.Server = httptest.NewServer(mux)
	t.Cleanup(as.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                as.URL,
			"authorization_endpoint":                as.URL + "/authorize",
			"token_endpoint":                        as.URL + "/token",
			"jwks_uri":                              as.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       &key.PublicKey,
			KeyID:     "test",
			Algorithm: "RS256",
			Use:       "sig",
		}}})
	})
	return as
}

func (as *fakeAS) mint(t *testing.T, aud string, expires time.Time) string {
	t.Helper()
	return as.mintAs(t, "user_01", aud, expires)
}

// mintAs is mint with the subject chosen, which is what the allow list tests
// need: a token that is valid in every other respect and belongs to somebody
// else. That is precisely the token a self-service sign-up hands an attacker.
func (as *fakeAS) mintAs(t *testing.T, sub, aud string, expires time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: as.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"iss": as.URL,
		"sub": sub,
		"aud": []string{aud},
		"exp": expires.Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAccessTokenVerification(t *testing.T) {
	const resource = "https://vault-mcp.example.ts.net/mcp"
	as := newFakeAS(t)
	v := newOAuthVerifier(as.URL, resource, []string{"user_01"}, false, quietLogger())
	ctx := context.Background()

	t.Run("correct audience is accepted", func(t *testing.T) {
		sub, err := v.verify(ctx, as.mint(t, resource, time.Now().Add(time.Hour)))
		if err != nil {
			t.Fatalf("valid token rejected: %v", err)
		}
		// The subject is what makes the audit trail answer "who".
		if sub != "user_01" {
			t.Errorf("subject = %q, want %q", sub, "user_01")
		}
	})

	// The check that matters. Without it, any token the same tenant issued for
	// any other resource would open this vault.
	t.Run("token for another resource is rejected", func(t *testing.T) {
		_, err := v.verify(ctx, as.mint(t, "https://something-else.example.com/mcp", time.Now().Add(time.Hour)))
		if err == nil {
			t.Fatal("token minted for a different audience was accepted")
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		if _, err := v.verify(ctx, as.mint(t, resource, time.Now().Add(-time.Hour))); err == nil {
			t.Fatal("expired token was accepted")
		}
	})

	t.Run("garbage is rejected", func(t *testing.T) {
		if _, err := v.verify(ctx, "not.a.jwt"); err == nil {
			t.Fatal("malformed token was accepted")
		}
	})

	// Signed by a different key, everything else identical.
	t.Run("token from another issuer is rejected", func(t *testing.T) {
		other := newFakeAS(t)
		if _, err := v.verify(ctx, other.mint(t, resource, time.Now().Add(time.Hour))); err == nil {
			t.Fatal("token from a different authorization server was accepted")
		}
	})
}

// The signature, issuer, expiry and audience checks above are all satisfied by
// *any* account on the tenant. If the authorization server permits self-service
// sign-up -- AuthKit does by default, with Google, Microsoft, GitHub and Apple
// -- then a stranger who finds the hostname can obtain a token that passes every
// one of them. The hostname is not a secret either: Funnel needs a Let's Encrypt
// certificate and certificates are published to Certificate Transparency.
//
// So this is the check that makes the vault yours rather than the tenant's.
func TestSubjectAllowList(t *testing.T) {
	const resource = "https://vault-mcp.example.ts.net/mcp"
	as := newFakeAS(t)
	ctx := context.Background()

	t.Run("a subject on the list is accepted", func(t *testing.T) {
		v := newOAuthVerifier(as.URL, resource, []string{"user_01"}, false, quietLogger())
		sub, err := v.verify(ctx, as.mintAs(t, "user_01", resource, time.Now().Add(time.Hour)))
		if err != nil {
			t.Fatalf("allow-listed subject rejected: %v", err)
		}
		if sub != "user_01" {
			t.Errorf("subject = %q, want %q", sub, "user_01")
		}
	})

	// The whole point of the change. This token is signed by the right issuer,
	// unexpired, and carries the right audience -- it is exactly what a stranger
	// gets by signing up on the tenant.
	t.Run("another tenant user is rejected", func(t *testing.T) {
		v := newOAuthVerifier(as.URL, resource, []string{"user_01"}, false, quietLogger())
		_, err := v.verify(ctx, as.mintAs(t, "user_99", resource, time.Now().Add(time.Hour)))
		if err == nil {
			t.Fatal("a valid token for a different subject was accepted")
		}
		if !errors.Is(err, errSubjectNotAllowed) {
			t.Errorf("error = %v, want errSubjectNotAllowed", err)
		}
	})

	t.Run("several subjects may be listed", func(t *testing.T) {
		v := newOAuthVerifier(as.URL, resource, []string{"user_01", "user_02"}, false, quietLogger())
		if _, err := v.verify(ctx, as.mintAs(t, "user_02", resource, time.Now().Add(time.Hour))); err != nil {
			t.Fatalf("second allow-listed subject rejected: %v", err)
		}
	})

	// An empty list must not mean "allow". loadConfig refuses to start in that
	// state, and this asserts the verifier does not quietly permit it either --
	// the same failure mode that once left this route wide open.
	t.Run("an empty list allows nobody", func(t *testing.T) {
		v := newOAuthVerifier(as.URL, resource, nil, false, quietLogger())
		if _, err := v.verify(ctx, as.mintAs(t, "user_01", resource, time.Now().Add(time.Hour))); err == nil {
			t.Fatal("an empty allow list accepted a token")
		}
	})

	t.Run("anySubject honours every account, and must be explicit", func(t *testing.T) {
		v := newOAuthVerifier(as.URL, resource, nil, true, quietLogger())
		if _, err := v.verify(ctx, as.mintAs(t, "user_99", resource, time.Now().Add(time.Hour))); err != nil {
			t.Fatalf("OAUTH_ALLOW_ANY_SUBJECT did not honour a tenant account: %v", err)
		}
	})
}

// A blank allow list must stop the server, not open it. This is the same
// judgement as MCP_ALLOW_NO_AUTH: the permissive state has to be stated.
func TestSubjectAllowListRequiredWithIssuer(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		t.Setenv("OAUTH_ISSUER", "https://tenant.authkit.app")
		t.Setenv("OAUTH_RESOURCE", "https://vault-mcp.example.ts.net/mcp")
		t.Setenv("OAUTH_ALLOWED_SUBJECTS", "")
		t.Setenv("OAUTH_ALLOW_ANY_SUBJECT", "")
	}

	t.Run("issuer set with no subjects refuses to start", func(t *testing.T) {
		base(t)
		if _, err := loadConfig(false); err == nil {
			t.Fatal("loadConfig accepted OAUTH_ISSUER with an empty OAUTH_ALLOWED_SUBJECTS")
		}
	})

	t.Run("an explicit override starts", func(t *testing.T) {
		base(t)
		t.Setenv("OAUTH_ALLOW_ANY_SUBJECT", "1")
		if _, err := loadConfig(false); err != nil {
			t.Fatalf("OAUTH_ALLOW_ANY_SUBJECT=1 did not start: %v", err)
		}
	})

	t.Run("subjects are split and trimmed", func(t *testing.T) {
		base(t)
		t.Setenv("OAUTH_ALLOWED_SUBJECTS", " user_01 , user_02 ,, ")
		c, err := loadConfig(false)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"user_01", "user_02"}
		if len(c.oauthSubjects) != len(want) {
			t.Fatalf("subjects = %v, want %v", c.oauthSubjects, want)
		}
		for i := range want {
			if c.oauthSubjects[i] != want[i] {
				t.Errorf("subjects[%d] = %q, want %q", i, c.oauthSubjects[i], want[i])
			}
		}
	})
}

// The client only begins the OAuth flow if the 401 carries this header, and it
// is ignored on any other status.
func TestUnauthorizedCarriesChallenge(t *testing.T) {
	const resource = "https://vault-mcp.example.ts.net/mcp"
	s := testServer(&config{})
	s.oauth = newOAuthVerifier("https://issuer.example.com", resource, []string{"user_01"}, false, quietLogger())

	h := s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler reached without a credential")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer resource_metadata="` + resource + resourceMetadataPath + `"`
	if got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

// resource must match the connector URL exactly, path included, or the client
// rejects the document and the connection fails as "couldn't reach the server".
func TestResourceMetadataDocument(t *testing.T) {
	const resource = "https://vault-mcp.example.ts.net/mcp"
	const issuer = "https://tenant.authkit.app"
	v := newOAuthVerifier(issuer, resource, []string{"user_01"}, false, quietLogger())

	rec := httptest.NewRecorder()
	v.resourceMetadata(rec, httptest.NewRequest(http.MethodGet, resourceMetadataPath, nil))

	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Resource != resource {
		t.Errorf("resource = %q, want %q", doc.Resource, resource)
	}
	if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != issuer {
		t.Errorf("authorization_servers = %v, want [%q]", doc.AuthorizationServers, issuer)
	}
}

func TestOauthIssuerRequiresResource(t *testing.T) {
	t.Setenv("OAUTH_ISSUER", "https://tenant.authkit.app")
	t.Setenv("OAUTH_RESOURCE", "")
	t.Setenv("MCP_TOKEN", "")
	t.Setenv("MCP_PATH_SECRET", "")
	if _, err := loadConfig(false); err == nil {
		t.Fatal("loadConfig accepted OAUTH_ISSUER with no OAUTH_RESOURCE")
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"BEARER  abc": "abc",
		"Basic abc":   "",
		"abc":         "",
		"":            "",
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := bearerToken(r); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

// End-to-end: a real MCP tool call, authenticated with a real signed token,
// asserting the audit line names the token subject.
//
// This is the test the identity work hangs on. The subject is attached to the
// HTTP request context by the auth middleware and read again inside a tool
// handler, which only works if the MCP SDK propagates the request context
// through to handlers. That is an assumption about someone else's library, so it
// is checked rather than believed -- if it ever stops holding, the audit trail
// silently degrades to "unknown" and nothing else breaks to tell you.
func TestAuditTrailNamesTheSubject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Fishing.md"), []byte("Rods and pike.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vault, err := NewVault(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	as := newFakeAS(t)
	const resource = "https://vault-mcp.example.ts.net/mcp"

	var logs bytes.Buffer
	s := &server{
		cfg:   &config{oauthIssuer: as.URL, oauthResource: resource},
		vault: vault,
		log:   slog.New(slog.NewTextHandler(&logs, nil)),
	}
	s.oauth = newOAuthVerifier(as.URL, resource, []string{"user_01"}, false, s.log)

	h := s.withAuth(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcpServer() },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	))

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_notes","arguments":{"query":"pike"}}}`
	req := httptest.NewRequest(http.MethodPost, resource, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+as.mint(t, resource, time.Now().Add(time.Hour)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, "name=search_notes") {
		t.Fatalf("no audit line for the tool call:\n%s", out)
	}
	if !strings.Contains(out, "sub=user_01") {
		t.Fatalf("audit line does not name the subject (context propagation broken?):\n%s", out)
	}
}
