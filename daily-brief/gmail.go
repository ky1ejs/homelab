package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Gmail, from the client side only, and only to send.
//
// The same shape as gmail-mcp's google.go and store.go, cut down to what one
// daily send needs, and deliberately not shared with it: the two stacks keep
// separate Google Cloud projects, OAuth clients and grants, so revoking one
// cannot cut the other off. Two clients in one project share the user's grant
// for that project, and revoking either one's token is reported to revoke
// both (DECISIONS.md, "A daily brief by email and podcast").

// The scopes gmail-login asks for, and the only ones. gmail.send can send mail
// and nothing else: it cannot read, search, draft, label or delete, so the
// token in /state is worth one thing to whoever takes it, and the publisher
// never sees the mailbox.
var googleScopes = []string{
	"openid",
	"email",
	"https://www.googleapis.com/auth/gmail.send",
}

const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleRevokeURL   = "https://oauth2.googleapis.com/revoke"
	googleUserinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	gmailSendURL      = "https://gmail.googleapis.com/gmail/v1/users/me/messages/send"
)

var errGoogleRevoked = errors.New("Google no longer accepts the stored token (revoked, expired or password changed): run `daily-brief gmail-login` again")

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
	// Overridden in tests.
	tokenURL, userinfoURL, revokeURL, sendURL string
}

func newGoogleOAuth(clientID, clientSecret string, client *http.Client) *googleOAuth {
	return &googleOAuth{
		clientID: clientID, clientSecret: clientSecret, http: client,
		tokenURL: googleTokenURL, userinfoURL: googleUserinfoURL,
		revokeURL: googleRevokeURL, sendURL: gmailSendURL,
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
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&t)
	if resp.StatusCode != http.StatusOK || t.AccessToken == "" {
		if t.Error == "invalid_grant" {
			return nil, errGoogleRevoked
		}
		return nil, fmt.Errorf("google token endpoint: status %d %s", resp.StatusCode, truncate(t.Error, 60))
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return "", false, err
	}
	return strings.ToLower(body.Email), body.EmailVerified, nil
}

// revoke asks Google to drop a grant. Anything but a 200 is a failure that the
// caller reports, because a grant meant to be refused stays live until it is
// removed by hand.
func (g *googleOAuth) revoke(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.revokeURL, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("google revoke: status %d", resp.StatusCode)
	}
	return nil
}

