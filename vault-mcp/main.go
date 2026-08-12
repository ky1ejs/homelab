// vault-mcp serves the Obsidian vault to Claude's voice mode as a remote MCP
// server.
//
// Why this exists at all: Claude Code Remote Control is the surface for real
// work, but the phone app's voice mode cannot drive it. Voice mode can call
// custom connectors, and connector traffic is routed through Anthropic's cloud
// — so unlike everything else in this repo, this component needs a publicly
// reachable HTTPS endpoint. See README.md#trust-boundary and
// obsidian-vault/DECISIONS.md#options-assessed.
//
// It is deliberately NOT a general file server. It cannot delete, cannot
// overwrite a note wholesale, and cannot touch .claude/, AGENTS.md or CLAUDE.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0"

type config struct {
	addr        string
	vaultDir    string
	snapshotDir string
	captureNote string

	allowNoAuth bool

	// Hosted authorization server. Empty issuer disables the OAuth path
	// entirely, which is how this shipped before and how it stays testable
	// without a network dependency.
	oauthIssuer   string
	oauthResource string

	// Which WorkOS user ids may use this server, and the explicit override that
	// says every account on the tenant may.
	oauthSubjects   []string
	oauthAnySubject bool

	clientIPHeader string
	allowedNets    []*net.IPNet

	excludes []string

	snapshot    bool
	authorName  string
	authorEmail string
	lockTimeout time.Duration
}

func loadConfig() (*config, error) {
	c := &config{
		addr:           env("MCP_ADDR", ":8080"),
		vaultDir:       env("VAULT_DIR", "/vault"),
		snapshotDir:    env("SNAPSHOT_DIR", "/snapshots"),
		captureNote:    env("CAPTURE_NOTE", "Inbox.md"),
		clientIPHeader: env("MCP_CLIENT_IP_HEADER", "CF-Connecting-IP"),
		authorName:     env("VOICE_GIT_NAME", "Claude Voice"),
		authorEmail:    env("VOICE_GIT_EMAIL", "voice@vault.local"),
		snapshot:       env("MCP_SNAPSHOT", "1") == "1",
	}

	// OAuth 2.1 against a hosted authorization server, and nothing else.
	//
	// Two shared-secret schemes preceded this and are gone on purpose: a static
	// header token, and the same secret carried in the connector URL because
	// claude.ai offers no header field on a personal account. Both worked. Both
	// left a credential that never expired, could not be revoked, and had to
	// stay correct in code nobody exercised -- which is exactly where the one
	// real hole in this server was found. See README.md#how-this-authenticates.
	c.oauthIssuer = env("OAUTH_ISSUER", "")
	c.oauthResource = env("OAUTH_RESOURCE", "")
	if c.oauthIssuer != "" && c.oauthResource == "" {
		return nil, errors.New("OAUTH_ISSUER is set but OAUTH_RESOURCE is empty; it must equal the connector URL exactly")
	}
	if c.oauthIssuer == "" {
		if os.Getenv("MCP_ALLOW_NO_AUTH") != "1" {
			return nil, errors.New("OAUTH_ISSUER is not set (MCP_ALLOW_NO_AUTH=1 only for local testing)")
		}
		// Local testing only. Refusing to start without this being explicit is
		// the point: an unauthenticated start that "worked" would be a vault
		// readable by anyone who found the hostname.
		c.allowNoAuth = true
	}

	// Which subjects may use this server.
	//
	// A token that passes signature, issuer, expiry and audience proves only
	// that the authorization server issued it -- to *someone*. If the AuthKit
	// tenant permits self-service sign-up, that someone is anybody who finds the
	// hostname, and the hostname is public: Funnel needs a Let's Encrypt
	// certificate and every certificate is published to Certificate
	// Transparency. So the allow list, not the sign-up toggle, is what keeps
	// this vault yours.
	for _, part := range strings.Split(env("OAUTH_ALLOWED_SUBJECTS", ""), ",") {
		if part = strings.TrimSpace(part); part != "" {
			c.oauthSubjects = append(c.oauthSubjects, part)
		}
	}
	c.oauthAnySubject = os.Getenv("OAUTH_ALLOW_ANY_SUBJECT") == "1"
	if c.oauthIssuer != "" && len(c.oauthSubjects) == 0 && !c.oauthAnySubject {
		// Refusing to start is the whole point, and it is the same judgement as
		// MCP_ALLOW_NO_AUTH above: a blank value must never be the permissive
		// one. Before this check existed the server started happily and honoured
		// every account on the tenant.
		return nil, errors.New("OAUTH_ALLOWED_SUBJECTS is empty; set it to your WorkOS user id (OAUTH_ALLOW_ANY_SUBJECT=1 to honour every account on the tenant)")
	}

	// Off by default, because the deployed path is Tailscale Funnel and Funnel
	// does not forward the caller's public IP — every request would be rejected
	// for having no client address. Kept because it is the right control behind
	// any proxy that does provide one (Cloudflare's CF-Connecting-IP), where
	// Anthropic's published egress range is 160.79.104.0/21.
	//
	// Only meaningful when this listener is unreachable except through that
	// proxy, since the header is otherwise trivially forged.
	if raw := env("MCP_ALLOWED_CIDRS", ""); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			_, n, err := net.ParseCIDR(part)
			if err != nil {
				return nil, fmt.Errorf("MCP_ALLOWED_CIDRS %q: %w", part, err)
			}
			c.allowedNets = append(c.allowedNets, n)
		}
	}

	// Folders this connector cannot see. The reason is asymmetry, not secrecy:
	// vault-claude reads the whole vault and has Bash and the web tools denied,
	// so an injected clipping there has nothing to exfiltrate through. The client
	// on this end is claude.ai, whose tool surface is not ours to configure and
	// does have web access — so the leg of the trifecta we can remove here is the
	// untrusted content, not the egress. See README.md#what-voice-cannot-see.
	for _, part := range strings.Split(env("MCP_EXCLUDE", ""), ",") {
		if strings.TrimSpace(part) != "" {
			c.excludes = append(c.excludes, part)
		}
	}

	secs, err := strconv.Atoi(env("SNAPSHOT_LOCK_TIMEOUT", "120"))
	if err != nil {
		return nil, fmt.Errorf("SNAPSHOT_LOCK_TIMEOUT: %w", err)
	}
	c.lockTimeout = time.Duration(secs) * time.Second

	return c, nil
}

