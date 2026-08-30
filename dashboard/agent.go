// The agent role: the only process in this repo that holds the Docker socket.
//
// It exists so that the process rendering HTML does not. Everything the browser
// can cause to happen arrives here as one of the verbs in actionSpecs, and this
// file builds the argv itself -- the request contributes at most a stack name
// and a service name, both checked against what is actually on disk and running
// before they are allowed near exec. There is deliberately no passthrough, no
// "extra args" field and no shell: exec.Command with an explicit argv means a
// service name of `; rm -rf /` is a name that fails validation, not a command.
//
// It delegates to bin/homelab rather than reimplementing deploys against the
// Engine API. That CLI is already the sanctioned entry point, it is linted in
// CI, and it carries behaviour a reimplementation would silently lose --
// provenance verification, the obsidian-vault deploy.sh delegation, and the
// re-pair warning. The dashboard is a front-end for it, not a second way to do
// the same job.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// nameRe is what a stack or service name may look like. Compose itself is more
// permissive, but everything in this repo is lower-kebab and the narrow rule is
// the one worth enforcing: it admits no path separator, no whitespace, no shell
// metacharacter and no leading dash that could be read as a flag.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type actionSpec struct {
	// args builds the argv after the bin/homelab path. Constants only, plus the
	// already-validated stack and service.
	args func(stack, service string) []string
	// mutating actions require the caller to have proved the token AND are
	// refused against the dashboard's own stack.
	mutating bool
	// sensitive actions change nothing but hand back content, so they need the
	// token too. The distinction matters because the page itself is open: what
	// is ungated has to be no more than the page already shows.
	sensitive bool
	// allowService permits an optional per-service target.
	allowService bool
	// onlyStack restricts a verb to one stack, for options that only exist there.
	onlyStack string
	timeout   time.Duration
}

// actionSpecs is the closed set. Adding a verb is a deliberate act with a
// review; there is no map lookup that falls through to "run whatever was asked".
var actionSpecs = map[Action]actionSpec{
	ActionStatus:    {args: func(s, _ string) []string { return []string{"status", s} }, timeout: 60 * time.Second},
	ActionPs:        {args: func(s, _ string) []string { return []string{"ps", s} }, timeout: 60 * time.Second},
	ActionEnvCheck:  {args: func(s, _ string) []string { return []string{"env", "check", s} }, timeout: 30 * time.Second},
	ActionPreflight: {args: func(s, _ string) []string { return []string{"preflight", s} }, timeout: 2 * time.Minute},
	// Sensitive: this is the public hostname of the one service in this repo
	// reachable from the internet. The status page deliberately does not print
	// it, so neither should an ungated button.
	ActionURL: {args: func(_, _ string) []string { return []string{"url"} }, onlyStack: "vault-mcp", sensitive: true, timeout: 30 * time.Second},

	// Sensitive, and this is the one that matters. `logs obsidian-vault
	// vault-claude` returns the output of the always-on Claude agent -- note
	// contents, file paths, what it has been doing. That is inside the trust
	// boundary obsidian-vault/ARCHITECTURE.md exists to protect, and nothing
	// like the container list the page already shows.
	ActionLogs: {
		sensitive:    true,
		allowService: true,
		timeout:      60 * time.Second,
		args: func(s, svc string) []string {
			if svc == "" {
				return []string{"logs", s}
			}
			return []string{"logs", s, svc}
		},
	},

	ActionRestart: {
		mutating:     true,
		allowService: true,
		// Recreating one service of a stack is the common case -- restarting
		// vault-claude must not interrupt vault-sync, which is the whole reason
		// obsidian-vault splits them.
		timeout: 5 * time.Minute,
		args: func(s, svc string) []string {
			if svc == "" {
				return []string{"restart", s}
			}
			return []string{"restart", s, svc}
		},
	},

	// Long because it pulls. A cold pull of the agent image over a domestic
	// connection is minutes, and a deploy killed halfway is worse than a slow one.
	ActionDeploy: {
		mutating: true,
		timeout:  20 * time.Minute,
		args:     func(s, _ string) []string { return []string{"deploy", s} },
	},

	// A separate verb rather than a free-form flag, so the "no arguments from
	// the browser" property holds. It exists because deploy.sh's --sync-only is
	// the option you want when vault-claude is holding a live session you would
	// rather not interrupt.
	ActionDeploySyncOnly: {
		mutating:  true,
		onlyStack: "obsidian-vault",
		timeout:   20 * time.Minute,
		args:      func(s, _ string) []string { return []string{"deploy", s, "--sync-only"} },
	},
}

