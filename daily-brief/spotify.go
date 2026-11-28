package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Spotify is what the podcast job does with Spotify's own CLI,
// save-to-spotify. An interface so the tests can stand in for it.
type Spotify interface {
	Upload(ctx context.Context, file, title, summary, showID string) (episodeURI string, err error)
	Episodes(ctx context.Context, showID string) ([]Episode, error)
	WaitReady(ctx context.Context, episodeURI string, wait time.Duration) error
	SetTimeline(ctx context.Context, episodeURI, file string) error
	Delete(ctx context.Context, episodeURI string) error
}

// Episode is one entry of the show's episode list. Only the two fields the
// podcast job matches on are kept.
type Episode struct {
	URI   string
	Title string
}

// errEpisodeGone is Delete's answer when Spotify no longer has the episode,
// for instance because it was deleted by hand.
var errEpisodeGone = errors.New("the episode is no longer on the show")

// spotifyCLI runs the binary with `--json` and an environment of its own.
//
// It runs in brief-podcast, a container of its own, because it is a
// third-party binary, checked by sha256 but not read line by line. A filtered
// environment cannot keep a secret from a child process: the child can read
// its parent's /proc/<pid>/environ, and tini's, as the same user. So the
// container is the boundary, and it holds the Spotify login and nothing else.
// The filter below only keeps the environment to what the CLI needs, so that
// nothing else reaches its logs.
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

// parseShowID takes the show id bare or as the URI `shows create` prints
// (spotify:show:<id>), as the CLI's own --show-id does, and returns it bare.
func parseShowID(raw string) (string, error) {
	id := strings.TrimPrefix(strings.TrimSpace(raw), "spotify:show:")
	if !showIDRE.MatchString(id) {
		return "", errors.New("SPOTIFY_SHOW_ID must be the show's id or its spotify:show: URI, from `save-to-spotify --json shows`")
	}
	return id, nil
}

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

// Episodes lists the show. The CLI asks for it in one request, with no paging.
func (s *spotifyCLI) Episodes(ctx context.Context, showID string) ([]Episode, error) {
	out, err := s.run(ctx, "episodes", "--show-id", showID)
	if err != nil {
		return nil, err
	}
	var r struct {
		Episodes []struct {
			EpisodeURI string `json:"episode_uri"`
			Title      string `json:"title"`
		} `json:"episodes"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, errors.New("save-to-spotify episodes: output is not the expected JSON")
	}
	eps := make([]Episode, 0, len(r.Episodes))
	for _, e := range r.Episodes {
		if episodeURIRE.MatchString(e.EpisodeURI) {
			eps = append(eps, Episode{URI: e.EpisodeURI, Title: e.Title})
		}
	}
	return eps, nil
}

func (s *spotifyCLI) Delete(ctx context.Context, uri string) error {
	_, err := s.run(ctx, "episodes", "delete", episodeID(uri))
	// The CLI reports an HTTP error as "API error (<status>): <body>".
	if err != nil && strings.Contains(err.Error(), "API error (404)") {
		return errEpisodeGone
	}
	return err
}
