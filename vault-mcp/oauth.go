package main

import (
	"context"
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

// OAuth 2.1 resource-server support, with the authorization server hosted
// elsewhere (WorkOS AuthKit).
//
// This file deliberately implements only three things:
//
//  1. protected resource metadata (RFC 9728), so a client can find the AS
//  2. the 401 + WWW-Authenticate challenge that starts the flow
//  3. access-token validation against the AS's published keys
//
// Everything genuinely dangerous — /authorize, /token, dynamic client
// registration, PKCE verification, redirect-URI validation, consent, refresh
// rotation — belongs to the authorization server and is deliberately NOT here.
// Writing those into an internet-facing binary to serve one person's notes was
// the alternative, and it is a much larger amount of code that has to be exactly
// right. See README.md#how-this-authenticates.
type oauthVerifier struct {
	issuer   string
	resource string
	log      *slog.Logger

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
}

func newOAuthVerifier(issuer, resource string, log *slog.Logger) *oauthVerifier {
	return &oauthVerifier{issuer: issuer, resource: resource, log: log}
}

// challenge is the WWW-Authenticate value that tells a client where to look.
//
// Anthropic's client requires the 401 status specifically — it does not honour
// this header on a 200 — and uses resource_metadata to skip probing well-known
// paths. Without it the connection fails as "couldn't reach the MCP server".
func (o *oauthVerifier) challenge() string {
	return fmt.Sprintf("Bearer resource_metadata=%q", o.resource+resourceMetadataPath)
}

const resourceMetadataPath = "/.well-known/oauth-protected-resource"

// discovery is lazy and retried rather than done at startup.
//
// Fetching the AS's keys at boot would mean a WorkOS outage, or the NAS coming
// up before its network does, leaves the container in a crash loop — taking the
// URL-secret path down with it for a dependency only the OAuth path needs.
func (o *oauthVerifier) get(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.verifier != nil {
		return o.verifier, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	provider, err := oidc.NewProvider(ctx, o.issuer)
	if err != nil {
		return nil, fmt.Errorf("oauth discovery on %s: %w", o.issuer, err)
	}
	// SkipClientIDCheck because the audience of an access token is the resource
	// (this server), not a client id. The audience is checked explicitly below,
	// which is the check that actually matters: a token minted for a different
	// resource on the same tenant must not open this one.
	o.verifier = provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	return o.verifier, nil
}

var errNoAudience = errors.New("token audience does not include this server")

// verify returns the token subject, and a nil error, only for a token this
// server should honour. The subject is what makes the audit trail answer "who"
// rather than only "what".
func (o *oauthVerifier) verify(ctx context.Context, raw string) (string, error) {
	v, err := o.get(ctx)
	if err != nil {
		return "", err
	}
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		return "", err
	}

	// go-oidc validates the signature, issuer and expiry. Audience is ours to
	// enforce, and skipping it would accept any token the same tenant issued for
	// any other resource.
	for _, aud := range tok.Audience {
		if aud == o.resource {
			return tok.Subject, nil
		}
	}
	return "", fmt.Errorf("%w (got %v, want %q)", errNoAudience, tok.Audience, o.resource)
}

// The authenticated subject, carried from the auth middleware to the tool
// handlers. A private key type so nothing else can collide with it.
type subjectKey struct{}

func withSubject(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, subjectKey{}, sub)
}

// subjectFrom returns the authenticated subject, or "unknown" when the request
// did not carry one -- which is the case under MCP_ALLOW_NO_AUTH, and would also
// be the case if context propagation ever silently broke. "unknown" in the log
// is a signal, not a blank.
func subjectFrom(ctx context.Context) string {
	if s, ok := ctx.Value(subjectKey{}).(string); ok && s != "" {
		return s
	}
	return "unknown"
}

// resourceMetadata serves RFC 9728, pointing clients at the authorization
// server. Unauthenticated on purpose: it is discovery data, and a client that
// cannot read it cannot begin the flow.
func (o *oauthVerifier) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	// `resource` MUST match the connector URL exactly as it is entered in
	// Claude, path included, or the client rejects the document.
	body := map[string]any{
		"resource":                 o.resource,
		"authorization_servers":    []string{o.issuer},
		"bearer_methods_supported": []string{"header"},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = json.NewEncoder(w).Encode(body)
}

// bearerToken extracts the credential from an Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}
