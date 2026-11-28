package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/views"
)

func testEnv(over map[string]string) func(string) string {
	env := map[string]string{
		"OAUTH_RESOURCE":        testResource,
		"OAUTH_ALLOWED_CLIENTS": testClient,
		"MAC_DIAG_SNAPSHOT":     "/var/db/mac-diag/snapshot.json",
		"MAC_DIAG_QUEUE":        "/var/db/mac-diag/queue",
		"MAC_DIAG_STATE_DIR":    "/var/db/mac-diag-mcp",
		"MAC_DIAG_HOSTNAME":     "mac-diag",
		"MAC_DIAG_CONFIG":       "/var/db/mac-diag/settings",
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
	// The test's user owns the files, so the owner check is modeOK alone;
	// TestOwnedByRoot checks the owner.
	read := func(p string) (map[string]string, error) { return readSettingsWith(p, modeOK) }
	p := writeSettings(t, "# comment\nOAUTH_ISSUER=https://auth.example.com\n\nOAUTH_ALLOWED_SUBJECTS=user_kyle\n", 0o640)
	m, err := read(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["OAUTH_ISSUER"] != "https://auth.example.com" || m["OAUTH_ALLOWED_SUBJECTS"] != "user_kyle" {
		t.Fatalf("settings = %v", m)
	}

	// The file holds the two values root writes, and nothing that would
	// change where the server listens or what it trusts.
	p = writeSettings(t, "OAUTH_ISSUER=https://auth.example.com\nOAUTH_RESOURCE=https://evil.example/mcp\n", 0o640)
	if _, err := read(p); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("unknown key: err = %v", err)
	}
	for _, mode := range []os.FileMode{0o660, 0o642} {
		p = writeSettings(t, "OAUTH_ISSUER=https://auth.example.com\n", mode)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := read(p); err == nil {
			t.Errorf("mode %o accepted", mode)
		}
	}
	if _, err := read(t.TempDir()); err == nil {
		t.Error("a folder accepted")
	}
}

func TestOwnedByRoot(t *testing.T) {
	p := writeSettings(t, "OAUTH_ISSUER=https://auth.example.com\n", 0o640)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	err = ownedByRoot(fi)
	if os.Geteuid() == 0 && err != nil {
		t.Errorf("root's file refused: %v", err)
	}
	if os.Geteuid() != 0 && err == nil {
		t.Error("a file owned by another user was accepted")
	}
	if _, err := readSettings(p); (err == nil) != (os.Geteuid() == 0) {
		t.Errorf("readSettings: %v", err)
	}
}

func TestCheckNode(t *testing.T) {
	tags := views.SliceOf([]string{"tag:mac-diag"})
	other := views.SliceOf([]string{"tag:mac-studio"})
	for _, c := range []struct {
		name string
		self *ipnstate.PeerStatus
		ok   bool
	}{
		{"right", &ipnstate.PeerStatus{DNSName: "mac-diag.tail0.ts.net.", Tags: &tags}, true},
		{"renamed", &ipnstate.PeerStatus{DNSName: "mac-diag-1.tail0.ts.net.", Tags: &tags}, false},
		{"other tag", &ipnstate.PeerStatus{DNSName: "mac-diag.tail0.ts.net.", Tags: &other}, false},
		{"untagged", &ipnstate.PeerStatus{DNSName: "mac-diag.tail0.ts.net."}, false},
		{"no self", nil, false},
	} {
		name, err := checkNode(&ipnstate.Status{Self: c.self}, "mac-diag")
		if (err == nil) != c.ok {
			t.Errorf("%s: %q %v", c.name, name, err)
		}
	}
}

func TestEmptyDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "tailscaled.state"), []byte("x"), 0o600)
	_ = os.MkdirAll(filepath.Join(dir, "certs", "a"), 0o700)
	if err := emptyDir(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("removed the folder itself: %v", err)
	}
}

func TestRateLog(t *testing.T) {
	var buf strings.Builder
	now := testNow
	r := newRateLog(slog.New(slog.NewTextHandler(&buf, nil)), time.Minute)
	r.now = func() time.Time { return now }
	for range 100 {
		r.warn("rejected: no credential")
	}
	now = now.Add(time.Minute)
	r.warn("rejected: no credential")
	_, _ = r.writer("http server").Write([]byte(strings.Repeat("x", 500) + "\n"))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[1], "held_back=99") {
		t.Errorf("second line: %s", lines[1])
	}
	if len(lines[2]) > 300 {
		t.Errorf("http line is %d bytes", len(lines[2]))
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