// send posts one RFC 5322 message to messages.send and returns its id.
func (g *googleOAuth) send(ctx context.Context, accessToken string, raw []byte) (string, error) {
	body, err := json.Marshal(map[string]string{"raw": base64.URLEncoding.EncodeToString(raw)})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.sendURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		if beforeRequest(err) {
			return "", notSent(err)
		}
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		ID    string `json:"id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r)
	if resp.StatusCode != http.StatusOK {
		// Gmail answered and refused it. Google asks clients to retry its
		// 5xx answers too, which it would not if the message had gone.
		return "", notSent(fmt.Errorf("gmail send: status %d %s", resp.StatusCode, truncate(r.Error.Message, 200)))
	}
	if r.ID == "" {
		return "", errors.New("gmail send: status 200 with no message id")
	}
	return r.ID, nil
}

// notSentError marks a failure that came before Gmail could have accepted the
// message, so sending again cannot deliver it twice. Every other send error
// leaves the outcome unknown, and publish does not repeat an unknown send.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

func notSent(err error) error { return &notSentError{err} }

func definitelyNotSent(err error) bool {
	var e *notSentError
	return errors.As(err, &e)
}

// beforeRequest reports an error from before any of the request was written:
// the name did not resolve, or the connection was never made.
func beforeRequest(err error) bool {
	var dns *net.DNSError
	var op *net.OpError
	return errors.As(err, &dns) || (errors.As(err, &op) && op.Op == "dial")
}

// Mailer sends one message. An interface so publish's tests need no Google.
type Mailer interface {
	Send(ctx context.Context, raw []byte) (messageID string, err error)
}

// gmailMailer refreshes on every send. That is one or two token requests a
// day, so nothing is cached in memory or on disk beyond the refresh token.
type gmailMailer struct {
	oauth *googleOAuth
	store *tokenStore
}

func (m *gmailMailer) Send(ctx context.Context, raw []byte) (string, error) {
	refresh, err := m.store.Load()
	if err != nil {
		return "", notSent(err)
	}
	// Nothing has been sent while the access token is being fetched.
	t, err := m.oauth.refresh(ctx, refresh)
	if err != nil {
		return "", notSent(err)
	}
	return m.oauth.send(ctx, t.AccessToken, raw)
}

// The sealed refresh token, as gmail-mcp keeps its own.
//
// It is sealed with AES-256-GCM under BRIEF_TOKEN_KEY, which lives in .env in
// the checkout, while the file lives in /state. What the sealing buys is
// narrow: a stray copy of the state directory, such as a backup, is not a
// credential. It does not protect the file from code running in this
// container, which can read the key from /proc; that is why the only
// third-party binary in the image, save-to-spotify, runs in brief-podcast and
// never here. Against someone who owns the NAS it is nothing, and is not
// claimed to be.
//
// The account is bound in as additional authenticated data, so a token sealed
// for one mailbox cannot be presented as another's by editing the JSON.

var (
	errNoToken      = errors.New("no Google token yet: run `daily-brief gmail-login` once")
	errTokenAccount = errors.New("the stored Google token belongs to a different account than GMAIL_ACCOUNT")
)

type tokenFile struct {
	Version int       `json:"version"`
	Account string    `json:"account"`
	Scope   string    `json:"scope"`
	Sealed  string    `json:"sealed"`
	SavedAt time.Time `json:"saved_at"`
}

type tokenStore struct {
	path    string
	key     []byte
	account string
}

func parseKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("BRIEF_TOKEN_KEY is empty")
	}
	var key []byte
	var err error
	if len(raw) == 64 {
		key, err = hex.DecodeString(raw)
	} else {
		key, err = base64.StdEncoding.DecodeString(raw)
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("BRIEF_TOKEN_KEY must be 32 bytes, as base64 or hex (openssl rand -base64 32)")
	}
	return key, nil
}

func (s *tokenStore) aad() []byte {
	return []byte("daily-brief/google-refresh-token/v1/" + strings.ToLower(s.account))
}

func (s *tokenStore) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Save writes temp-then-rename, mode 0600: a crash mid-write leaves the old
// token, never half of a new one.
func (s *tokenStore) Save(refreshToken, scope string) error {
	aead, err := s.gcm()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := aead.Seal(nonce, nonce, []byte(refreshToken), s.aad())
	body, err := json.MarshalIndent(tokenFile{
		Version: 1,
		Account: strings.ToLower(s.account),
		Scope:   scope,
		Sealed:  base64.RawURLEncoding.EncodeToString(sealed),
		SavedAt: time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, append(body, '\n'), 0o600)
}

func (s *tokenStore) Load() (string, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errNoToken
	}
	if err != nil {
		return "", err
	}
	var f tokenFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", fmt.Errorf("token file %s is not valid JSON", s.path)
	}
	if f.Version != 1 {
		return "", fmt.Errorf("token file %s has unsupported version %d", s.path, f.Version)
	}
	if !strings.EqualFold(f.Account, s.account) {
		return "", errTokenAccount
	}
	sealed, err := base64.RawURLEncoding.DecodeString(f.Sealed)
	if err != nil {
		return "", fmt.Errorf("token file %s is damaged", s.path)
	}
	aead, err := s.gcm()
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize() {
		return "", errors.New("token file is truncated")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], s.aad())
	if err != nil {
		return "", errors.New("cannot decrypt the Google token: wrong BRIEF_TOKEN_KEY, or the file was altered")
	}
	return string(plain), nil
}

// writeFileAtomic is the repo's temp-then-rename rule, for the token and for
// every state file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