// probeHealth backs `vault-mcp -healthcheck`, so the container healthcheck needs
// no curl in the image and no copy of the token.
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

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

type server struct {
	cfg   *config
	vault *Vault
	snaps *Snapshotter
	oauth *oauthVerifier // nil when OAUTH_ISSUER is unset
	log   *slog.Logger
}

func main() {
	// -version gives the Dockerfile something to assert on, so a broken binary
	// fails the build rather than producing a container that starts and does
	// nothing. -healthcheck lets compose probe the server using this binary
	// instead of adding curl to the runtime image.
	showVersion := flag.Bool("version", false, "print version and exit")
	health := flag.Bool("healthcheck", false, "probe /healthz on the local listener and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("vault-mcp", version)
		return
	}
	if *health {
		os.Exit(probeHealth(env("MCP_ADDR", ":8080")))
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig()
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}
	vault, err := NewVault(cfg.vaultDir, cfg.excludes)
	if err != nil {
		log.Error("vault", "err", err)
		os.Exit(1)
	}

	// Fatal: a capture note this server may not write is a connector whose main
	// tool fails on first use, and the first use will be from the car. The
	// same check catches CAPTURE_NOTE pointing at CLAUDE.md or at a .txt.
	if err := vault.CheckWritable(cfg.captureNote); err != nil {
		log.Error("CAPTURE_NOTE is not writable through this server",
			"note", cfg.captureNote, "err", err)
		os.Exit(1)
	}

	// Warning, not fatal: naming a folder before creating it in Obsidian is
	// legitimate. But an exclusion matching nothing protects nothing while every
	// other check still passes, which is the silent failure worth one line of log.
	if missing := vault.MissingExcludes(); len(missing) > 0 {
		log.Warn("MCP_EXCLUDE entries match nothing in the vault — they are protecting nothing",
			"entries", strings.Join(missing, ", "))
	}

	s := &server{
		cfg:   cfg,
		vault: vault,
		log:   log,
		snaps: NewSnapshotter(cfg.snapshotDir, cfg.vaultDir, cfg.authorName, cfg.authorEmail, cfg.lockTimeout, log),
	}
	if cfg.oauthIssuer != "" {
		s.oauth = newOAuthVerifier(cfg.oauthIssuer, cfg.oauthResource, cfg.oauthSubjects, cfg.oauthAnySubject, log)
	}

	mux := http.NewServeMux()
	// Unauthenticated on purpose: it reveals nothing and lets the container
	// healthcheck run without holding a copy of the token.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Discovery for the OAuth flow. Unauthenticated on purpose: a client that
	// cannot read this cannot begin the flow. Both paths are served because
	// Anthropic's client probes the path-suffixed form first.
	if s.oauth != nil {
		mux.HandleFunc(resourceMetadataPath, s.oauth.resourceMetadata)
		mux.HandleFunc(resourceMetadataPath+"/mcp", s.oauth.resourceMetadata)
	}

	// Stateless + JSON responses: the 2026-07-28 spec moved MCP to a
	// request/response model, and a tunnel is a much better fit for discrete
	// POSTs than for a long-lived SSE stream it may buffer or time out.
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcpServer() },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	// One route, one credential scheme. The endpoint an internet scanner finds
	// by guessing the hostname reveals nothing but a 401 and a pointer to the
	// authorization server.
	mux.Handle("/mcp", s.withAuth(s.withAllowedIP(handler)))

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening",
			"addr", cfg.addr,
			"vault", cfg.vaultDir,
			"capture_note", cfg.captureNote,
			"excluded", strings.Join(vault.Excludes(), ", "),
			"snapshot", cfg.snapshot,
			"cidrs", len(cfg.allowedNets),
			"oauth", cfg.oauthIssuer != "",
		)
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

