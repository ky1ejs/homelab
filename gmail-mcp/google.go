package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Google OAuth, from the client side only. The server never handles a Google
// authorization code over the internet: -login does that once, on the NAS,
// over loopback. After that this file only ever refreshes.

// The scopes -login asks for, and the only ones. compose covers drafts and
// send; there is no gmail.modify (labels, archive, trash) and no full
// mail.google.com access, so the token itself cannot delete mail or change
// filters and forwarding even if this code were made to try.
var googleScopes = []string{
	"openid",
	"email",
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.compose",
}

const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleRevokeURL   = "https://oauth2.googleapis.com/revoke"
	googleUserinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

var errGoogleRevoked = errors.New("Google no longer accepts the stored token (revoked, expired or password changed): run `gmail-mcp -login` again")

type googleToken struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

type googleOAuth struct {
	clientID     string
	clientSecret string
	http         *http.Client
	tokenURL     string // overridden in tests
	userinfoURL  string
	revokeURL    string
}

func newGoogleOAuth(clientID, clientSecret string, client *http.Client) *googleOAuth {
	return &googleOAuth{
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         client,
		tokenURL:     googleTokenURL,
		userinfoURL:  googleUserinfoURL,
		revokeURL:    googleRevokeURL,
	}
}

func (g *googleOAuth) token(ctx context.Context, form url.Values) (*googleToken, error) {
	form.Set("client_id", g.clientID)
	form.Set("client_secret", g.clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var t googleToken
	_ = json.NewDecoder(resp.Body).Decode(&t)
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		if t.Error == "invalid_grant" {
			return nil, errGoogleRevoked
		}
		return nil, fmt.Errorf("google token endpoint: status %d %s", resp.StatusCode, t.Error)
	}
	return &t, nil
}

func (g *googleOAuth) exchange(ctx context.Context, code, verifier, redirectURI string) (*googleToken, error) {
	return g.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	})
}

func (g *googleOAuth) refresh(ctx context.Context, refreshToken string) (*googleToken, error) {
	return g.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

// email reports which account an access token belongs to.
func (g *googleOAuth) email(ctx context.Context, accessToken string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.userinfoURL, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := g.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("google userinfo: status %d", resp.StatusCode)
	}
	var body struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false, err
	}
	return strings.ToLower(body.Email), body.EmailVerified, nil
}

func (g *googleOAuth) revoke(ctx context.Context, token string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.revokeURL, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := g.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

// accessTokens hands out a Gmail access token, refreshing from the sealed
// refresh token when the cached one is within a minute of expiry.
type accessTokens struct {
	oauth *googleOAuth
	store *tokenStore

	mu      sync.Mutex
	current string
	expiry  time.Time
}

func (a *accessTokens) Get(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current != "" && time.Until(a.expiry) > time.Minute {
		return a.current, nil
	}
	refresh, err := a.store.Load()
	if err != nil {
		return "", err
	}
	t, err := a.oauth.refresh(ctx, refresh)
	if errors.Is(err, errGoogleRevoked) {
		a.current = ""
		_ = a.store.Remove()
		return "", err
	}
	if err != nil {
		return "", err
	}
	a.current, a.expiry = t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn)*time.Second)
	return a.current, nil
}
