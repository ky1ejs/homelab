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

// OAuth 2.1 resource-server support against the same hosted authorization
// server as vault-mcp (WorkOS AuthKit).
//
// This is vault-mcp/oauth.go with two deliberate differences, and the two
// should otherwise be kept in step:
//
//   - There is no "allow any subject" switch. vault-mcp has one so that
//     honouring every account on the tenant is something you state rather than
//     get by leaving a variable blank. For a server that can send mail as you
//     there is no account other than yours worth stating, so the escape hatch
//     does not exist rather than existing and defaulting off.
//   - Discovery and key fetches go through the egress-restricted client, so
//     the authorization server's host is the only non-Google host this binary
//     can reach.
//
// Everything genuinely dangerous -- /authorize, /token, client registration,
// PKCE verification, redirect-URI validation, consent, refresh rotation --
// belongs to the authorization server and is deliberately not here. See
// vault-mcp/README.md#why-not-an-authorization-server-of-our-own.
type oauthVerifier struct {
	issuer   string
	resource string
	subjects map[string]bool
	client   *http.Client
	log      *slog.Logger

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
}

func newOAuthVerifier(issuer, resource string, subjects []string, client *http.Client, log *slog.Logger) *oauthVerifier {
	set := make(map[string]bool, len(subjects))
	for _, s := range subjects {
		set[s] = true
	}
	return &oauthVerifier{issuer: issuer, resource: resource, subjects: set, client: client, log: log}
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
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// go-oidc keeps the client from this context (and only the client) for
	// later key refreshes, so the egress restriction holds for its lifetime.
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, o.client), o.issuer)
	if err != nil {
		return nil, fmt.Errorf("oauth discovery on %s: %w", o.issuer, err)
	}
	// The audience of an access token is the resource, not a client id, so the
	// library's client-id check is skipped and the audience is checked below.
	o.verifier = provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	return o.verifier, nil
}

var (
	errNoAudience        = errors.New("token audience does not include this server")
	errSubjectNotAllowed = errors.New("token subject is not on the allow list")
)

// verify returns the subject, and a nil error, only for a token this server
// should honour.
func (o *oauthVerifier) verify(ctx context.Context, raw string) (string, error) {
	v, err := o.get(ctx)
	if err != nil {
		return "", err
	}
	tok, err := v.Verify(oidc.ClientContext(ctx, o.client), raw)
	if err != nil {
		return "", err
	}

	// Audience matters more here than anywhere else in the repo: vault-mcp and
	// gmail-mcp share a tenant, so without this check a token minted for the
	// vault would open your mailbox, and the other way round.
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