// withAuth compares the configured header in constant time. A mismatch and a
// missing header return the same 401 with no detail — a error that distinguishes
// them is an oracle.
func (s *server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An access token from the authorization server.
		if s.oauth != nil {
			if tok := bearerToken(r); tok != "" {
				if sub, err := s.oauth.verify(r.Context(), tok); err == nil {
					// Carried to the tool handlers so each audit line names the
					// subject the token was issued to.
					next.ServeHTTP(w, r.WithContext(withSubject(r.Context(), sub)))
					return
				} else {
					// The error names the issuer and audience, never the token.
					s.log.Warn("rejected: invalid access token", "ip", s.clientIP(r), "err", err)
				}
			}
		}

		// Nothing configured at all used to mean "pass through", which made this
		// route wide open whenever the credential lived in the URL instead of a
		// header — a public, unauthenticated endpoint that every unit test still
		// passed. Only an explicit MCP_ALLOW_NO_AUTH may open it.
		if s.oauth == nil && s.cfg.allowNoAuth {
			next.ServeHTTP(w, r)
			return
		}

		// The challenge is what starts the OAuth flow in a compliant client, and
		// it only means anything on a 401.
		if s.oauth != nil {
			w.Header().Set("WWW-Authenticate", s.oauth.challenge())
		}
		// Deliberately does NOT log the path. Once a secret can live in the URL,
		// logging the path writes near-miss credentials into the container log,
		// which is the one place they must never appear.
		s.log.Warn("rejected: no valid credential", "ip", s.clientIP(r))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// withAllowedIP is a no-op unless MCP_ALLOWED_CIDRS is set, which it is not on
// the Funnel path. Where it is set, an absent header is refused rather than
// waved through: absent means the request did not arrive the way we expect.
func (s *server) withAllowedIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.allowedNets) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		raw := s.clientIP(r)
		ip := net.ParseIP(raw)
		if ip == nil {
			s.log.Warn("rejected: no client ip", "header", s.cfg.clientIPHeader)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		for _, n := range s.cfg.allowedNets {
			if n.Contains(ip) {
				next.ServeHTTP(w, r)
				return
			}
		}
		s.log.Warn("rejected: ip outside allowlist", "ip", raw)
		http.Error(w, "forbidden", http.StatusForbidden)
	})
}

