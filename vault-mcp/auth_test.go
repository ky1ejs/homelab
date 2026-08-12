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

func TestPathSecretAuth(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	s := testServer(&config{pathSecret: secret})

	var ran bool
	h := s.withPathSecret(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name     string
		path     string
		wantCode int
		wantRan  bool
	}{
		{"correct secret", "/mcp/" + secret, http.StatusOK, true},
		{"correct secret, trailing path", "/mcp/" + secret + "/extra", http.StatusOK, true},
		{"wrong secret", "/mcp/" + strings.Repeat("f", len(secret)), http.StatusUnauthorized, false},
		{"empty secret", "/mcp/", http.StatusUnauthorized, false},
		{"prefix of the secret", "/mcp/" + secret[:20], http.StatusUnauthorized, false},
		{"secret plus suffix in same segment", "/mcp/" + secret + "x", http.StatusUnauthorized, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran = false
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if ran != tc.wantRan {
				t.Errorf("handler reached = %v, want %v", ran, tc.wantRan)
			}
		})
	}
}

// The bare /mcp route must keep requiring the header even when a URL secret is
// configured, so the endpoint a scanner finds by guessing the hostname gives up
// nothing.
func TestBareMcpStillRequiresHeader(t *testing.T) {
	s := testServer(&config{
		authHeader: "Authorization",
		authValue:  "Bearer sekrit",
		pathSecret: "0123456789abcdef0123456789abcdef",
	})

	var ran bool
	h := s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran = true }))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || ran {
		t.Errorf("bare /mcp without header: status %d, reached %v; want 401 and not reached", rec.Code, ran)
	}
}

// Regression: configuring ONLY a URL secret left authValue empty, and withAuth
// treated empty as "no auth configured, pass through" — so bare /mcp served the
// whole vault unauthenticated on a public hostname. Every unit test passed;
// only an end-to-end request against a real config caught it.
func TestUrlSecretOnlyDoesNotOpenBareMcp(t *testing.T) {
	t.Setenv("MCP_TOKEN", "")
	t.Setenv("MCP_ALLOW_NO_AUTH", "")
	t.Setenv("MCP_PATH_SECRET", "0123456789abcdef0123456789abcdef")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	s := testServer(cfg)

	var ran bool
	h := s.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))

	if rec.Code != http.StatusUnauthorized || ran {
		t.Fatalf("bare /mcp with only a URL secret: status %d, reached %v; want 401 and not reached", rec.Code, ran)
	}
}

// A short secret is guessable and there is no human to notice the attempts, so
// the server must refuse to start rather than serve a weak one.
func TestShortPathSecretRefusesToStart(t *testing.T) {
	t.Setenv("MCP_PATH_SECRET", "tooshort")
	t.Setenv("MCP_TOKEN", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted an 8-character MCP_PATH_SECRET")
	}
}

func TestPathSecretRejectsSeparators(t *testing.T) {
	t.Setenv("MCP_PATH_SECRET", "0123456789abcdef0123456789abcdef/extra")
	t.Setenv("MCP_TOKEN", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted a secret containing a slash")
	}
}

func TestNoCredentialAtAllRefusesToStart(t *testing.T) {
	t.Setenv("MCP_PATH_SECRET", "")
	t.Setenv("MCP_TOKEN", "")
	t.Setenv("MCP_ALLOW_NO_AUTH", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig started with no credential configured")
	}
}