type agent struct {
	repoDir   string
	selfStack string
	docker    *dockerClient
	dockerErr string
	token     string

	// One mutation at a time, host-wide. Two concurrent deploys against the same
	// daemon is not a case worth reasoning about, and the second tab that tries
	// gets a clear "busy with X" rather than an interleaved log.
	mu sync.Mutex

	// busy is read by the goroutine that FAILED to take mu, precisely while
	// another goroutine holds mu and is writing it -- so it cannot be guarded by
	// mu itself. Its own mutex, and never touched directly.
	busyMu sync.Mutex
	busy   string

	// Bounds concurrent read commands. Reads are deliberately not serialised
	// behind mu, so without this a loop of requests spawns unbounded bash and
	// docker compose children inside the one container on this host that holds
	// the daemon socket. The cap is generous for a single-user dashboard and
	// still finite.
	reads chan struct{}
}

func (a *agent) setBusy(what string) {
	a.busyMu.Lock()
	a.busy = what
	a.busyMu.Unlock()
}

func (a *agent) loadBusy() string {
	a.busyMu.Lock()
	defer a.busyMu.Unlock()
	return a.busy
}

func newAgentServer() (*http.Server, error) {
	repoDir := envOr("REPO_DIR", "/repo")
	if _, err := os.Stat(filepath.Join(repoDir, "bin", "homelab")); err != nil {
		return nil, fmt.Errorf("REPO_DIR=%s does not look like the checkout: %w", repoDir, err)
	}

	token := strings.TrimSpace(os.Getenv("DASH_AGENT_TOKEN"))
	if token == "" {
		// Refuse to start rather than run open. The agent has no published port,
		// but "unreachable from outside" is a property of the compose file, and a
		// compose file is exactly the thing this dashboard makes easy to change.
		return nil, errors.New("DASH_AGENT_TOKEN is empty: the agent will not run without one")
	}

	a := &agent{
		repoDir:   repoDir,
		selfStack: envOr("DASH_SELF_STACK", "dashboard"),
		token:     token,
		reads:     make(chan struct{}, envIntOr("DASH_MAX_READS", 4)),
	}

	d, err := newDockerClient(envOr("DOCKER_HOST", "unix:///var/run/docker.sock"))
	if err != nil {
		return nil, err
	}
	a.docker = d

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/v1/state", a.requireToken(a.handleState))
	mux.HandleFunc("/v1/action", a.requireToken(a.handleAction))

	return &http.Server{
		Addr:    agentAddr(),
		Handler: mux,
		// No WriteTimeout: a deploy legitimately holds the response open for
		// minutes, and the per-action timeout above is the real bound.
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

func (a *agent) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// CutPrefix, not TrimPrefix: TrimPrefix leaves a header that is just the
		// bare token unchanged, which quietly accepts a second credential format
		// nobody designed. Require the scheme.
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// --- state ------------------------------------------------------------------

func (a *agent) handleState(w http.ResponseWriter, r *http.Request) {
	st := a.state(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

func (a *agent) state(ctx context.Context) State {
	st := State{TakenAt: time.Now().UTC()}

	containers, err := a.docker.containers(ctx)
	if err != nil {
		st.DockerErr = err.Error()
	}

	byStack := map[string][]Container{}
	for _, c := range containers {
		byStack[c.Stack] = append(byStack[c.Stack], c)
	}

	names, err := a.stackNames()
	if err != nil {
		st.StacksErr = err.Error()
	}

	for _, name := range names {
		s := Stack{Name: name, Self: name == a.selfStack}
		envPath := filepath.Join(a.repoDir, name, ".env")
		if info, err := os.Stat(envPath); err == nil {
			s.EnvPresent = true
			s.EnvMode = fmt.Sprintf("%03o", info.Mode().Perm())
			s.Image = envValue(envPath, "IMAGE")
		}
		if _, err := os.Stat(filepath.Join(a.repoDir, name, "scripts", "preflight.sh")); err == nil {
			s.HasPreflight = true
		}
		s.Containers = byStack[name]
		sort.Slice(s.Containers, func(i, j int) bool { return s.Containers[i].Name < s.Containers[j].Name })
		st.Stacks = append(st.Stacks, s)
	}

	// Everything Container Station runs that this repo does not own. The root
	// README is explicit that `docker ps` is the ground truth for the host, so a
	// dashboard that only showed this repo's stacks would be quietly wrong about
	// what is running.
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	for _, c := range containers {
		if !known[c.Stack] {
			st.Unmanaged = append(st.Unmanaged, c)
		}
	}
	sort.Slice(st.Unmanaged, func(i, j int) bool { return st.Unmanaged[i].Name < st.Unmanaged[j].Name })

	return st
}

// stackNames asks bin/homelab which stacks it will accept.
//
// It used to scan the checkout for docker-compose.yml instead, and that was a
// bug: the CLI gates every command on its own STACKS list, so a directory could
// have a compose file, appear here, and then have every button fail with
// "unknown stack". Two answers to one question, and the dashboard was drawing
// buttons from the wrong one.
//
// Asking is slower than a readdir by about a bash startup, on a call made once
// per page load. Worth it to have a single list: adding a stack is one edit to
// STACKS, and this follows automatically.
//
// A failure here is deliberately NOT softened into an empty list that renders as
// "no stacks". If the CLI cannot be run, every button was going to fail anyway,
// and saying so is the useful answer.
func (a *agent) stackNames() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, filepath.Join(a.repoDir, "bin", "homelab"), "stacks")
	cmd.Dir = a.repoDir
	cmd.Env = a.childEnv()

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("cannot list stacks: %w", err)
	}

	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// Skip anything that is not a plain name rather than trusting the
		// output wholesale: this list decides what reaches exec.
		if line == "" || !nameRe.MatchString(line) {
			continue
		}
		names = append(names, line)
	}
	sort.Strings(names)
	return names, nil
}

