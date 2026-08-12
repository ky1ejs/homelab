package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
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
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: as.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"iss": as.URL,
		"sub": "user_01",
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
	v := newOAuthVerifier(as.URL, resource, quietLogger())
	ctx := context.Background()

	t.Run("correct audience is accepted", func(t *testing.T) {
		if err := v.verify(ctx, as.mint(t, resource, time.Now().Add(time.Hour))); err != nil {
			t.Fatalf("valid token rejected: %v", err)
		}
	})

	// The check that matters. Without it, any token the same tenant issued for
	// any other resource would open this vault.
	t.Run("token for another resource is rejected", func(t *testing.T) {
		err := v.verify(ctx, as.mint(t, "https://something-else.example.com/mcp", time.Now().Add(time.Hour)))
		if err == nil {
			t.Fatal("token minted for a different audience was accepted")
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		if err := v.verify(ctx, as.mint(t, resource, time.Now().Add(-time.Hour))); err == nil {
			t.Fatal("expired token was accepted")
		}
	})

	t.Run("garbage is rejected", func(t *testing.T) {
		if err := v.verify(ctx, "not.a.jwt"); err == nil {
			t.Fatal("malformed token was accepted")
		}
	})

	// Signed by a different key, everything else identical.
	t.Run("token from another issuer is rejected", func(t *testing.T) {
		other := newFakeAS(t)
		if err := v.verify(ctx, other.mint(t, resource, time.Now().Add(time.Hour))); err == nil {
			t.Fatal("token from a different authorization server was accepted")
		}
	})
}

// The client only begins the OAuth flow if the 401 carries this header, and it
// is ignored on any other status.
func TestUnauthorizedCarriesChallenge(t *testing.T) {
	const resource = "https://vault-mcp.example.ts.net/mcp"
	s := testServer(&config{})
	s.oauth = newOAuthVerifier("https://issuer.example.com", resource, quietLogger())

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
	v := newOAuthVerifier(issuer, resource, quietLogger())

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
	if _, err := loadConfig(); err == nil {
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
