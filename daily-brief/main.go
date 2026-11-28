// Command daily-brief is the Go half of the daily-brief stack: it checks the
// agent's issue, uploads each day's podcast and emails each day's brief.
//
//	daily-brief validate FILE DATE   check an issue; exit 1 with the problems
//	daily-brief podcast              one tick of the upload job (brief-podcast)
//	daily-brief publish              one tick of the email job (brief-publisher)
//	daily-brief gmail-login          the one-time Google consent
//	daily-brief version
//
// Standard library only, like every stack here that holds a credential: the
// CI job fails the build if go.mod gains a require.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // TZ must resolve even if the image's zoneinfo is missing
)

var version = "dev"

// config is the publisher's. The podcast job has its own, podConfig, and
// neither reads the other's variables: compose gives each container only its
// own.
type config struct {
	issuesDir, stateDir, podcastDays string

	account, from, to      string
	clientID, clientSecret string
	tokenKey               []byte
	podcasts               bool
	podcastWait            time.Duration
	lookbackDays           int
	hcPingURL              string
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

// loadConfig reads the publisher's environment. Everything that ends up in a
// mail header or a command line is checked here, once.
func loadConfig(getenv func(string, string) string) (*config, error) {
	c := &config{
		issuesDir:   getenv("ISSUES_DIR", "/issues"),
		stateDir:    getenv("STATE_DIR", "/state"),
		podcastDays: getenv("PODCAST_DIR", "/podcast") + "/days",
	}
	var errs []error
	addr := func(name string) string {
		raw := strings.TrimSpace(getenv(name, ""))
		a, err := mail.ParseAddress(raw)
		if err != nil || a.Name != "" || a.Address != raw {
			errs = append(errs, fmt.Errorf("%s must be one bare email address", name))
			return ""
		}
		return strings.ToLower(a.Address)
	}
	c.account = addr("GMAIL_ACCOUNT")
	c.from = c.account
	c.to = addr("BRIEF_TO")
	c.clientID = getenv("GMAIL_CLIENT_ID", "")
	c.clientSecret = getenv("GMAIL_CLIENT_SECRET", "")
	if c.clientID == "" || c.clientSecret == "" {
		errs = append(errs, errors.New("GMAIL_CLIENT_ID and GMAIL_CLIENT_SECRET are required"))
	}
	key, err := parseKey(getenv("BRIEF_TOKEN_KEY", ""))
	if err != nil {
		errs = append(errs, err)
	}
	c.tokenKey = key

	// The publisher never talks to Spotify. A show id only says that the
	// email should wait for the podcast job.
	if raw := getenv("SPOTIFY_SHOW_ID", ""); strings.TrimSpace(raw) != "" {
		if _, err := parseShowID(raw); err != nil {
			errs = append(errs, err)
		}
		c.podcasts = true
	}
	if c.podcastWait, err = time.ParseDuration(getenv("PODCAST_WAIT", "4h")); err != nil || c.podcastWait < 0 || c.podcastWait > 24*time.Hour {
		errs = append(errs, errors.New("PODCAST_WAIT must be a duration from 0 to 24h, like 4h"))
	}
	c.lookbackDays = intVar(getenv, &errs, "LOOKBACK_DAYS", "2", 1, 7)

	if c.hcPingURL = strings.TrimSpace(getenv("HC_PING_URL", "")); c.hcPingURL != "" {
		if u, err := url.Parse(c.hcPingURL); err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, errors.New("HC_PING_URL must be an https URL"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// loadPodConfig reads the podcast job's environment. It holds no Gmail
// setting, and the podcast container is given none.
func loadPodConfig(getenv func(string, string) string) (*podConfig, error) {
	c := &podConfig{
		issuesDir:  getenv("ISSUES_DIR", "/issues"),
		audioDir:   getenv("AUDIO_DIR", "/audio"),
		podcastDir: getenv("PODCAST_DIR", "/podcast"),
		spotifyBin: getenv("SAVE_TO_SPOTIFY_BIN", "/usr/local/bin/save-to-spotify"),
	}
	var errs []error
	if raw := getenv("SPOTIFY_SHOW_ID", ""); strings.TrimSpace(raw) != "" {
		id, err := parseShowID(raw)
		if err != nil {
			errs = append(errs, err)
		}
		c.showID = id
	}
	// Spotify's Free tier keeps 30 episodes per show; 21 leaves room for the
	// days the job is down and catches up.
	c.retentionDays = intVar(getenv, &errs, "RETENTION_DAYS", "21", 0, 365)
	c.lookbackDays = intVar(getenv, &errs, "LOOKBACK_DAYS", "2", 1, 7)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

func intVar(getenv func(string, string) string, errs *[]error, name, def string, lo, hi int) int {
	n, err := strconv.Atoi(getenv(name, def))
	if err != nil || n < lo || n > hi {
		*errs = append(*errs, fmt.Errorf("%s must be a whole number from %d to %d", name, lo, hi))
	}
	return n
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "validate":
		if len(os.Args) != 4 {
			usage(os.Stderr)
			os.Exit(2)
		}
		err = cmdValidate(os.Args[2], os.Args[3], os.Stdout)
	case "podcast":
		err = cmdPodcast(ctx)
	case "publish":
		err = cmdPublish(ctx)
	case "gmail-login":
		err = cmdLogin()
	case "version":
		fmt.Println(version)
	default:
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		log.Printf("daily-brief %s: %v", os.Args[1], err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: daily-brief validate FILE DATE | podcast | publish | gmail-login | version")
}

// cmdValidate prints the problems one per line on failure, which is what
// write.sh hands back to the agent for its fix-up run.
func cmdValidate(path, date string, out io.Writer) error {
	if !validDate(date) {
		return errors.New("DATE must be YYYY-MM-DD")
	}
	_, err := LoadIssue(path, date)
	var ve *ValidationError
	if errors.As(err, &ve) {
		for _, p := range ve.Problems {
			fmt.Fprintln(out, p)
		}
		return errors.New("issue is not valid")
	}
	return err
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

func cmdPublish(ctx context.Context) error {
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	hc := newHTTPClient()
	p := &publisher{
		cfg:    cfg,
		mailer: &gmailMailer{oauth: newGoogleOAuth(cfg.clientID, cfg.clientSecret, hc), store: tokenStoreFor(cfg)},
		http:   hc,
		now:    time.Now,
		log:    log.New(os.Stderr, "", 0),
	}
	return p.Tick(ctx)
}

func cmdPodcast(ctx context.Context) error {
	cfg, err := loadPodConfig(env)
	if err != nil {
		return err
	}
	if cfg.showID == "" {
		return errors.New("SPOTIFY_SHOW_ID is blank, so there is no show to upload to")
	}
	p := &podcaster{
		cfg:     cfg,
		spotify: newSpotifyCLI(cfg.spotifyBin, os.Environ()),
		now:     time.Now,
		log:     log.New(os.Stderr, "", 0),
	}
	return p.Tick(ctx)
}

func cmdLogin() error {
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	return runLogin(cfg.account, newGoogleOAuth(cfg.clientID, cfg.clientSecret, newHTTPClient()), tokenStoreFor(cfg), os.Stdin, os.Stdout)
}

func tokenStoreFor(cfg *config) *tokenStore {
	return &tokenStore{path: cfg.stateDir + "/google-token.json", key: cfg.tokenKey, account: cfg.account}
}