func (s *server) clientIP(r *http.Request) string {
	if v := r.Header.Get(s.cfg.clientIPHeader); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Tools
//
// Every description is written for a model that will speak the result aloud.
// Results are short, parameters are few, and nothing requires a disambiguation
// round trip — a clarifying question costs a whole conversational turn in voice.
// ---------------------------------------------------------------------------

type searchInput struct {
	Query string `json:"query" jsonschema:"what to look for, in note titles and note text"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many notes to return (default 5, max 20)"`
}

type readInput struct {
	Note string `json:"note" jsonschema:"the note to read, as a title or a vault path such as 'Projects/Homelab'"`
}

type listInput struct {
	Folder string `json:"folder,omitempty" jsonschema:"vault folder to list, or omit for the top level"`
}

type captureInput struct {
	Text string `json:"text" jsonschema:"the thought to capture, in the user's own words"`
}

type appendInput struct {
	Note string `json:"note" jsonschema:"the note to add to, as a title or vault path; created if it does not exist"`
	Text string `json:"text" jsonschema:"the text to add at the end of the note"`
}

type createInput struct {
	Note    string `json:"note" jsonschema:"title or vault path for the new note; fails if it already exists"`
	Content string `json:"content" jsonschema:"the full markdown body of the new note"`
}

type editInput struct {
	Note    string `json:"note" jsonschema:"the note to edit, as a title or vault path"`
	OldText string `json:"old_text" jsonschema:"exact text to replace; must appear exactly once in the note"`
	NewText string `json:"new_text" jsonschema:"replacement text; may be empty to remove the old text"`
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func (s *server) mcpServer() *mcp.Server {
	instructions := "Read and add to the user's personal Obsidian vault. " +
		"Results are spoken aloud, so summarise rather than reading notes verbatim, " +
		"and keep answers to a couple of sentences unless asked for detail. " +
		"To save a passing thought use capture_note. Notes cannot be deleted through this server."
	if s.vault.HasExcludes() {
		// Without this the model reads an exclusion as "no such note" and offers
		// to create one, which is a confusing turn to sit through over voice.
		instructions += " Some folders of the vault are deliberately not available here; " +
			"if a tool says so, tell the user it is in Obsidian rather than assuming the note does not exist."
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "obsidian-vault", Version: version}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_notes",
		Description: "Search the vault by keyword and return matching notes with a short snippet. Use this before read_note when the exact title is not known.",
	}, s.searchNotes)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_note",
		Description: "Read one note by title or path. Long notes are truncated; summarise rather than reciting.",
	}, s.readNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_notes",
		Description: "List note titles and subfolders in a vault folder.",
	}, s.listNotes)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "capture_note",
		Description: "Save a thought to the user's capture note as a timestamped line. " +
			"This is the right tool for 'remember that', 'add to my list', or any passing idea with no obvious home.",
	}, s.captureNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "append_note",
		Description: "Add text to the end of a specific note, creating the note if it does not exist. Existing content is never touched.",
	}, s.appendNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_note",
		Description: "Create a new note with a full body. Fails if a note of that name already exists — use append_note or edit_note instead.",
	}, s.createNote)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "edit_note",
		Description: "Replace an exact piece of text in a note. The old text must appear exactly once. " +
			"Read the note first so the anchor is exact. There is no delete tool: notes cannot be removed through this server.",
	}, s.editNote)

	return srv
}

// audit records every tool invocation.
//
// Writes already leave a trail: each one is a git commit authored by Claude
// Voice. Reads left none at all, so a credential used against this vault
// produced no signal anywhere -- "possibly compromised, no way to check". This
// is the difference between that and being able to compare a week of calls
// against your own memory of using it.
//
// Deliberately records WHAT was touched and never the content: note paths and
// result counts, not note bodies and not search queries, which are themselves
// personal text.
func (s *server) audit(ctx context.Context, tool string, args ...any) {
	s.log.Info("tool", append([]any{"sub", subjectFrom(ctx), "name", tool}, args...)...)
}

// auditDenied records a refused tool call.
//
// This is the higher-value half of the trail. Successful reads are mostly noise;
// an attempt to reach an excluded folder is the single most security-relevant
// thing this server can observe, and it previously left no trace anywhere -- the
// caller got a polite message and the operator got nothing.
func (s *server) auditDenied(ctx context.Context, reason string, args ...any) {
	s.log.Warn("tool denied", append([]any{"sub", subjectFrom(ctx), "reason", reason}, args...)...)
}

func (s *server) searchNotes(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}
	hits, err := s.vault.Search(in.Query, limit)
	if err != nil {
		return nil, nil, err
	}
	s.audit(ctx, "search_notes", "hits", len(hits))
	if len(hits) == 0 {
		return text(fmt.Sprintf("No notes match %q.", in.Query)), nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d note(s) matching %q:\n", len(hits), in.Query)
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s — %s\n", h.Path, h.Snippet)
	}
	return text(b.String()), nil, nil
}

func (s *server) readNote(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
	body, err := s.vault.Read(in.Note)
	if err != nil {
		return s.toolError(ctx, err)
	}
	s.audit(ctx, "read_note", "note", in.Note, "bytes", len(body))
	return text(body), nil, nil
}

func (s *server) listNotes(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, any, error) {
	names, err := s.vault.List(in.Folder, 50)
	if err != nil {
		return s.toolError(ctx, err)
	}
	s.audit(ctx, "list_notes", "folder", in.Folder, "entries", len(names))
	if len(names) == 0 {
		return text("That folder is empty."), nil, nil
	}
	return text(strings.Join(names, "\n")), nil, nil
}

func (s *server) captureNote(ctx context.Context, _ *mcp.CallToolRequest, in captureInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Text) == "" {
		return text("Nothing to capture."), nil, nil
	}
	rel, err := s.vault.Capture(s.cfg.captureNote, in.Text, time.Now())
	if err != nil {
		return s.toolError(ctx, err)
	}
	s.audit(ctx, "capture_note", "note", rel)
	s.commit(ctx, rel, "voice: capture")
	return text("Captured to " + rel + "."), nil, nil
}

