package main

import (
	"strings"
	"testing"
)

func setGoodEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"GOOGLE_CLIENT_ID":       "id",
		"GOOGLE_CLIENT_SECRET":   "secret",
		"GMAIL_ACCOUNT":          "Me@Example.com",
		"GMAIL_TOKEN_KEY":        strings.Repeat("ab", 32),
		"OAUTH_ISSUER":           "https://auth.example.com",
		"OAUTH_RESOURCE":         "https://gmail-mcp.example.ts.net/mcp",
		"OAUTH_ALLOWED_SUBJECTS": " user_kyle , ",
		"OAUTH_ALLOWED_CLIENTS":  " https://claude.ai/oauth/mcp-oauth-client-metadata , client_other",
		"LOGIN_REDIRECT":         "",
	} {
		t.Setenv(k, v)
	}
}

func TestLoadConfig(t *testing.T) {
	setGoodEnv(t)
	c, err := loadConfig(false)
	if err != nil {
		t.Fatal(err)
	}
	if c.account != "me@example.com" || len(c.oauthSubjects) != 1 || c.oauthSubjects[0] != "user_kyle" || len(c.oauthClients) != 2 || c.oauthClients[0] != "https://claude.ai/oauth/mcp-oauth-client-metadata" {
		t.Errorf("config = %+v", c)
	}
	if c.loginRedirect != "http://127.0.0.1:8765/callback" || c.tokenFile != "/data/google-token.json" {
		t.Errorf("defaults = %q, %q", c.loginRedirect, c.tokenFile)
	}
}

// The real config path, fail closed: every blank or malformed gating value
// refuses to start rather than quietly meaning "no check".
func TestLoadConfigFailsClosed(t *testing.T) {
	cases := map[string][2]string{
		"blank subjects":    {"OAUTH_ALLOWED_SUBJECTS", " , "},
		"blank clients":     {"OAUTH_ALLOWED_CLIENTS", " , "},
		"blank issuer":      {"OAUTH_ISSUER", ""},
		"blank resource":    {"OAUTH_RESOURCE", ""},
		"resource not /mcp": {"OAUTH_RESOURCE", "https://gmail-mcp.example.ts.net/"},
		"resource http":     {"OAUTH_RESOURCE", "http://gmail-mcp.example.ts.net/mcp"},
		"blank account":     {"GMAIL_ACCOUNT", ""},
		"short key":         {"GMAIL_TOKEN_KEY", "abcd"},
		"public redirect":   {"LOGIN_REDIRECT", "https://gmail-mcp.example.ts.net/callback"},
	}
	for name, kv := range cases {
		setGoodEnv(t)
		t.Setenv(kv[0], kv[1])
		if _, err := loadConfig(false); err == nil {
			t.Errorf("%s: started", name)
		}
	}
}

func TestLoginNeedsNoResourceServerSettings(t *testing.T) {
	setGoodEnv(t)
	t.Setenv("OAUTH_ISSUER", "")
	t.Setenv("OAUTH_RESOURCE", "")
	t.Setenv("OAUTH_ALLOWED_SUBJECTS", "")
	t.Setenv("OAUTH_ALLOWED_CLIENTS", "")
	if _, err := loadConfig(true); err != nil {
		t.Errorf("login refused: %v", err)
	}
	if _, err := loadConfig(false); err == nil {
		t.Error("serve started without resource-server settings")
	}
}
