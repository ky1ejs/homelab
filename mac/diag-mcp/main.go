// mac-diag-mcp serves the mac studio's diagnostics to claude.ai as a remote
// MCP server: disk use by folder, memory and CPU, the CI slots, Docker and
// Cut's databases, and the week's usage trend. Read-only, and it runs
// nothing: every tool reads one file a root daemon writes
// (./mac-diag), so a sizing question needs no one at a terminal.
//
// Built like gmail-mcp and vault-mcp on purpose: a resource server only,
// against the same hosted authorization server (WorkOS AuthKit), published
// with Tailscale Funnel. The differences: it is its own tailnet node, in
// process (tsnet), so the mac studio's own node publishes nothing and no
// port listens on the Mac; and it runs as _macdiag, a user of its own that
// reads only the snapshot and its settings. docs/mac-studio.md,
// "Diagnostics connector", has the trust boundary.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

const version = "0.1.0"

type config struct {
	oauthIssuer   string
	oauthResource string
	oauthSubjects []string
	oauthClients  []string

	snapshot string // the collector's file, read-only to this user
	queue    string // where request_refresh drops a request
	stateDir string // the tailnet node's state, this user's alone
	hostname string // the tailnet node's name: the connector URL's first label
}

// errNotSetUp means `sudo mac-diag login` has not run. main exits 0 on it,
// so launchd's KeepAlive (SuccessfulExit false) leaves the daemon stopped
// until the login starts it, rather than restarting it every 30 seconds.
var errNotSetUp = errors.New("not set up: run `sudo mac-diag login`")

// The settings file's keys. Only these two come from it: they are Kyle's
// (the tenant and his user id), so not in this public repo. Everything else
// comes from the environment ./default.nix sets.
var fileKeys = map[string]bool{"OAUTH_ISSUER": true, "OAUTH_ALLOWED_SUBJECTS": true}

// loadConfig fails closed, as gmail-mcp's does: every value that gates
// access is required, and a blank one refuses to start rather than meaning
// "no check".
func loadConfig(getenv func(string) string, readFile func(string) (map[string]string, error)) (*config, error) {
	c := &config{
		oauthResource: strings.TrimSpace(getenv("OAUTH_RESOURCE")),
		oauthClients:  csv(getenv("OAUTH_ALLOWED_CLIENTS")),
		snapshot:      strings.TrimSpace(getenv("MAC_DIAG_SNAPSHOT")),
		queue:         strings.TrimSpace(getenv("MAC_DIAG_QUEUE")),
		stateDir:      strings.TrimSpace(getenv("MAC_DIAG_STATE_DIR")),
		hostname:      strings.TrimSpace(getenv("MAC_DIAG_HOSTNAME")),
	}
	path := strings.TrimSpace(getenv("MAC_DIAG_CONFIG"))
	if path == "" {
		return nil, errors.New("MAC_DIAG_CONFIG is not set")
	}
	file, err := readFile(path)
	if err != nil {
		return nil, err
	}
	c.oauthIssuer = strings.TrimSpace(file["OAUTH_ISSUER"])
	c.oauthSubjects = csv(file["OAUTH_ALLOWED_SUBJECTS"])

	var missing []string
	need := func(name, v string) {
		if v == "" {
			missing = append(missing, name)
		}
	}
	need("OAUTH_ISSUER", c.oauthIssuer)
	need("OAUTH_RESOURCE", c.oauthResource)
	if len(c.oauthSubjects) == 0 {
		missing = append(missing, "OAUTH_ALLOWED_SUBJECTS")
	}
	if len(c.oauthClients) == 0 {
		missing = append(missing, "OAUTH_ALLOWED_CLIENTS")
	}
	need("MAC_DIAG_SNAPSHOT", c.snapshot)
	need("MAC_DIAG_QUEUE", c.queue)
	need("MAC_DIAG_STATE_DIR", c.stateDir)
	need("MAC_DIAG_HOSTNAME", c.hostname)
	if len(missing) > 0 {
		return nil, fmt.Errorf("required and not set: %s", strings.Join(missing, ", "))
	}
	u, err := url.Parse(c.oauthResource)
	if err != nil || u.Scheme != "https" || u.Path != "/mcp" || !strings.HasPrefix(u.Hostname(), c.hostname+".") {
		return nil, fmt.Errorf("OAUTH_RESOURCE must be the connector URL exactly, https://%s.<tailnet>/mcp; got %q", c.hostname, c.oauthResource)
	}
	return c, nil
}

