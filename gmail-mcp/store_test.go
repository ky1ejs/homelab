package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestTokenStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "google-token.json")
	s := &tokenStore{path: path, key: testKey(t), account: "me@example.com"}

	if _, err := s.Load(); !errors.Is(err, errNoToken) {
		t.Fatalf("empty store: err = %v", err)
	}
	if err := s.Save("refresh-secret", "scope"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "refresh-secret") {
		t.Error("refresh token is on disk in plaintext")
	}
	got, err := s.Load()
	if err != nil || got != "refresh-secret" {
		t.Fatalf("Load = %q, %v", got, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".google-token-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestTokenStoreRefusesTheWrongKeyAccountOrBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "google-token.json")
	key := testKey(t)
	s := &tokenStore{path: path, key: key, account: "me@example.com"}
	if err := s.Save("refresh-secret", "scope"); err != nil {
		t.Fatal(err)
	}

	if _, err := (&tokenStore{path: path, key: testKey(t), account: "me@example.com"}).Load(); err == nil {
		t.Error("wrong key decrypted the token")
	}
	if _, err := (&tokenStore{path: path, key: key, account: "other@example.com"}).Load(); !errors.Is(err, errTokenAccount) {
		t.Errorf("other account: err = %v", err)
	}

	// Editing the account field to match does not help: it is bound in as
	// additional authenticated data.
	var f tokenFile
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &f)
	f.Account = "other@example.com"
	edited, _ := json.Marshal(f)
	_ = os.WriteFile(path, edited, 0o600)
	if _, err := (&tokenStore{path: path, key: key, account: "other@example.com"}).Load(); err == nil {
		t.Error("a token re-labelled for another account decrypted")
	}
}

func TestParseKey(t *testing.T) {
	for _, bad := range []string{"", "short", strings.Repeat("a", 63)} {
		if _, err := parseKey(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := parseKey(strings.Repeat("ab", 32)); err != nil {
		t.Errorf("hex key rejected: %v", err)
	}
	if _, err := parseKey("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); err != nil {
		t.Errorf("base64 key rejected: %v", err)
	}
}
