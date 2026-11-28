package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OAuth 2.1 resource-server support against the same hosted authorization
// server as vault-mcp and gmail-mcp (WorkOS AuthKit).
//
// This is gmail-mcp/oauth.go, copied whole, and the two should be kept in
// step. Its header says how it differs from vault-mcp's: no "allow any
// subject" switch, key fetches through the egress-restricted client, the
// client pinned as well as the subject, and claims pre-checked before any key
// fetch. Each matters as much here: the data is read-only, but it maps the
// mac studio, and the server answers on a public hostname.
//
// Everything genuinely dangerous -- /authorize, /token, client registration,
// PKCE verification, redirect-URI validation, consent, refresh rotation --
// belongs to the authorization server and is deliberately not here. See
// vault-mcp/README.md#why-not-an-authorization-server-of-our-own.
type oauthVerifier struct {
	issuer   string
	resource string
	subjects map[string]bool
	clients  map[string]bool
	client   *http.Client
	log      *slog.Logger

	mu         sync.Mutex
	verifier   *oidc.IDTokenVerifier
	failedAt   time.Time // last discovery failure, so an outage is not retried on every request
	failure    error
	retryAfter time.Duration
}

func newOAuthVerifier(issuer, resource string, subjects, clients []string, client *http.Client, log *slog.Logger) *oauthVerifier {
	set := func(in []string) map[string]bool {
		m := make(map[string]bool, len(in))
		for _, s := range in {
			m[s] = true
		}
		return m
	}
	return &oauthVerifier{
		issuer: issuer, resource: resource, subjects: set(subjects), clients: set(clients),
		client: client, log: log, retryAfter: 5 * time.Second,
	}
}

func (o *oauthVerifier) challenge() string {
	return fmt.Sprintf("Bearer resource_metadata=%q", o.resource+resourceMetadataPath)
}

const resourceMetadataPath = "/.well-known/oauth-protected-resource"

// Lazy and retried rather than done at startup, for the same reason as
// vault-mcp: an authorization-server outage at boot should not become a crash
// loop.
func (o *oauthVerifier) get(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.verifier != nil {
		return o.verifier, nil
	}
	if o.failure != nil && time.Since(o.failedAt) < o.retryAfter {
		return nil, o.failure
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// go-oidc keeps the client from this context (and only the client) for
	// later key refreshes, so the egress restriction holds for its lifetime.
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, o.client), o.issuer)
	if err != nil {
		o.failure, o.failedAt = fmt.Errorf("oauth discovery on %s: %w", o.issuer, err), time.Now()
		return nil, o.failure
	}
	o.failure = nil
	// The audience of an access token is the resource, not a client id, so the
	// library's client-id check is skipped and the audience is checked below.
	o.verifier = provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	return o.verifier, nil
}

var (
	errNoAudience        = errors.New("token audience does not include this server")
	errSubjectNotAllowed = errors.New("token subject is not on the allow list")
	errClientNotAllowed  = errors.New("token client_id is not on the allow list")
	errMalformedToken    = errors.New("token is not a JWT this server could accept")
)

type accessClaims struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"`
	Audience audience `json:"aud"`
	ClientID string   `json:"client_id"`
}

// audience accepts both forms RFC 7519 allows: a string or an array.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (c accessClaims) hasAudience(resource string) bool {
	for _, aud := range c.Audience {
		if aud == resource {
			return true
		}
	}
	return false
}

// unverifiedClaims reads a JWT payload WITHOUT checking its signature. It is
// used only to refuse early; nothing it returns is trusted.
func unverifiedClaims(raw string) (accessClaims, error) {
	var c accessClaims
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return c, errMalformedToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &c) != nil {
		return c, errMalformedToken
	}
	return c, nil
}

// verify returns the subject, and a nil error, only for a token this server
// should honour.
func (o *oauthVerifier) verify(ctx context.Context, raw string) (string, error) {
	// Refuse what could never pass before discovery or a key fetch happens.
	pre, err := unverifiedClaims(raw)
	if err != nil {
		return "", err
	}
	// These are checked again after the signature; here they only decide
	// whether a token is worth a network round trip.
	switch {
	case pre.Issuer != o.issuer:
		return "", fmt.Errorf("%w (issuer does not match)", errMalformedToken)
	case !pre.hasAudience(o.resource):
		return "", errNoAudience
	case !o.subjects[pre.Subject]:
		return "", errSubjectNotAllowed
	}

	v, err := o.get(ctx)
	if err != nil {
		return "", err
	}
	tok, err := v.Verify(oidc.ClientContext(ctx, o.client), raw)
	if err != nil {
		return "", err
	}
	var claims accessClaims
	if err := tok.Claims(&claims); err != nil {
		return "", err
	}

	// Audience matters more here than anywhere else in the repo: vault-mcp and
	// gmail-mcp share a tenant with this server, so without this check a
	// token minted for the vault or the mailbox would open this one, and the
	// other way round.
	audienceOK := false
	for _, aud := range tok.Audience {
		if aud == o.resource {
			audienceOK = true
			break
		}
	}
	if !audienceOK {
		return "", fmt.Errorf("%w (got %v, want %q)", errNoAudience, tok.Audience, o.resource)
	}
	if !o.subjects[tok.Subject] {
		return "", fmt.Errorf("%w (subject %q)", errSubjectNotAllowed, tok.Subject)
	}
	// Logged in full on purpose: a client id is not a secret, and the first
	// connect from claude.ai is how you learn the value to allow.
	if !o.clients[claims.ClientID] {
		return "", fmt.Errorf("%w (client_id %q; add it to OAUTH_ALLOWED_CLIENTS if this is your claude.ai connector)", errClientNotAllowed, claims.ClientID)
	}
	return tok.Subject, nil
}

type subjectKey struct{}

func withSubject(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, subjectKey{}, sub)
}

func subjectFrom(ctx context.Context) string {
	if s, ok := ctx.Value(subjectKey{}).(string); ok && s != "" {
		return s
	}
	return "unknown"
}

// resourceMetadata serves RFC 9728. Unauthenticated on purpose: a client that
// cannot read it cannot begin the flow.
func (o *oauthVerifier) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"resource":                 o.resource,
		"authorization_servers":    []string{o.issuer},
		"bearer_methods_supported": []string{"header"},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = json.NewEncoder(w).Encode(body)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}
