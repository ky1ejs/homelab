package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The one secret this server keeps on disk: the Google refresh token that
// -login obtained.
//
// It is sealed with AES-256-GCM under GMAIL_TOKEN_KEY, which lives in .env in
// the checkout, while the sealed file lives in the data directory on the
// encrypted volume. The two are deliberately in different places so that a
// copy of one -- a stray backup of the data directory, say -- is not a
// credential by itself. Both being on the same NAS means this is not defence
// against someone who owns the NAS; nothing here could be.
//
// The account is bound in as additional authenticated data, so a token sealed
// for one mailbox cannot be presented as another's by editing the JSON.

var (
	errNoToken      = errors.New("no Google token yet: run `gmail-mcp -login` once")
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
		return nil, errors.New("GMAIL_TOKEN_KEY is empty")
	}
	var key []byte
	var err error
	if len(raw) == 64 {
		key, err = hex.DecodeString(raw)
	} else {
		key, err = base64.StdEncoding.DecodeString(raw)
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("GMAIL_TOKEN_KEY must be 32 bytes, as base64 or hex (openssl rand -base64 32)")
	}
	return key, nil
}

func (s *tokenStore) aad() []byte {
	return []byte("gmail-mcp/google-refresh-token/v1/" + s.account)
}

func (s *tokenStore) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Save writes temp-then-rename, mode 0600, the same atomic-write rule the rest
// of this repo follows: a crash mid-write leaves the old token, never half of
// a new one.
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
		Account: s.account,
		Scope:   scope,
		Sealed:  base64.RawURLEncoding.EncodeToString(sealed),
		SavedAt: time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".google-token-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
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
	return os.Rename(tmp.Name(), s.path)
}

// Load returns the refresh token, or errNoToken when -login has not run.
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
		return "", fmt.Errorf("token file %s is not valid JSON: %w", s.path, err)
	}
	if f.Version != 1 {
		return "", fmt.Errorf("token file %s has unsupported version %d", s.path, f.Version)
	}
	if !strings.EqualFold(f.Account, s.account) {
		return "", errTokenAccount
	}
	sealed, err := base64.RawURLEncoding.DecodeString(f.Sealed)
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", s.path, err)
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
		return "", errors.New("cannot decrypt the Google token: wrong GMAIL_TOKEN_KEY, or the file was altered")
	}
	return string(plain), nil
}

// Remove deletes the token, which is what happens when Google says it is no
// longer valid: a dead credential left on disk only invites confusion.
func (s *tokenStore) Remove() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
