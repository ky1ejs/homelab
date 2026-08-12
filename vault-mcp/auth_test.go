package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(cfg *config) *server {
	return &server{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// The regression that justifies having only one credential scheme.
//
// withAuth once treated "nothing configured" as "pass through", so a deployment
// whose credential lived somewhere it did not check served the whole vault
// unauthenticated on a public hostname. Every unit test passed, including one
// asserting that exact property, because it built its config by hand. This one
// goes through loadConfig.
func TestNoIssuerRefusesToStart(t *testing.T) {
	t.Setenv("OAUTH_ISSUER", "")
	t.Setenv("OAUTH_RESOURCE", "")
	t.Setenv("MCP_ALLOW_NO_AUTH", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig started with no authorization server configured")
	}
}

// Only an explicit opt-out opens the route, and it must not be reachable by
// simply leaving configuration blank.
func TestUnconfiguredServerRefusesRequests(t *testing.T) {
	s := testServer(&config{})

	h := s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler reached with no credential and no opt-out")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAllowNoAuthIsExplicitOnly(t *testing.T) {
	var ran bool
	s := testServer(&config{allowNoAuth: true})
	h := s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))
	if !ran || rec.Code == http.StatusUnauthorized {
		t.Fatalf("MCP_ALLOW_NO_AUTH did not open the route: status %d, reached %v", rec.Code, ran)
	}
}
