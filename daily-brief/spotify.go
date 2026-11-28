package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Spotify is the four things publish does with Spotify's own CLI,
// save-to-spotify. An interface so the tests can stand in for it.
type Spotify interface {
	Upload(ctx context.Context, file, title, summary, showID string) (episodeURI string, err error)
	WaitReady(ctx context.Context, episodeURI string, wait time.Duration) error
	SetTimeline(ctx context.Context, episodeURI, file string) error
	Delete(ctx context.Context, episodeURI string) error
}

// spotifyCLI runs the binary with `--json` and an environment built from
// scratch.
//
// THE ENVIRONMENT IS THE POINT. This process holds GMAIL_CLIENT_SECRET and
// BRIEF_TOKEN_KEY, and a child inherits its parent's environment by default.
// save-to-spotify is a third-party binary, verified by checksum but not read
// line by line, and it needs neither. Without the key it cannot open the
// sealed Gmail token it shares a directory with, which is what the sealing in
// gmail.go is for on this stack. So it gets the few variables it does need and
// nothing else, and spotify_test.go asserts that.
type spotifyCLI struct {
	bin string
	env []string
}

// spotifyEnvKeys are passed through when set. XDG_CONFIG_HOME is where its
// login lives; the proxy and CA variables only matter on a host that sets
// them.
var spotifyEnvKeys = []string{"PATH", "HOME", "TZ", "XDG_CONFIG_HOME", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE"}

func newSpotifyCLI(bin string, environ []string) *spotifyCLI {
	keep := map[string]bool{}
	for _, k := range spotifyEnvKeys {
		keep[k] = true
	}
	env := []string{
		// Its passive update check would phone home and print a nag on every
		// run; the version is pinned in the Dockerfile instead.
		"SAVE_TO_SPOTIFY_NO_UPDATE_CHECK=1",
	}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if keep[k] {
			env = append(env, kv)
		}
	}
	return &spotifyCLI{bin: bin, env: env}
}

var (
	episodeURIRE = regexp.MustCompile(`^spotify:episode:[A-Za-z0-9]{10,64}$`)
	showIDRE     = regexp.MustCompile(`^[A-Za-z0-9]{10,64}$`)
)

// episodeID is the bare id, for the open.spotify.com link and the commands.
func episodeID(uri string) string { return strings.TrimPrefix(uri, "spotify:episode:") }

func (s *spotifyCLI) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, s.bin, append([]string{"--json"}, args...)...)
	cmd.Env = s.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		// In --json mode a failure is {"error": "..."} on stdout and exit 1.
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(stdout.Bytes(), &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("save-to-spotify %s: %s", args[0], truncate(e.Error, 300))
		}
		return nil, fmt.Errorf("save-to-spotify %s: %v: %s", args[0], err, truncate(strings.TrimSpace(stderr.String()), 300))
	}
	return stdout.Bytes(), nil
}

func (s *spotifyCLI) Upload(ctx context.Context, file, title, summary, showID string) (string, error) {
	out, err := s.run(ctx, "upload", file, "--title", title, "--summary", summary, "--show-id", showID, "--language", "en")
	if err != nil {
		return "", err
	}
	var r struct {
		EpisodeURI string `json:"episode_uri"`
	}
	if err := json.Unmarshal(out, &r); err != nil || !episodeURIRE.MatchString(r.EpisodeURI) {
		return "", errors.New("save-to-spotify upload: no episode URI in its output")
	}
	return r.EpisodeURI, nil
}

func (s *spotifyCLI) WaitReady(ctx context.Context, uri string, wait time.Duration) error {
	out, err := s.run(ctx, "episodes", "status", episodeID(uri), "--wait="+wait.String())
	if err != nil {
		return err
	}
	var r struct {
		Readiness string `json:"readiness"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Readiness != "READY" {
		return fmt.Errorf("episode not ready (%q)", truncate(r.Readiness, 20))
	}
	return nil
}

func (s *spotifyCLI) SetTimeline(ctx context.Context, uri, file string) error {
	_, err := s.run(ctx, "timeline", "set", "--episode-id", episodeID(uri), "--from-file", file)
	return err
}

func (s *spotifyCLI) Delete(ctx context.Context, uri string) error {
	_, err := s.run(ctx, "episodes", "delete", episodeID(uri))
	return err
}

// osEnviron is os.Environ, as a variable for the tests.
var osEnviron = os.Environ