func (s *server) appendNote(ctx context.Context, _ *mcp.CallToolRequest, in appendInput) (*mcp.CallToolResult, any, error) {
	rel, err := s.vault.Append(in.Note, in.Text)
	if err != nil {
		return s.toolError(ctx, err)
	}
	s.audit(ctx, "append_note", "note", rel)
	s.commit(ctx, rel, "voice: append to "+rel)
	return text("Added to " + rel + "."), nil, nil
}

func (s *server) createNote(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, any, error) {
	rel, err := s.vault.Create(in.Note, in.Content)
	if err != nil {
		return s.toolError(ctx, err)
	}
	s.audit(ctx, "create_note", "note", rel)
	s.commit(ctx, rel, "voice: create "+rel)
	return text("Created " + rel + "."), nil, nil
}

func (s *server) editNote(ctx context.Context, _ *mcp.CallToolRequest, in editInput) (*mcp.CallToolResult, any, error) {
	rel, err := s.vault.Edit(in.Note, in.OldText, in.NewText)
	if err != nil {
		return s.toolError(ctx, err)
	}
	s.audit(ctx, "edit_note", "note", rel)
	s.commit(ctx, rel, "voice: edit "+rel)
	return text("Updated " + rel + "."), nil, nil
}

// commit snapshots a write. Failures are logged, never returned: the note is on
// disk already, and the hourly backstop in vault-sync is the safety net. Turning
// a successful write into a failed tool call would be a worse outcome than an
// unversioned one.
func (s *server) commit(ctx context.Context, relPath, message string) {
	if !s.cfg.snapshot {
		return
	}
	if err := s.snaps.Commit(ctx, relPath, message); err != nil {
		s.log.Warn("snapshot failed", "path", relPath, "err", err)
	}
}

// toolError converts an expected, user-facing failure into tool output the model
// can act on, rather than a protocol error it can only apologise for. Anything
// unexpected is still returned as a real error.
func (s *server) toolError(ctx context.Context, err error) (*mcp.CallToolResult, any, error) {
	// Refusals are recorded, not just returned. An attempt to reach an excluded
	// folder is the most security-relevant thing this server sees, and it used
	// to leave no trace: the caller got a polite message and the operator got
	// nothing at all.
	switch {
	case errors.Is(err, ErrExcluded):
		s.auditDenied(ctx, "excluded")
	case errors.Is(err, ErrDenied):
		s.auditDenied(ctx, "deny-list")
	case errors.Is(err, ErrOutside):
		s.auditDenied(ctx, "outside-vault")
	}

	switch {
	case errors.Is(err, ErrNotFound):
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
			&mcp.TextContent{Text: "There is no note at that path. Try search_notes to find the right one."},
		}}, nil, nil
	case errors.Is(err, ErrExists):
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
			&mcp.TextContent{Text: "A note with that name already exists. Use append_note or edit_note instead."},
		}}, nil, nil
	case errors.Is(err, ErrDenied):
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
			&mcp.TextContent{Text: "This server only handles markdown notes, and not .claude/, AGENTS.md or CLAUDE.md. Tell the user that one has to be done in Obsidian."},
		}}, nil, nil
	case errors.Is(err, ErrExcluded):
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
			&mcp.TextContent{Text: "That part of the vault is deliberately not available through this connector. Tell the user it is there in Obsidian, but voice cannot reach it — do not try another path to get at it."},
		}}, nil, nil
	case errors.Is(err, ErrOutside):
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
			&mcp.TextContent{Text: "That path is outside the vault."},
		}}, nil, nil
	case errors.Is(err, ErrNoAnchor):
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
			&mcp.TextContent{Text: "That exact text is not in the note. Read the note again and use text copied from it."},
		}}, nil, nil
	case errors.Is(err, ErrConflict):
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
			&mcp.TextContent{Text: "That note was changed by Obsidian while this edit was being prepared, so nothing was written. Read it again and retry."},
		}}, nil, nil
	case errors.Is(err, ErrWouldEmpty):
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
			&mcp.TextContent{Text: "That edit would leave the note empty, which this server does not allow. Tell the user to clear it in Obsidian."},
		}}, nil, nil
	case errors.Is(err, ErrNotUnique):
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
			&mcp.TextContent{Text: "That text appears more than once. Include surrounding lines so the anchor is unique."},
		}}, nil, nil
	default:
		return nil, nil, err
	}
}
