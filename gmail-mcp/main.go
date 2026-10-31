// gmail-mcp serves one Gmail account to claude.ai as a remote MCP server, so
// Claude can search, read, draft and send -- including from a verified "Send
// mail as" alias, which the hosted Gmail connector cannot do.
//
// It is the second stack in this repo reachable from the internet, after
// vault-mcp, and it is built the same way on purpose: a resource server only,
// against the same hosted authorization server, published by its own Funnel
// sidecar. See README.md#trust-boundary.
//
// It is deliberately NOT a general Gmail client. There are no tools for
// labels, archive, trash, delete, filters or forwarding, and the Google scopes
// it holds could not do most of those if there were.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0"

type config struct {
	addr          string
	oauthIssuer   string
	oauthResource string
	oauthSubjects []string
	oauthClients  []string

	googleClientID     string
	googleClientSecret string
	account            string
	tokenKey           []byte
	tokenFile          string

	loginAddr     string
	loginRedirect string
}

// loadConfig fails closed. Every value that gates access is required, and a
// blank one refuses to start rather than meaning "no check": a blank that
// silently meant "allow" is how vault-mcp was once wide open on a public
// hostname (vault-mcp/README.md#two-schemes-that-shipped-and-were-removed).
func loadConfig(login bool) (*config, error) {
	c := &config{
		addr:               env("MCP_ADDR", ":8080"),
		oauthIssuer:        strings.TrimSpace(os.Getenv("OAUTH_ISSUER")),
		oauthResource:      strings.TrimSpace(os.Getenv("OAUTH_RESOURCE")),
		googleClientID:     strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID")),
		googleClientSecret: strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET")),
		account:            strings.ToLower(strings.TrimSpace(os.Getenv("GMAIL_ACCOUNT"))),
		tokenFile:          envOr("TOKEN_FILE", "/data/google-token.json"),
		loginAddr:          envOr("LOGIN_ADDR", ":8766"),
		loginRedirect:      envOr("LOGIN_REDIRECT", "http://127.0.0.1:8766/callback"),
	}
	c.oauthSubjects = csv(os.Getenv("OAUTH_ALLOWED_SUBJECTS"))
	c.oauthClients = csv(os.Getenv("OAUTH_ALLOWED_CLIENTS"))

	var missing []string
	need := func(name, v string) {
		if v == "" {
			missing = append(missing, name)
		}
	}
	need("GOOGLE_CLIENT_ID", c.googleClientID)
	need("GOOGLE_CLIENT_SECRET", c.googleClientSecret)
	need("GMAIL_ACCOUNT", c.account)
	if !login {
		// -login never listens publicly and authenticates no one; the
		// resource-server settings are irrelevant to it.
		need("OAUTH_ISSUER", c.oauthIssuer)
		need("OAUTH_RESOURCE", c.oauthResource)
		if len(c.oauthSubjects) == 0 {
			missing = append(missing, "OAUTH_ALLOWED_SUBJECTS")
		}
		if len(c.oauthClients) == 0 {
			missing = append(missing, "OAUTH_ALLOWED_CLIENTS")
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("required and not set: %s", strings.Join(missing, ", "))
	}

	key, err := parseKey(os.Getenv("GMAIL_TOKEN_KEY"))
	if err != nil {
		return nil, err
	}
	c.tokenKey = key

	if !login {
		u, err := url.Parse(c.oauthResource)
		if err != nil || u.Scheme != "https" || u.Path != "/mcp" {
			return nil, fmt.Errorf("OAUTH_RESOURCE must be the connector URL exactly, https://<host>/mcp; got %q", c.oauthResource)
		}
	}
	// A loopback redirect is what keeps Google's callback off the internet.
	// Refusing anything else means a typo cannot quietly register a public one.
	if u, err := url.Parse(c.loginRedirect); err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return nil, fmt.Errorf("LOGIN_REDIRECT must be a loopback http URL; got %q", c.loginRedirect)
	}
	return c, nil
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

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func probeHealth(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = "", strings.TrimPrefix(addr, ":")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

type server struct {
	oauth *oauthVerifier
	tools *tools
	log   *slog.Logger
}

func (s *server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := bearerToken(r); tok != "" {
			sub, err := s.oauth.verify(r.Context(), tok)
			if err == nil {
				next.ServeHTTP(w, r.WithContext(withSubject(r.Context(), sub)))
				return
			}
			s.log.Warn("rejected: invalid access token", "err", err)
		} else {
			s.log.Warn("rejected: no credential")
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

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	health := flag.Bool("healthcheck", false, "probe /healthz on the local listener and exit")
	doLogin := flag.Bool("login", false, "run the one-time Google consent and save the sealed refresh token")
	flag.Parse()

	if *showVersion {
		fmt.Println("gmail-mcp", version)
		return
	}
	if *health {
		os.Exit(probeHealth(env("MCP_ADDR", ":8080")))
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig(*doLogin)
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}
	client, err := newEgressClient(cfg.oauthIssuer)
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}
	google := newGoogleOAuth(cfg.googleClientID, cfg.googleClientSecret, client)
	store := &tokenStore{path: cfg.tokenFile, key: cfg.tokenKey, account: cfg.account}

	if *doLogin {
		if err := runLogin(cfg, google, store, os.Stdin, os.Stdout); err != nil {
			log.Error("login", "err", err)
			os.Exit(1)
		}
		return
	}

	// Refuse to serve without a usable token rather than start and fail every
	// call: the order is -login, then up, and this makes that order obvious.
	if _, err := store.Load(); err != nil {
		log.Error("google token", "err", err, "file", cfg.tokenFile)
		os.Exit(1)
	}

	s := &server{
		oauth: newOAuthVerifier(cfg.oauthIssuer, cfg.oauthResource, cfg.oauthSubjects, cfg.oauthClients, client, log),
		tools: &tools{
			gmail: &gmailClient{base: gmailAPI, http: client, tokens: &accessTokens{oauth: google, store: store}},
			log:   log,
		},
		log: log,
	}

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info("listening", "addr", cfg.addr, "resource", cfg.oauthResource, "subjects", len(cfg.oauthSubjects), "clients", len(cfg.oauthClients))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