// readSettings reads KEY=VALUE lines from the file `mac-diag login` writes.
// It must be root's and writable by no one else, as mac-diagnose's mail file
// must: whoever can write it chooses who may call this server.
func readSettings(path string) (map[string]string, error) {
	return readSettingsWith(path, ownedByRoot)
}

// readSettingsWith is readSettings with the owner check passed in, so the
// tests can run it without root.
func readSettingsWith(path string, ownerOK func(os.FileInfo) error) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotSetUp
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := ownerOK(fi); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !fileKeys[k] {
			return nil, fmt.Errorf("%s line %d: only %s may be set here", path, n, "OAUTH_ISSUER and OAUTH_ALLOWED_SUBJECTS")
		}
		out[k] = v
	}
	return out, sc.Err()
}

func ownedByRoot(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read its owner")
	}
	if st.Uid != 0 {
		return errors.New("must be owned by root")
	}
	return modeOK(fi)
}

func modeOK(fi os.FileInfo) error {
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
		return errors.New("must be a file writable by no one but its owner")
	}
	return nil
}

func csv(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

type server struct {
	oauth    *oauthVerifier
	tools    *tools
	log      *slog.Logger
	rejected *rateLog
}

func (s *server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := bearerToken(r); tok != "" {
			sub, err := s.oauth.verify(r.Context(), tok)
			if err == nil {
				next.ServeHTTP(w, r.WithContext(withSubject(r.Context(), sub)))
				return
			}
			s.rejected.warn("rejected: invalid access token", "err", err)
		} else {
			s.rejected.warn("rejected: no credential")
		}
		w.Header().Set("WWW-Authenticate", s.oauth.challenge())
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc(resourceMetadataPath, s.oauth.resourceMetadata)
	mux.HandleFunc(resourceMetadataPath+"/mcp", s.oauth.resourceMetadata)
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.tools.server() },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	mux.Handle("/mcp", s.withAuth(mcpHandler))
	return mux
}

// node is the tailnet node, in this process. Its state, which holds the
// node's key, is in stateDir, which only this user can read.
func node(cfg *config, log *slog.Logger) *tsnet.Server {
	return &tsnet.Server{
		Dir:      cfg.stateDir,
		Hostname: cfg.hostname,
		// tsnet's own log is its backend's, many lines a minute; its
		// UserLogf is the part meant for people, such as a login URL.
		Logf:     func(string, ...any) {},
		UserLogf: func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) },
	}
}

// loggedIn is whether `mac-diag login` has run: tsnet keeps the node's
// state in this file once it has joined.
func loggedIn(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, "tailscaled.state"))
	return err == nil
}

var authKeyRE = regexp.MustCompile(`^tskey-auth-[A-Za-z0-9_-]{8,200}$`)

// tagged is whether the node is tag:<hostname>, the tag the tailnet policy
// grants nothing. A node that joined with a key for another tag, or as a
// person, could reach what they can.
func tagged(st *ipnstate.Status, hostname string) bool {
	if st == nil || st.Self == nil || st.Self.Tags == nil {
		return false
	}
	for _, t := range st.Self.Tags.AsSlice() {
		if t == "tag:"+hostname {
			return true
		}
	}
	return false
}

// checkNode is the two things a joined node must be: named as the
// connector URL, so its certificate matches, and tag:<hostname>.
func checkNode(st *ipnstate.Status, hostname string) (string, error) {
	name := ""
	if st != nil && st.Self != nil {
		name = strings.TrimSuffix(st.Self.DNSName, ".")
	}
	if !strings.HasPrefix(name, hostname+".") {
		return name, fmt.Errorf("the node joined as %q, not %s: an old %s node holds the name; remove it in the Tailscale admin console and log in again", name, hostname, hostname)
	}
	if !tagged(st, hostname) {
		return name, fmt.Errorf("the node is not tag:%s: make the auth key for that tag and log in again", hostname)
	}
	return name, nil
}

