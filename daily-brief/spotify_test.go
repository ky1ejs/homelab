package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCLI writes a shell script standing in for save-to-spotify. It records
// its arguments and environment, one per line, and prints out.
func fakeCLI(t *testing.T, out string, exit int) (bin, record string) {
	t.Helper()
	dir := t.TempDir()
	bin, record = filepath.Join(dir, "save-to-spotify"), filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf 'arg=%s\\n' \"$a\"; done > " + record + "\n" +
		"env | sed 's/^/env=/' >> " + record + "\n" +
		"printf '%s\\n' '" + out + "'\n" +
		"exit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

func recorded(t *testing.T, path string) (args, env []string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if a, ok := strings.CutPrefix(l, "arg="); ok {
			args = append(args, a)
		} else if e, ok := strings.CutPrefix(l, "env="); ok {
			env = append(env, e)
		}
	}
	return args, env
}

// THE TEST THAT MATTERS: the CLI must not see the publisher's secrets.
func TestSpotifyEnvironmentIsFiltered(t *testing.T) {
	bin, record := fakeCLI(t, `{"episode_uri":"spotify:episode:ABCDEFGHIJ12"}`, 0)
	environ := []string{
		"PATH=/usr/bin:/bin", "HOME=/home/app", "TZ=Europe/London", "XDG_CONFIG_HOME=/state/config",
		"BRIEF_TOKEN_KEY=secret-key", "GMAIL_CLIENT_SECRET=secret-client", "GMAIL_CLIENT_ID=cid",
		"CLAUDE_CODE_OAUTH_TOKEN=nope", "SAVE_TO_SPOTIFY_AUTH_TOKEN=injected",
	}
	s := newSpotifyCLI(bin, environ)
	uri, err := s.Upload(context.Background(), "/audio/2026-11-28/episode.mp3", "Sat 28 Nov: -title", "--summary", "SHOWID1234")
	if err != nil || uri != "spotify:episode:ABCDEFGHIJ12" {
		t.Fatalf("Upload = %q, %v", uri, err)
	}
	args, env := recorded(t, record)
	want := []string{"--json", "upload", "/audio/2026-11-28/episode.mp3", "--title", "Sat 28 Nov: -title", "--summary", "--summary", "--show-id", "SHOWID1234", "--language", "en"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %q", args)
	}
	joined := strings.Join(env, "\n")
	for _, secret := range []string{"secret-key", "secret-client", "nope", "injected", "GMAIL_CLIENT_ID"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("the CLI saw %q:\n%s", secret, joined)
		}
	}
	mustContain(t, joined, "XDG_CONFIG_HOME=/state/config")
	mustContain(t, joined, "SAVE_TO_SPOTIFY_NO_UPDATE_CHECK=1")
}

func TestSpotifyUploadRefusesOddURI(t *testing.T) {
	bin, _ := fakeCLI(t, `{"episode_uri":"spotify:episode:../../x"}`, 0)
	if _, err := newSpotifyCLI(bin, []string{"PATH=/usr/bin:/bin"}).Upload(context.Background(), "/a.mp3", "t", "s", "SHOWID1234"); err == nil {
		t.Fatal("accepted a malformed episode URI")
	}
}

func TestSpotifyJSONError(t *testing.T) {
	bin, _ := fakeCLI(t, `{"error":"not logged in"}`, 1)
	err := newSpotifyCLI(bin, []string{"PATH=/usr/bin:/bin"}).Delete(context.Background(), "spotify:episode:ABCDEFGHIJ12")
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("got %v", err)
	}
}

func TestSpotifyWaitAndTimelineArgs(t *testing.T) {
	bin, record := fakeCLI(t, `{"episode_uri":"spotify:episode:ABCDEFGHIJ12","readiness":"READY"}`, 0)
	s := newSpotifyCLI(bin, []string{"PATH=/usr/bin:/bin"})
	if err := s.WaitReady(context.Background(), "spotify:episode:ABCDEFGHIJ12", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	args, _ := recorded(t, record)
	if strings.Join(args, " ") != "--json episodes status ABCDEFGHIJ12 --wait=10m0s" {
		t.Fatalf("args = %q", args)
	}
	if err := s.SetTimeline(context.Background(), "spotify:episode:ABCDEFGHIJ12", "/state/tl.json"); err != nil {
		t.Fatal(err)
	}
	args, _ = recorded(t, record)
	if strings.Join(args, " ") != "--json timeline set --episode-id ABCDEFGHIJ12 --from-file /state/tl.json" {
		t.Fatalf("args = %q", args)
	}

	bin, _ = fakeCLI(t, `{"readiness":"PROCESSING"}`, 0)
	if err := newSpotifyCLI(bin, []string{"PATH=/usr/bin:/bin"}).WaitReady(context.Background(), "spotify:episode:ABCDEFGHIJ12", time.Minute); err == nil {
		t.Fatal("PROCESSING reported as ready")
	}
}

func TestSpotifyEpisodes(t *testing.T) {
	bin, record := fakeCLI(t, `{"episodes":[{"episode_uri":"spotify:episode:ABCDEFGHIJ12","title":"Sat 28 Nov: A"},{"episode_uri":"spotify:episode:../x","title":"Fri 27 Nov: B"},{"title":"no uri"}]}`, 0)
	eps, err := newSpotifyCLI(bin, []string{"PATH=/usr/bin:/bin"}).Episodes(context.Background(), "SHOWID1234")
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0] != (Episode{URI: "spotify:episode:ABCDEFGHIJ12", Title: "Sat 28 Nov: A"}) {
		t.Fatalf("%+v", eps)
	}
	args, _ := recorded(t, record)
	if strings.Join(args, " ") != "--json episodes --show-id SHOWID1234" {
		t.Fatalf("args = %q", args)
	}

	bin, _ = fakeCLI(t, `not json`, 0)
	if _, err := newSpotifyCLI(bin, []string{"PATH=/usr/bin:/bin"}).Episodes(context.Background(), "SHOWID1234"); err == nil {
		t.Fatal("accepted output that is not JSON")
	}
}

func TestSpotifyDeleteGone(t *testing.T) {
	bin, record := fakeCLI(t, `{"error":"API error (404): episode not found"}`, 1)
	err := newSpotifyCLI(bin, []string{"PATH=/usr/bin:/bin"}).Delete(context.Background(), "spotify:episode:ABCDEFGHIJ12")
	if !errors.Is(err, errEpisodeGone) {
		t.Fatalf("got %v", err)
	}
	args, _ := recorded(t, record)
	if strings.Join(args, " ") != "--json episodes delete ABCDEFGHIJ12" {
		t.Fatalf("args = %q", args)
	}
}

func TestParseShowID(t *testing.T) {
	for in, want := range map[string]string{
		"0123456789abcdef":                 "0123456789abcdef",
		" spotify:show:0123456789abcdef\n": "0123456789abcdef",
	} {
		if got, err := parseShowID(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"--new-show", "spotify:episode:0123456789abcdef", "https://open.spotify.com/show/0123456789abcdef", "short"} {
		if _, err := parseShowID(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}