// envValue reads one key out of a .env without sourcing it.
//
// Never returns anything but the requested key's value, and the only key any
// caller asks for is IMAGE. This file also holds MCP_TOKEN and TS_AUTHKEY, so
// "read the whole file into a map and hand it to the template" is a shape worth
// not having available at all.
func envValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

// --- actions ----------------------------------------------------------------

func (a *agent) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req ActionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	res, status := a.run(r.Context(), req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// validate is the gate. It is a separate function from run so the tests can
// hammer it directly with everything a browser could send.
func (a *agent) validate(req ActionRequest) (actionSpec, error) {
	spec, ok := actionSpecs[req.Action]
	if !ok {
		return spec, fmt.Errorf("unknown action %q", req.Action)
	}
	if !nameRe.MatchString(req.Stack) {
		return spec, fmt.Errorf("invalid stack name %q", req.Stack)
	}

	names, err := a.stackNames()
	if err != nil {
		return spec, err
	}
	known := false
	for _, n := range names {
		if n == req.Stack {
			known = true
			break
		}
	}
	if !known {
		return spec, fmt.Errorf("bin/homelab does not know a stack called %q", req.Stack)
	}

	if spec.onlyStack != "" && spec.onlyStack != req.Stack {
		return spec, fmt.Errorf("%s is not available for %s", req.Action, req.Stack)
	}

	// The dashboard does not manage the dashboard. A deploy of this stack
	// recreates the very container running the deploy, so the command is killed
	// partway through and the result is unknowable -- did it pull? did it
	// recreate the agent but not the web? Rather than special-case a
	// self-restart that cannot report its own outcome, the verb is simply not
	// available for this stack. Deploy the dashboard over SSH, like everything
	// else in this repo was deployed before it existed.
	if spec.mutating && req.Stack == a.selfStack {
		return spec, fmt.Errorf("%s manages the other stacks, not itself: run `homelab %s %s` over SSH",
			a.selfStack, req.Action, req.Stack)
	}

	if req.Service != "" {
		if !spec.allowService {
			return spec, fmt.Errorf("%s does not take a service", req.Action)
		}
		if !nameRe.MatchString(req.Service) {
			return spec, fmt.Errorf("invalid service name %q", req.Service)
		}
		if !a.serviceExists(req.Stack, req.Service) {
			return spec, fmt.Errorf("no service %q running in %s", req.Service, req.Stack)
		}
	}

	return spec, nil
}

// serviceExists checks the name against the compose labels of containers that
// actually exist, rather than parsing docker-compose.yml.
//
// That means a service which has never been created cannot be targeted
// individually -- deploy the stack instead. Accepted deliberately: it keeps a
// YAML parser out of the process holding the Docker socket, and the failure mode
// is a missing button, not a wrong one.
func (a *agent) serviceExists(stack, service string) bool {
	containers, err := a.docker.containers(context.Background())
	if err != nil {
		return false
	}
	for _, c := range containers {
		if c.Stack == stack && c.Service == service {
			return true
		}
	}
	return false
}

// childEnv is the environment every bin/homelab invocation gets.
//
// Deliberately tiny. The CLI needs docker on PATH and nothing else; inheriting
// this process's environment would hand every child DASH_AGENT_TOKEN for no
// reason, which TestChildEnvironmentDoesNotLeakTheToken asserts against.
//
// NO_COLOR and TERM=dumb: bin/homelab drops ANSI when stdout is not a terminal,
// but it is cheaper to state it than to depend on it, and the output goes
// straight into an HTML page.
func (a *agent) childEnv() []string {
	return []string{
		"PATH=" + envOr("HOMELAB_PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"),
		"HOME=/tmp",
		"NO_COLOR=1",
		"TERM=dumb",
		"DOCKER_HOST=" + envOr("DOCKER_HOST", "unix:///var/run/docker.sock"),
	}
}

func (a *agent) run(ctx context.Context, req ActionRequest) (ActionResult, int) {
	res := ActionResult{Action: req.Action, Stack: req.Stack, Service: req.Service}

	spec, err := a.validate(req)
	if err != nil {
		res.Err = err.Error()
		res.ExitCode = -1
		return res, http.StatusBadRequest
	}

	if spec.mutating {
		if !a.mu.TryLock() {
			res.Err = fmt.Sprintf("busy: %s is still running", a.loadBusy())
			res.ExitCode = -1
			return res, http.StatusConflict
		}
		a.setBusy(fmt.Sprintf("%s %s", req.Action, req.Stack))
		defer func() {
			a.setBusy("")
			a.mu.Unlock()
		}()
	} else {
		// Reads run concurrently with each other and with a mutation, which is
		// what makes "watch the logs during a deploy" work. Bounded, though:
		// see the note on the reads field.
		select {
		case a.reads <- struct{}{}:
			defer func() { <-a.reads }()
		default:
			res.Err = "too many commands running at once, try again in a moment"
			res.ExitCode = -1
			return res, http.StatusTooManyRequests
		}
	}

	// DETACH A MUTATION FROM THE CALLER.
	//
	// ctx arrives from the HTTP request, and net/http cancels it the moment the
	// client connection goes away. Left attached it reaches exec.CommandContext
	// below, so closing the browser tab -- or a phone locking and dropping the
	// connection -- SIGKILLs `docker compose` partway through a pull or a
	// recreate. That is exactly the outcome ActionDeploy's long timeout exists
	// to avoid, and a half-finished deploy is worse than a slow one.
	//
	// So a mutation is bounded by its own timeout and nothing else. A read stays
	// attached: nobody is harmed by cancelling a `logs` nobody is waiting for.
	if spec.mutating {
		ctx = context.WithoutCancel(ctx)
	}

	ctx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()

	argv := spec.args(req.Stack, req.Service)
	start := time.Now()

	cmd := exec.CommandContext(ctx, filepath.Join(a.repoDir, "bin", "homelab"), argv...)
	cmd.Dir = a.repoDir
	cmd.Env = a.childEnv()
	// Stdin is deliberately left as the zero value: os/exec gives a nil Stdin
	// the null device, so a script that ever tries to prompt reads EOF and fails
	// immediately instead of blocking. Do not "fix" this by wiring os.Stdin --
	// there is no terminal on the other end of an HTTP request.

	out, err := cmd.CombinedOutput()
	res.Duration = time.Since(start)
	res.Output = string(out)

	// err first. A command that ran to completion is a success even if the
	// context expired immediately afterwards -- checking ctx.Err() ahead of err
	// reported a finished deploy as a timeout, with its own successful output
	// printed underneath the failure line.
	//
	// And a cancelled context is not a timeout: with the detach above only a
	// read can be cancelled, but reporting "timed out after 60s" three seconds
	// in sends you looking at the wrong thing entirely.
	switch {
	case err == nil:
		// Ran, exited 0.
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.ExitCode = -1
		res.Err = fmt.Sprintf("timed out after %s", spec.timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		res.ExitCode = -1
		res.Err = "cancelled before it finished"
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			res.Err = fmt.Sprintf("exited %d", ee.ExitCode())
		} else {
			res.ExitCode = -1
			res.Err = err.Error()
		}
	}

	log.Printf("action=%s stack=%s service=%s exit=%d took=%s",
		req.Action, req.Stack, req.Service, res.ExitCode, res.Duration.Round(time.Millisecond))

	return res, http.StatusOK
}
