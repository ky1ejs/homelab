package main

import (
	"strings"
	"testing"
	"time"
)

func goodEnv() map[string]string {
	return map[string]string{
		"GMAIL_ACCOUNT":       "You@Example.com",
		"BRIEF_TO":            "you@example.com",
		"GMAIL_CLIENT_ID":     "cid",
		"GMAIL_CLIENT_SECRET": "secret",
		"BRIEF_TOKEN_KEY":     strings.Repeat("ab", 32),
	}
}

func getenvFrom(m map[string]string) func(string, string) string {
	return func(k, def string) string {
		if v := m[k]; v != "" {
			return v
		}
		return def
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(getenvFrom(goodEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.account != "you@example.com" || c.from != c.account || c.showID != "" ||
		c.podcastWait != 4*time.Hour || c.retentionDays != 21 || c.lookbackDays != 2 || c.stateDir != "/state" {
		t.Fatalf("%+v", c)
	}
}

func TestLoadConfigRefuses(t *testing.T) {
	for k, v := range map[string]string{
		"GMAIL_ACCOUNT":   "You <you@example.com>",
		"BRIEF_TO":        "a@b.co, c@d.co",
		"BRIEF_TOKEN_KEY": "short",
		"SPOTIFY_SHOW_ID": "--new-show",
		"PODCAST_WAIT":    "forever",
		"RETENTION_DAYS":  "-1",
		"LOOKBACK_DAYS":   "30",
		"HC_PING_URL":     "http://hc-ping.com/x",
	} {
		e := goodEnv()
		e[k] = v
		if _, err := loadConfig(getenvFrom(e)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%q: got %v", k, v, err)
		}
	}
	e := goodEnv()
	e["BRIEF_TO"] = "you@example.com\r\nBcc: x@y.z"
	if _, err := loadConfig(getenvFrom(e)); err == nil {
		t.Error("accepted a header break in BRIEF_TO")
	}
}
