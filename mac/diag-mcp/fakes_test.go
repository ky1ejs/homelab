package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Copied from gmail-mcp/fakes_test.go, the authorization server part only.

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- a stand-in authorization server, the same shape as vault-mcp's --------

// TLS, and reached through the production egress client (see testVerifier),
// so the tests exercise the same transport rules as the real server.
type fakeAS struct {
	*httptest.Server
	key  *rsa.PrivateKey
	hits atomic.Int32 // requests of any kind
	// jwksURI, when set, replaces the advertised key URL.
	jwksURI string
	// down makes discovery fail.
	down atomic.Bool
}

// claude.ai's real client_id is its metadata URL, so the tests use one.
const testClient = "https://claude.ai/oauth/mcp-oauth-client-metadata"

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	as := &fakeAS{key: key}
	mux := http.NewServeMux()
	as.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.hits.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(as.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if as.down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                as.URL,
			"authorization_endpoint":                as.URL + "/authorize",
			"token_endpoint":                        as.URL + "/token",
			"jwks_uri":                              firstNonEmpty(as.jwksURI, as.URL+"/jwks"),
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig",
		}}})
	})
	return as
}

// mint signs a token as claude.ai's would look: the test client, an array
// audience.
func (as *fakeAS) mint(t *testing.T, sub, aud string, expires time.Time) string {
	t.Helper()
	return as.mintClaims(t, map[string]any{"sub": sub, "aud": []string{aud}, "exp": expires.Unix()})
}

// mintClaims signs the defaults overlaid with claims; a nil value deletes one.
func (as *fakeAS) mintClaims(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: as.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]any{
		"iss": as.URL, "sub": "user_kyle", "aud": []string{testResource}, "client_id": testClient,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(all, k)
		} else {
			all[k] = v
		}
	}
	payload, _ := json.Marshal(all)
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
