// The web role: renders the page, holds no privilege.
//
// It has no Docker socket, no checkout and no way to run a command. Everything
// it shows came from the agent over /v1/state, and everything it can cause came
// back through /v1/action. Its own secret, DASH_TOKEN, only decides whether a
// visitor may ask the agent to do the mutating half.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiFS embed.FS

type web struct {
	agent    *agentClient
	registry *registryClient
	auth     *authenticator
	tmpl     *template.Template

	selfStack string
	refresh   time.Duration
}

func newWebServer() (*http.Server, error) {
	agentURL := envOr("DASH_AGENT_URL", "http://homelab-dashd:8090")

	agentToken := strings.TrimSpace(os.Getenv("DASH_AGENT_TOKEN"))
	if agentToken == "" {
		return nil, fmt.Errorf("DASH_AGENT_TOKEN is empty: the web role cannot talk to the agent without it")
	}

	tmpl, err := parseUI()
	if err != nil {
		return nil, err
	}

	w := &web{
		agent:     newAgentClient(agentURL, agentToken),
		registry:  newRegistryClient(envDurationOr("DASH_REGISTRY_TTL", 15*time.Minute)),
		auth:      newAuthenticator(os.Getenv("DASH_TOKEN"), envDurationOr("DASH_SESSION_TTL", 12*time.Hour)),
		tmpl:      tmpl,
		selfStack: envOr("DASH_SELF_STACK", "dashboard"),
		refresh:   envDurationOr("DASH_REFRESH", 60*time.Second),
	}

	if w.auth.ReadOnly() {
		log.Print("DASH_TOKEN is not set: running READ-ONLY, no action will be accepted")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) { fmt.Fprintln(rw, "ok") })
	mux.HandleFunc("/login", w.handleLogin)
	mux.HandleFunc("/logout", w.handleLogout)
	mux.HandleFunc("/action", w.handleAction)
	mux.HandleFunc("/", w.handleIndex)

	return &http.Server{
		Addr:              webAddr(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

// parseUI compiles the page. Separate from newWebServer so the tests can render
// every state -- including the ones with no agent and no Docker -- without
// standing up a server.
func parseUI() (*template.Template, error) {
	return template.New("ui.html").Funcs(templateFuncs()).ParseFS(uiFS, "ui.html")
}

// --- page -------------------------------------------------------------------

type pageData struct {
	State     State
	Err       string
	LoginErr  string
	ReadOnly  bool
	SignedIn  bool
	SelfStack string
	Version   string
	RefreshS  int
	Now       time.Time
}

func (w *web) handleIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}

	data := pageData{
		ReadOnly:  w.auth.ReadOnly(),
		SignedIn:  w.auth.SignedIn(r),
		SelfStack: w.selfStack,
		Version:   Version,
		RefreshS:  int(w.refresh.Seconds()),
		Now:       time.Now(),
		LoginErr:  r.URL.Query().Get("login_error"),
	}

	st, err := w.agent.State(r.Context())
	if err != nil {
		// Render the shell with the error rather than a bare 500. "The agent is
		// down" is itself a useful thing for this page to be able to say.
		data.Err = err.Error()
	} else {
		w.annotate(r.Context(), &st)
		data.State = st
	}

	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := w.tmpl.Execute(rw, data); err != nil {
		log.Printf("render: %v", err)
	}
}

// annotate fills in the update status for each stack.
//
// Concurrently, because each stack is an independent HTTPS round trip and doing
// them in series makes a cold page load as slow as the sum of them. Bounded by
// the number of stacks, which is small by construction.
func (w *web) annotate(ctx context.Context, st *State) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := range st.Stacks {
		s := &st.Stacks[i]
		if s.Image == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Update = w.registry.updateFor(ctx, s.Image, runningDigest(s))
		}()
	}
	wg.Wait()
}

// runningDigest picks the digest to compare against the registry.
//
// A stack can run several containers off several images -- obsidian-vault runs
// three off one, vault-mcp runs its server plus a pinned Tailscale sidecar. The
// question the badge answers is about the stack's OWN image, the one .env names
// and a deploy would pull, so match on that repository and ignore the rest.
func runningDigest(s *Stack) string {
	want, err := parseImageRef(s.Image)
	if err != nil {
		return ""
	}
	for _, c := range s.Containers {
		got, err := parseImageRef(c.Image)
		if err != nil {
			continue
		}
		if got.Repo == want.Repo && got.Registry == want.Registry && c.RepoDigest != "" {
			return c.RepoDigest
		}
	}
	return ""
}

// --- auth handlers ----------------------------------------------------------

func (w *web) handleLogin(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}
	if err := w.auth.Login(r.PostFormValue("token")); err != nil {
		// Back to the page with a message, not a 401 page: this is a single-user
		// dashboard on a home network, and the useful response to a typo is the
		// form again.
		http.Redirect(rw, r, "/?login_error="+urlQueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	w.auth.SetCookie(rw)
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

func (w *web) handleLogout(rw http.ResponseWriter, r *http.Request) {
	w.auth.ClearCookie(rw)
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

// --- actions ----------------------------------------------------------------

// actionTimeouts mirror the agent's, plus slack for the round trip. The web
// role must not give up before the agent does, or a deploy would look failed
// while it was still pulling.
var actionTimeouts = map[Action]time.Duration{
	ActionDeploy:         21 * time.Minute,
	ActionDeploySyncOnly: 21 * time.Minute,
	ActionRestart:        6 * time.Minute,
	ActionPreflight:      3 * time.Minute,
}

func (w *web) handleAction(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ActionRequest
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(rw, http.StatusBadRequest, ActionResult{Err: "bad request"})
		return
	}

	// Read-only verbs stay open, exactly as the page they appear on does.
	// Anything that changes the host needs the token.
	if spec, ok := actionSpecs[req.Action]; !ok {
		writeJSON(rw, http.StatusBadRequest, ActionResult{Action: req.Action, Err: "unknown action"})
		return
	} else if spec.mutating && !w.auth.Authorized(r) {
		msg := "sign in with DASH_TOKEN to run this"
		if w.auth.ReadOnly() {
			msg = "this dashboard is read-only: DASH_TOKEN is not set in its .env"
		}
		writeJSON(rw, http.StatusForbidden, ActionResult{Action: req.Action, Stack: req.Stack, Err: msg})
		return
	}

	timeout, ok := actionTimeouts[req.Action]
	if !ok {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	res, err := w.agent.Do(ctx, req)
	if err != nil {
		writeJSON(rw, http.StatusBadGateway, ActionResult{Action: req.Action, Stack: req.Stack, Err: err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, res)
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

func urlQueryEscape(s string) string {
	return strings.NewReplacer(" ", "+", "&", "%26", "#", "%23", "?", "%3F", "\"", "%22").Replace(s)
}
