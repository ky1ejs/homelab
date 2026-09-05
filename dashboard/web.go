// The web role: renders the page, holds no privilege.
//
// It has no Docker socket, no checkout and no way to run a command. Everything
// it shows came from the agent over /v1/state, and everything it can cause came
// back through /v1/action.
//
// It holds no credential of its own any more. Who a visitor is comes from the
// Tailscale-User-Login header that `tailscale serve` adds (auth.go), and whether
// that header can be believed is settled by the agent (agent.go's exposure()).
//
// THIS ROLE IS THE ONLY PLACE IDENTITY IS CHECKED. The agent independently
// enforces the exposure premise, but it never sees a tailnet identity -- its own
// credential is DASH_AGENT_TOKEN, which this role holds unconditionally. So the
// authorisation in handleAction is not a convenience in front of a second check;
// it is the check.
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
	github   *githubClient
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

	auth, err := newAuthenticator(
		os.Getenv("DASH_ALLOWED_LOGINS"),
		envBool("DASH_ALLOW_ANY_TAILNET_USER"),
		envBool("DASH_READ_ONLY"),
	)
	if err != nil {
		return nil, err
	}

	w := &web{
		agent:     newAgentClient(agentURL, agentToken),
		registry:  newRegistryClient(envDurationOr("DASH_REGISTRY_TTL", 15*time.Minute)),
		github:    newGitHubClient(envDurationOr("DASH_GITHUB_TTL", 15*time.Minute)),
		auth:      auth,
		tmpl:      tmpl,
		selfStack: envOr("DASH_SELF_STACK", "dashboard"),
		refresh:   envDurationOr("DASH_REFRESH", 60*time.Second),
	}

	if w.auth.ReadOnly() {
		log.Print("DASH_READ_ONLY is set: no action will be accepted")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) { fmt.Fprintln(rw, "ok") })
	// No /login or /logout. Identity arrives on every request from `tailscale
	// serve`, so there is no session to start or end -- signing out means
	// leaving the tailnet or coming off DASH_ALLOWED_LOGINS.
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
	State State
	Err   string
	// Login is the tailnet user this request came from, "" when there is not
	// one. IdentityErr says why not, and is shown on the page rather than
	// swallowed: every way of failing to identify is a misconfiguration someone
	// has to go and fix, and "forbidden" would send them looking in the wrong
	// place.
	Login       string
	IdentityErr string
	ReadOnly    bool
	SignedIn    bool
	SelfStack   string
	Version     string
	RefreshS    int
	Now         time.Time
}

func (w *web) handleIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}

	data := pageData{
		ReadOnly:  w.auth.ReadOnly(),
		SelfStack: w.selfStack,
		Version:   Version,
		RefreshS:  int(w.refresh.Seconds()),
		Now:       time.Now(),
	}
	// Two questions, asked separately. Who you are is shown whatever the answer
	// to the second one is: "signed in as X, and X may not deploy" beats a page
	// that claims not to know you.
	data.Login, _ = w.auth.Identify(r)
	if _, err := w.auth.MayAct(r); err != nil {
		// Not shown in read-only mode, which has its own banner saying the same
		// thing more usefully. See ui.html.
		data.IdentityErr = err.Error()
	} else {
		data.SignedIn = true
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

	// Containers this repo does not manage get the same check, per container
	// rather than per stack: they have no stack and no .env naming an intended
	// image, so the reference on the container itself is the only question
	// there is to ask. Nothing here can act on the answer -- Container Station
	// owns their lifecycle -- but "Home Assistant is three releases behind" is
	// worth knowing from the same page as everything else.
	for i := range st.Unmanaged {
		c := &st.Unmanaged[i]
		if c.Image == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Update = w.registry.updateFor(ctx, c.Image, c.RepoDigest)
		}()
	}

	// The checkout comparison runs alongside the image checks rather than after
	// them: it is a third-party HTTPS call like the others, and serialising it
	// would add its latency to every cold page load.
	wg.Add(1)
	go func() {
		defer wg.Done()
		b := w.github.Behind(ctx, st.Checkout)
		if b == nil {
			return
		}
		b.Stacks = knownStacks(b.Stacks, st.Stacks)
		st.Checkout.Behind = b
	}()

	wg.Wait()
}

// knownStacks narrows the top-level directories a comparison touched to the
// ones that are actually deployable stacks.
//
// github.go cannot do this itself -- it does not know what is deployed -- and
// the page needs it done, because "these commits touch .github and bin" is not
// an answer to "what do I deploy after applying this".
func knownStacks(touched []string, stacks []Stack) []string {
	known := make(map[string]bool, len(stacks))
	for _, s := range stacks {
		known[s.Name] = true
	}
	// A NEW slice, never an in-place filter: `touched` belongs to a struct the
	// github client copied out of a shared cache, and its backing array is the
	// cached one.
	var out []string
	for _, t := range touched {
		if known[t] {
			out = append(out, t)
		}
	}
	return out
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

	// EVERY action needs this header, not only the gated ones.
	//
	// It used to be checked inside Authorize(), so the ungated verbs -- status,
	// ps, env check, preflight -- were reachable without it. That made this
	// endpoint a CORS "simple request": the body is never inspected for
	// Content-Type, so a cross-origin form post of
	// `{"action":"preflight","stack":"dashboard"}` needs no preflight and the
	// browser sends it. The response is unreadable to the attacker, so nothing
	// leaks -- but each of those verbs forks bash and docker compose inside the
	// container holding the daemon socket, and DASH_MAX_READS is 4, so any page
	// a tailnet user visits could hold the read pool shut and make every button
	// return 429.
	//
	// Requiring it uniformly costs nothing: ui.html sets it on every request
	// already. What it buys is that the whole endpoint is non-simple, so no
	// cross-origin request reaches it without a preflight this server does not
	// answer.
	if r.Header.Get(csrfHeader) == "" {
		writeJSON(rw, http.StatusForbidden, ActionResult{Err: "missing " + csrfHeader + " header"})
		return
	}

	var req ActionRequest
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(rw, http.StatusBadRequest, ActionResult{Err: "bad request"})
		return
	}

	// Two kinds of action need an identity: those that change the host, and
	// those that hand back content the open page does not already show. What is
	// left ungated -- status, ps, env check, preflight -- tells you no more than
	// the page rendered above it.
	if spec, ok := actionSpecs[req.Action]; !ok {
		writeJSON(rw, http.StatusBadRequest, ActionResult{Action: req.Action, Err: "unknown action"})
		return
	} else if spec.mutating || spec.sensitive {
		// THIS IS THE ONLY PLACE IDENTITY IS CHECKED. The agent independently
		// enforces the exposure premise -- a genuinely separate check in a
		// separate process -- but it never sees a tailnet identity: its own
		// credential is DASH_AGENT_TOKEN, which this role holds unconditionally
		// and presents on every call. So deleting this branch would let anything
		// that reaches the listener deploy, and the agent would not object.
		if _, err := w.auth.Authorize(r); err != nil {
			writeJSON(rw, http.StatusForbidden, ActionResult{Action: req.Action, Stack: req.Stack, Err: err.Error()})
			return
		}
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
