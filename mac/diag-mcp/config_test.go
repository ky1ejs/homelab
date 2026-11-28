package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testEnv(over map[string]string) func(string) string {
	env := map[string]string{
		"OAUTH_RESOURCE":        testResource,
		"OAUTH_ALLOWED_CLIENTS": testClient,
		"MAC_DIAG_SNAPSHOT":     "/var/db/mac-diag/snapshot.json",
		"MAC_DIAG_QUEUE":        "/var/db/mac-diag/queue",
		"MAC_DIAG_STATE_DIR":    "/var/db/mac-diag-mcp",
		"MAC_DIAG_HOSTNAME":     "mac-diag",
		"MAC_DIAG_CONFIG":       "/etc/homelab/diag-mcp",
	}
	for k, v := range over {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

func testFile(over map[string]string) func(string) (map[string]string, error) {
	file := map[string]string{"OAUTH_ISSUER": "https://auth.example.com", "OAUTH_ALLOWED_SUBJECTS": "user_kyle"}
	for k, v := range over {
		file[k] = v
	}
	return func(string) (map[string]string, error) { return file, nil }
}

func TestLoadConfig(t *testing.T) {
	c, err := loadConfig(testEnv(nil), testFile(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.oauthIssuer != "https://auth.example.com" || len(c.oauthSubjects) != 1 || c.hostname != "mac-diag" {
		t.Fatalf("config = %+v", c)
	}
}

// Every value is needed, and a missing one stops the server rather than
// leaving a check off.
func TestLoadConfigFailsClosed(t *testing.T) {
	for _, k := range []string{"OAUTH_RESOURCE", "OAUTH_ALLOWED_CLIENTS", "MAC_DIAG_SNAPSHOT", "MAC_DIAG_QUEUE", "MAC_DIAG_STATE_DIR", "MAC_DIAG_HOSTNAME"} {
		if _, err := loadConfig(testEnv(map[string]string{k: ""}), testFile(nil)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s unset: err = %v", k, err)
		}
	}
	for _, k := range []string{"OAUTH_ISSUER", "OAUTH_ALLOWED_SUBJECTS"} {
		if _, err := loadConfig(testEnv(nil), testFile(map[string]string{k: " "})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s unset: err = %v", k, err)
		}
	}
	if _, err := loadConfig(testEnv(map[string]string{"MAC_DIAG_CONFIG": ""}), testFile(nil)); err == nil {
		t.Error("MAC_DIAG_CONFIG unset: no error")
	}
	notSetUp := func(string) (map[string]string, error) { return nil, errNotSetUp }
	if _, err := loadConfig(testEnv(nil), notSetUp); !errors.Is(err, errNotSetUp) {
		t.Errorf("no settings file: err = %v, want errNotSetUp", err)
	}
}

// The resource is the audience every token is checked against, so it must be
// this node's own connector URL.
func TestLoadConfigResource(t *testing.T) {
	for _, r := range []string{
		"http://mac-diag.example.ts.net/mcp",
		"https://mac-diag.example.ts.net/",
		"https://mac-diag.example.ts.net/mcp/x",
		"https://gmail-mcp.example.ts.net/mcp",
		"https://mac-diagx.example.ts.net/mcp",
	} {
		if _, err := loadConfig(testEnv(map[string]string{"OAUTH_RESOURCE": r}), testFile(nil)); err == nil {
			t.Errorf("resource %q accepted", r)
		}
	}
}

func writeSettings(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "diag-mcp")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadSettingsMissing(t *testing.T) {
	if _, err := readSettings(filepath.Join(t.TempDir(), "none")); !errors.Is(err, errNotSetUp) {
		t.Fatalf("err = %v, want errNotSetUp", err)
	}
}

func TestReadSettings(t *testing.T) {
	if os.Geteuid() != 0 {
		// Owned by the test's user, so ownedByRoot refuses it, which
		// TestReadSettingsNotRoot checks.
		t.Skip("needs root to make a root-owned file")
	}
	p := writeSettings(t, "# comment\nOAUTH_ISSUER=https://auth.example.com\n\nOAUTH_ALLOWED_SUBJECTS=user_kyle\n", 0o640)
	m, err := readSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["OAUTH_ISSUER"] != "https://auth.example.com" || m["OAUTH_ALLOWED_SUBJECTS"] != "user_kyle" {
		t.Fatalf("settings = %v", m)
	}

	// The file holds the two values root writes, and nothing that would
	// change where the server listens or what it trusts.
	p = writeSettings(t, "OAUTH_ISSUER=https://auth.example.com\nOAUTH_RESOURCE=https://evil.example/mcp\n", 0o640)
	if _, err := readSettings(p); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("unknown key: err = %v", err)
	}
	p = writeSettings(t, "OAUTH_ISSUER=https://auth.example.com\n", 0o660)
	if _, err := readSettings(p); err == nil {
		t.Fatal("group-writable file accepted")
	}
}

func TestReadSettingsNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, so the file is root's")
	}
	p := writeSettings(t, "OAUTH_ISSUER=https://auth.example.com\n", 0o640)
	if _, err := readSettings(p); err == nil {
		t.Fatal("a file owned by another user was accepted")
	}
}

func TestRunLoginRejectsNonKey(t *testing.T) {
	var out strings.Builder
	for _, in := range []string{"", "hunter2\n", "tskey-api-abcdefgh12345\n", "tskey-auth-x\n", "tskey-auth-abcdefgh12345 rm -rf\n"} {
		if err := runLogin(t.TempDir(), "mac-diag", strings.NewReader(in), &out); err == nil {
			t.Errorf("login accepted %q", in)
		}
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}