// runLogin joins the tailnet with a single-use auth key read from stdin, as
// root's `mac-diag login` hands it over, and prints the node's address. The
// key is used once and never written: the node's state is what stays. Any
// earlier state goes first, because tsnet ignores a key once it has one.
// A node with the wrong name or tag logs out again.
func runLogin(stateDir, hostname string, in io.Reader, out io.Writer) error {
	line, err := bufio.NewReader(io.LimitReader(in, 1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	key := strings.TrimSpace(line)
	if !authKeyRE.MatchString(key) {
		return errors.New("that is not a Tailscale auth key (tskey-auth-...)")
	}
	if err := emptyDir(stateDir); err != nil {
		return fmt.Errorf("clearing the old node's state: %w", err)
	}
	ts := &tsnet.Server{Dir: stateDir, Hostname: hostname, AuthKey: key, Logf: func(string, ...any) {}}
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := ts.Up(ctx)
	if err != nil {
		return fmt.Errorf("joining the tailnet: %w", err)
	}
	name, err := checkNode(st, hostname)
	if err != nil {
		if lc, lerr := ts.LocalClient(); lerr == nil {
			_ = lc.Logout(ctx)
		}
		_ = emptyDir(stateDir)
		return err
	}
	fmt.Fprintln(out, name)
	return nil
}

// emptyDir removes what is in dir, and not dir, which root made.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	doLogin := flag.Bool("login", false, "join the tailnet with an auth key read from stdin, and print the node's name")
	stateDir := flag.String("state-dir", os.Getenv("MAC_DIAG_STATE_DIR"), "with -login: the node's state folder")
	hostname := flag.String("hostname", os.Getenv("MAC_DIAG_HOSTNAME"), "with -login: the node's name")
	flag.Parse()

	if *showVersion {
		fmt.Println("mac-diag-mcp", version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *doLogin {
		if *stateDir == "" || *hostname == "" {
			log.Error("login", "err", "-state-dir and -hostname are required")
			os.Exit(1)
		}
		if err := runLogin(*stateDir, *hostname, os.Stdin, os.Stdout); err != nil {
			log.Error("login", "err", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := loadConfig(os.Getenv, readSettings)
	if errors.Is(err, errNotSetUp) {
		log.Warn("stopped", "reason", err)
		os.Exit(0)
	}
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}
	if !loggedIn(cfg.stateDir) {
		log.Warn("stopped", "reason", errNotSetUp)
		os.Exit(0)
	}
	client, err := newEgressClient(cfg.oauthIssuer)
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ts := node(cfg, log)
	defer ts.Close()
	upCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	st, err := ts.Up(upCtx)
	cancel()
	if err != nil {
		log.Error("tailnet", "err", err)
		os.Exit(1)
	}
	// The connector URL is fixed in ./default.nix, and every token's
	// audience must equal it. A node that came up under another name, such
	// as mac-diag-1 beside a stale mac-diag, would serve an address no token
	// is for; one without its tag could reach the tailnet. Say so and stay
	// stopped until a login fixes it.
	if _, err := checkNode(st, cfg.hostname); err != nil {
		log.Error("stopped", "reason", err.Error()+" (`sudo mac-diag login`)")
		os.Exit(0)
	}
	host, _ := url.Parse(cfg.oauthResource)
	domains := ts.CertDomains()
	if len(domains) == 0 {
		// Not this node's doing: HTTPS certificates are off for the tailnet,
		// or the control server has not sent them yet. launchd tries again.
		log.Error("tailnet", "err", "no certificate domains: turn on HTTPS certificates in the Tailscale admin console's DNS page")
		os.Exit(1)
	}
	if !contains(domains, host.Hostname()) {
		log.Error("stopped: this node's name is not the connector's; remove the old node in the Tailscale admin console, then run `sudo mac-diag login`",
			"want", host.Hostname(), "have", domains)
		os.Exit(0)
	}
	// Funnel only: the tailnet gets no path to this node, so claude.ai's,
	// through Funnel, is the one way in, and the policy needs no grant for it.
	ln, err := ts.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
	if err != nil {
		log.Error("funnel", "err", err)
		os.Exit(1)
	}

	// Anyone can reach the Funnel URL, so neither failed sign-ins nor TLS
	// handshake errors may write a line each.
	rejected := newRateLog(log, time.Minute)
	s := &server{
		oauth:    newOAuthVerifier(cfg.oauthIssuer, cfg.oauthResource, cfg.oauthSubjects, cfg.oauthClients, client, log),
		tools:    &tools{snap: &snapshotFile{path: cfg.snapshot}, queue: cfg.queue, log: log},
		log:      log,
		rejected: rejected,
	}
	httpSrv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          stdlog.New(rejected.writer("http server"), "", 0),
	}
	go func() {
		log.Info("listening", "resource", cfg.oauthResource, "subjects", len(cfg.oauthSubjects), "clients", len(cfg.oauthClients))
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
