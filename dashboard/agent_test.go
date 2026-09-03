// These tests ARE the trust boundary.
//
// Same convention as vault-mcp's deny-list tests: the agent is the only process
// in this repo holding the Docker socket, and the argument that this is safe
// rests entirely on validate() rejecting everything that is not a known verb
// against a known stack. Asserting that here means an image that reaches the NAS
// cannot have a hole these cases would have caught, even if someone pushes past
// a red CI run.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shellList renders names as shell words for the stub CLI. Empty produces
// nothing, so the stub prints a blank line and the agent sees no stacks.
func shellList(names []string) string {
	if len(names) == 0 {
		return "''"
	}
	return strings.Join(names, " ")
}

// newTestAgent builds an agent over a throwaway checkout containing the given
// stacks, plus a fake Docker daemon serving the given containers.
func newTestAgent(t *testing.T, stacks []string, containers []apiContainer) *agent {
	t.Helper()

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real executable, so a test that reaches exec actually runs something
	// harmless and observable instead of failing on a missing file. It answers
	// `stacks` the way the real CLI does, because that is now how the agent
	// learns which stacks exist.
	script := "#!/bin/sh\nif [ \"$1\" = \"stacks\" ]; then\n" +
		"printf '%s\\n' " + shellList(stacks) + "\nexit 0\nfi\necho \"argv: $*\"\n"
	if err := os.WriteFile(filepath.Join(repo, "bin", "homelab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range stacks {
		if err := os.MkdirAll(filepath.Join(repo, s), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, s, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The exposure gate refuses every mutating and sensitive action unless this
	// stack's own containers are published on loopback only, so a compliant one
	// is always present. Tests that care about exposure override it by passing
	// their own container in the dashboard project; see the exposure tests
	// below, which build the agent through newTestAgentRaw instead.
	containers = append([]apiContainer{{
		Names:  []string{"/homelab-dash"},
		Labels: map[string]string{labelProject: "dashboard", labelService: "homelab-dash"},
		Ports:  []apiPort{{IP: "127.0.0.1", PrivatePort: 8080, PublicPort: 8088, Type: "tcp"}},
	}}, containers...)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/containers/json"):
			_ = json.NewEncoder(w).Encode(containers)
		case strings.HasPrefix(r.URL.Path, "/images/"):
			_ = json.NewEncoder(w).Encode(apiImage{RepoDigests: []string{"ghcr.io/o/r@sha256:abc"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	d, err := newDockerClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	// reads must be non-nil: a nil channel makes every select fall to default,
	// which would refuse every read command and quietly disable the limiter the
	// tests below are meant to exercise.
	return &agent{
		repoDir:     repo,
		selfStack:   "dashboard",
		selfService: "homelab-dash",
		docker:      d,
		token:       "secret",
		reads:       make(chan struct{}, 4),
	}
}

func TestValidateRejectsHostileInput(t *testing.T) {
	a := newTestAgent(t, []string{"obsidian-vault", "vault-mcp", "dashboard"}, nil)

	cases := []struct {
		name string
		req  ActionRequest
	}{
		{"unknown action", ActionRequest{Action: "rm", Stack: "vault-mcp"}},
		{"empty action", ActionRequest{Action: "", Stack: "vault-mcp"}},
		{"action that is a homelab subcommand but not exposed", ActionRequest{Action: "env", Stack: "vault-mcp"}},

		{"empty stack", ActionRequest{Action: ActionStatus, Stack: ""}},
		{"path traversal", ActionRequest{Action: ActionStatus, Stack: "../etc"}},
		{"absolute path", ActionRequest{Action: ActionStatus, Stack: "/etc"}},
		{"leading dash reads as a flag", ActionRequest{Action: ActionStatus, Stack: "-rf"}},
		{"shell metacharacters", ActionRequest{Action: ActionStatus, Stack: "vault-mcp; rm -rf /"}},
		{"command substitution", ActionRequest{Action: ActionStatus, Stack: "$(id)"}},
		{"newline injection", ActionRequest{Action: ActionStatus, Stack: "vault-mcp\nstatus"}},
		{"whitespace", ActionRequest{Action: ActionStatus, Stack: "vault mcp"}},
		{"uppercase", ActionRequest{Action: ActionStatus, Stack: "Vault-MCP"}},
		{"nul byte", ActionRequest{Action: ActionStatus, Stack: "vault-mcp\x00"}},
		{"unknown stack", ActionRequest{Action: ActionStatus, Stack: "not-a-stack"}},

		{"service on an action that takes none", ActionRequest{Action: ActionDeploy, Stack: "vault-mcp", Service: "vault-mcp"}},
		{"hostile service name", ActionRequest{Action: ActionRestart, Stack: "vault-mcp", Service: "../../bin/sh"}},
		{"service that is not running", ActionRequest{Action: ActionRestart, Stack: "vault-mcp", Service: "ghost"}},

		{"stack-specific verb on the wrong stack", ActionRequest{Action: ActionDeploySyncOnly, Stack: "vault-mcp"}},
		{"url on the wrong stack", ActionRequest{Action: ActionURL, Stack: "obsidian-vault"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.validate(context.Background(), tc.req); err == nil {
				t.Fatalf("validate accepted %+v", tc.req)
			}
		})
	}
}

func TestValidateAcceptsTheRealThing(t *testing.T) {
	a := newTestAgent(t, []string{"obsidian-vault", "vault-mcp", "dashboard"}, []apiContainer{
		{Names: []string{"/vault-sync"}, Labels: map[string]string{labelProject: "obsidian-vault", labelService: "vault-sync"}},
	})

	ok := []ActionRequest{
		{Action: ActionStatus, Stack: "vault-mcp"},
		{Action: ActionPs, Stack: "vault-mcp"},
		{Action: ActionLogs, Stack: "obsidian-vault"},
		{Action: ActionLogs, Stack: "obsidian-vault", Service: "vault-sync"},
		{Action: ActionRestart, Stack: "obsidian-vault", Service: "vault-sync"},
		{Action: ActionDeploy, Stack: "vault-mcp"},
		{Action: ActionDeploySyncOnly, Stack: "obsidian-vault"},
		{Action: ActionEnvCheck, Stack: "vault-mcp"},
		{Action: ActionURL, Stack: "vault-mcp"},
	}
	for _, req := range ok {
		if _, err := a.validate(context.Background(), req); err != nil {
			t.Errorf("validate rejected %+v: %v", req, err)
		}
	}
}

// The dashboard must not be able to deploy or restart itself: the command would
// kill the process running it partway through and report nothing.
func TestSelfStackIsNotMutable(t *testing.T) {
	a := newTestAgent(t, []string{"dashboard", "vault-mcp"}, nil)

	for _, action := range []Action{ActionDeploy, ActionRestart} {
		if _, err := a.validate(context.Background(), ActionRequest{Action: action, Stack: "dashboard"}); err == nil {
			t.Errorf("%s against the dashboard's own stack was accepted", action)
		}
	}
	// Reading about itself is fine and useful.
	if _, err := a.validate(context.Background(), ActionRequest{Action: ActionStatus, Stack: "dashboard"}); err != nil {
		t.Errorf("status against its own stack was rejected: %v", err)
	}
}

// Every verb's argv must be constants plus the validated names -- nothing the
// request supplies may become a flag.
func TestArgvIsConstantsPlusValidatedNames(t *testing.T) {
	for action, spec := range actionSpecs {
		argv := spec.args("vault-mcp", "vault-sync")
		if len(argv) == 0 {
			t.Errorf("%s produced an empty argv", action)
			continue
		}
		for i, arg := range argv {
			if arg == "" {
				t.Errorf("%s argv[%d] is empty", action, i)
			}
			if i > 0 && strings.HasPrefix(arg, "-") && arg != "--sync-only" {
				t.Errorf("%s argv[%d]=%q is an unexpected flag", action, i, arg)
			}
		}
	}
}

// Mutating verbs are serialised host-wide: two deploys against one daemon is
// not a state worth reasoning about.
func TestConcurrentMutationIsRefused(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)

	a.mu.Lock()
	a.busy = "deploy vault-mcp"
	defer a.mu.Unlock()

	res, status := a.run(context.Background(), ActionRequest{Action: ActionRestart, Stack: "vault-mcp"})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	if !strings.Contains(res.Err, "busy") {
		t.Fatalf("error = %q, want it to mention busy", res.Err)
	}
}

// A read-only verb is not serialised, so looking at a log while a deploy runs
// still works.
func TestReadsAreNotBlockedByAMutation(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)

	a.mu.Lock()
	a.busy = "deploy vault-mcp"
	defer a.mu.Unlock()

	res, status := a.run(context.Background(), ActionRequest{Action: ActionStatus, Stack: "vault-mcp"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(res.Output, "argv: status vault-mcp") {
		t.Fatalf("output = %q, want the stub's argv echo", res.Output)
	}
}

// The child gets a deliberately tiny environment. In particular it must not
// inherit the agent's own token.
func TestChildEnvironmentDoesNotLeakTheToken(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)
	a.repoDir = t.TempDir()

	if err := os.MkdirAll(filepath.Join(a.repoDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.repoDir, "vault-mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.repoDir, "vault-mcp", "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Answers `stacks` like the real CLI, then dumps its environment so the
	// test can see exactly what the child was given.
	stub := "#!/bin/sh\nif [ \"$1\" = \"stacks\" ]; then echo vault-mcp; exit 0; fi\nenv\n"
	if err := os.WriteFile(filepath.Join(a.repoDir, "bin", "homelab"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DASH_AGENT_TOKEN", "super-secret-value")

	res, _ := a.run(context.Background(), ActionRequest{Action: ActionStatus, Stack: "vault-mcp"})
	if strings.Contains(res.Output, "super-secret-value") {
		t.Fatalf("the child inherited DASH_AGENT_TOKEN:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "NO_COLOR=1") {
		t.Fatalf("NO_COLOR was not set for the child:\n%s", res.Output)
	}
}

func TestRequireTokenRejectsWrongAndMissing(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)
	handler := a.requireToken(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })

	for _, header := range []string{"", "Bearer ", "Bearer wrong", "secret", "Bearer secret "} {
		req := httptest.NewRequest(http.MethodGet, "/v1/state", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Authorization=%q got %d, want 403", header, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/state", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Errorf("the right token got %d, want it through", rec.Code)
	}
}

// The stack list comes from bin/homelab, NOT from scanning the checkout.
//
// This is a regression test for a real bug: the agent used to look for
// directories containing docker-compose.yml, while every command in the CLI
// gates on its own STACKS list. A directory present in one and absent from the
// other meant the dashboard drew a full row of buttons for a stack that every
// command then rejected with "unknown stack".
func TestStackListComesFromTheCLI(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp", "fishing"}, nil)

	// A directory with a compose file that the CLI does NOT list. The old
	// implementation would have returned it.
	if err := os.MkdirAll(filepath.Join(a.repoDir, "not-a-stack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.repoDir, "not-a-stack", "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	names, err := a.stackNames()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "fishing,vault-mcp" {
		t.Fatalf("stackNames() = %q, want \"fishing,vault-mcp\"", got)
	}

	// And the buttons must not be offered for it either.
	if _, err := a.validate(context.Background(), ActionRequest{Action: ActionStatus, Stack: "not-a-stack"}); err == nil {
		t.Fatal("validate accepted a directory the CLI does not list as a stack")
	}
}

// If the CLI cannot be run at all, say so rather than rendering an empty page
// that looks like a host with no stacks on it.
func TestUnrunnableCLIIsReportedNotSwallowed(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)
	if err := os.Remove(filepath.Join(a.repoDir, "bin", "homelab")); err != nil {
		t.Fatal(err)
	}

	if _, err := a.stackNames(); err == nil {
		t.Fatal("stackNames() succeeded with no CLI present")
	}
	if st := a.state(context.Background()); st.StacksErr == "" {
		t.Fatal("state() did not report that the stack list could not be read")
	}
	if _, err := a.validate(context.Background(), ActionRequest{Action: ActionDeploy, Stack: "vault-mcp"}); err == nil {
		t.Fatal("validate accepted an action while the stack list was unreadable")
	}
}

func TestEnvValueReadsOnlyTheRequestedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# a comment\nIMAGE=ghcr.io/o/r:latest\nMCP_TOKEN=do-not-read-me\nQUOTED=\"x\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := envValue(path, "IMAGE"); got != "ghcr.io/o/r:latest" {
		t.Errorf("IMAGE = %q", got)
	}
	if got := envValue(path, "QUOTED"); got != "x" {
		t.Errorf("QUOTED = %q, want the quotes stripped", got)
	}
	if got := envValue(path, "MISSING"); got != "" {
		t.Errorf("MISSING = %q, want empty", got)
	}
}

// The regression test for a data race that -race in CI did not catch, because
// no test ran two mutations concurrently. TestConcurrentMutationIsRefused locks
// the mutex on the test goroutine and then calls run on that same goroutine, so
// the detector saw no concurrency at all.
//
// This one really does run them in parallel: whichever loses the TryLock reads
// a.busy while the winner is writing it.
func TestConcurrentMutationsAreRaceFree(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Result is deliberately ignored: exactly one caller wins the lock
			// and the rest are refused as busy. What is under test is that the
			// refusal path reads a.busy safely.
			_, _ = a.run(context.Background(), ActionRequest{Action: ActionRestart, Stack: "vault-mcp"})
		}()
	}
	wg.Wait()

	if got := a.loadBusy(); got != "" {
		t.Fatalf("busy = %q after every action finished, want empty", got)
	}
}

// Reads are unserialised by design, but not unbounded: without a cap, a loop of
// requests forks bash and docker compose without limit inside the container
// holding the daemon socket.
func TestReadsAreBounded(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)

	// Saturate the limiter, leaving nothing for the next caller.
	for i := 0; i < cap(a.reads); i++ {
		a.reads <- struct{}{}
	}

	res, status := a.run(context.Background(), ActionRequest{Action: ActionStatus, Stack: "vault-mcp"})
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if !strings.Contains(res.Err, "too many") {
		t.Fatalf("error = %q, want it to say why", res.Err)
	}

	// And the slot is returned afterwards, so the limiter is not a one-way door.
	<-a.reads
	if _, status := a.run(context.Background(), ActionRequest{Action: ActionStatus, Stack: "vault-mcp"}); status != http.StatusOK {
		t.Fatalf("status = %d after a slot freed, want 200", status)
	}
}

// A mutation must outlive the caller that asked for it. Closing a browser tab
// mid-deploy used to reach exec.CommandContext and SIGKILL docker compose
// partway through a pull.
func TestMutationSurvivesCallerCancellation(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)

	// A stub that takes long enough for the cancellation to land mid-run.
	stub := "#!/bin/sh\nif [ \"$1\" = \"stacks\" ]; then echo vault-mcp; exit 0; fi\nsleep 1\necho finished\n"
	if err := os.WriteFile(filepath.Join(a.repoDir, "bin", "homelab"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel() // the browser tab closes
	}()

	res, status := a.run(ctx, ActionRequest{Action: ActionRestart, Stack: "vault-mcp"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("exit=%d err=%q: the deploy was killed with its caller", res.ExitCode, res.Err)
	}
	if !strings.Contains(res.Output, "finished") {
		t.Fatalf("output = %q, want the command to have run to completion", res.Output)
	}
}

// A read, by contrast, dies with its caller -- and must be reported as
// cancelled, not as a timeout that never happened.
func TestCancelledReadIsNotReportedAsATimeout(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)

	stub := "#!/bin/sh\nif [ \"$1\" = \"stacks\" ]; then echo vault-mcp; exit 0; fi\nsleep 5\n"
	if err := os.WriteFile(filepath.Join(a.repoDir, "bin", "homelab"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	res, _ := a.run(ctx, ActionRequest{Action: ActionStatus, Stack: "vault-mcp"})
	if strings.Contains(res.Err, "timed out") {
		t.Fatalf("error = %q, but nothing timed out -- the caller hung up", res.Err)
	}
	if !strings.Contains(res.Err, "cancelled") {
		t.Fatalf("error = %q, want it to say cancelled", res.Err)
	}
}

// A command that finished successfully must not be reported as a failure just
// because its context expired on the way out.
func TestSuccessIsNotOverriddenByAnExpiredContext(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	res, status := a.run(ctx, ActionRequest{Action: ActionStatus, Stack: "vault-mcp"})
	cancel()

	if status != http.StatusOK || res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("status=%d exit=%d err=%q, want a clean success", status, res.ExitCode, res.Err)
	}
}

// --- the exposure gate ------------------------------------------------------
//
// auth.go treats Tailscale-User-Login as a credential, which it is only while
// `tailscale serve` is the sole route to the web listener. These are that
// premise asserted: get the publish wrong and the buttons must go away, because
// a header anyone can set is not authentication.

// newTestAgentRaw is newTestAgent without the compliant self container, so the
// exposure cases can supply their own.
func newTestAgentRaw(t *testing.T, containers []apiContainer) *agent {
	t.Helper()

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = \"stacks\" ]; then\nprintf '%s\\n' dashboard vault-mcp\nexit 0\nfi\necho \"argv: $*\"\n"
	if err := os.WriteFile(filepath.Join(repo, "bin", "homelab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/containers/json"):
			_ = json.NewEncoder(w).Encode(containers)
		case strings.HasPrefix(r.URL.Path, "/images/"):
			_ = json.NewEncoder(w).Encode(apiImage{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	d, err := newDockerClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return &agent{
		repoDir:     repo,
		selfStack:   "dashboard",
		selfService: "homelab-dash",
		docker:      d,
		token:       "secret",
		reads:       make(chan struct{}, 4),
	}
}

func selfContainer(ports ...apiPort) apiContainer {
	return apiContainer{
		Names:  []string{"/homelab-dash"},
		Labels: map[string]string{labelProject: "dashboard", labelService: "homelab-dash"},
		Ports:  ports,
	}
}

// agentContainer is homelab-dashd: same project, publishes nothing, and NOT the
// service that serves the page.
func agentContainer() apiContainer {
	return apiContainer{
		Names:  []string{"/homelab-dashd"},
		Labels: map[string]string{labelProject: "dashboard", labelService: "homelab-dashd"},
	}
}

func TestExposureRefusesANonLoopbackPublish(t *testing.T) {
	// Every one of these is reachable by something other than `tailscale serve`,
	// so every one of these means a forgeable identity header.
	cases := []struct {
		name string
		port apiPort
	}{
		{"wildcard IPv4", apiPort{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 8088}},
		{"wildcard IPv6", apiPort{IP: "::", PrivatePort: 8080, PublicPort: 8088}},
		{"the LAN address", apiPort{IP: "192.168.1.40", PrivatePort: 8080, PublicPort: 8088}},
		{"the tailnet address", apiPort{IP: "100.101.102.103", PrivatePort: 8080, PublicPort: 8088}},
		// Not a shape this code anticipated, so it must not be the one that
		// passes: an unrecognised binding fails closed.
		{"an unparseable host ip", apiPort{IP: "", PrivatePort: 8080, PublicPort: 8088}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAgentRaw(t, []apiContainer{selfContainer(tc.port)})

			if ex := a.exposureVerdict(context.Background()); ex.OK {
				t.Fatalf("exposure accepted a publish on %q", tc.port.IP)
			}
			for _, action := range []Action{ActionDeploy, ActionRestart, ActionLogs, ActionURL} {
				req := ActionRequest{Action: action, Stack: "vault-mcp"}
				if _, err := a.validate(context.Background(), req); err == nil {
					t.Errorf("%s was accepted while published on %q", action, tc.port.IP)
				}
			}
		})
	}
}

// An exposed stack must stay READABLE. The page is deliberately open, so the
// verbs that tell a caller no more than the page does keep working -- otherwise
// the one thing that would explain the problem is also the thing that breaks.
func TestExposureStillAllowsOpenReads(t *testing.T) {
	a := newTestAgentRaw(t, []apiContainer{
		selfContainer(apiPort{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 8088}),
	})

	for _, action := range []Action{ActionStatus, ActionPs, ActionEnvCheck} {
		if _, err := a.validate(context.Background(), ActionRequest{Action: action, Stack: "vault-mcp"}); err != nil {
			t.Errorf("%s was refused on an exposed stack: %v", action, err)
		}
	}
}

func TestExposureAcceptsLoopbackOnly(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1"} {
		a := newTestAgentRaw(t, []apiContainer{
			selfContainer(apiPort{IP: ip, PrivatePort: 8080, PublicPort: 8088}),
		})
		if ex := a.exposureVerdict(context.Background()); !ex.OK {
			t.Errorf("exposure refused a %s publish: %s", ip, ex.Reason)
		}
	}
}

// A port that is exposed but never published is not reachable from the host at
// all, which is the safest case there is.
func TestExposureIgnoresUnpublishedPorts(t *testing.T) {
	a := newTestAgentRaw(t, []apiContainer{
		selfContainer(apiPort{IP: "", PrivatePort: 8090, PublicPort: 0}),
	})
	if ex := a.exposureVerdict(context.Background()); !ex.OK {
		t.Fatalf("exposure refused a container that publishes nothing: %s", ex.Reason)
	}
}

// "I could not find my own containers" is not "I am not exposed". It means the
// bindings were read from the wrong set, and the honest answer is to refuse.
func TestExposureFailsClosedWhenItCannotCheck(t *testing.T) {
	a := newTestAgentRaw(t, nil)
	if ex := a.exposureVerdict(context.Background()); ex.OK {
		t.Fatal("exposure passed with no containers in its own stack")
	}

	a.allowUnverified = true
	if ex := a.exposureVerdict(context.Background()); !ex.OK {
		t.Fatal("DASH_ALLOW_UNVERIFIED_EXPOSURE did not permit the unverifiable case")
	}
}

// THE HOLE THIS CHECK SHIPPED WITH, kept as a test so it cannot come back.
//
// The guard used to ask whether ANY container of this project was present.
// homelab-dashd publishes nothing, so a listing containing only the agent gave
// a clean verdict having examined no web listener at all -- while a web role
// started outside compose, unlabelled and bound to 0.0.0.0, would not appear
// here to contradict it. Seeing the agent is not seeing the thing that serves
// the page.
func TestExposureRefusesWhenTheWebContainerIsMissing(t *testing.T) {
	a := newTestAgentRaw(t, []apiContainer{agentContainer()})

	ex := a.exposureVerdict(context.Background())
	if ex.OK {
		t.Fatal("exposure passed on a listing that contains only the agent container")
	}
	if !strings.Contains(ex.Reason, "homelab-dash") {
		t.Fatalf("the refusal does not name the service it could not find: %s", ex.Reason)
	}
	for _, action := range []Action{ActionDeploy, ActionRestart, ActionLogs, ActionURL} {
		if _, err := a.validate(context.Background(), ActionRequest{Action: action, Stack: "vault-mcp"}); err == nil {
			t.Errorf("%s was accepted while the web container was unexamined", action)
		}
	}
}

// The agent container being alongside the web one is the normal case and must
// not itself be the thing that satisfies the check.
func TestExposureIsSatisfiedByTheWebContainerNotItsNeighbours(t *testing.T) {
	a := newTestAgentRaw(t, []apiContainer{
		agentContainer(),
		selfContainer(apiPort{IP: "127.0.0.1", PrivatePort: 8080, PublicPort: 8088}),
	})
	if ex := a.exposureVerdict(context.Background()); !ex.OK {
		t.Fatalf("the normal two-container arrangement was refused: %s", ex.Reason)
	}
}

// A bad binding is the more urgent answer and must not be masked by the web
// container also being absent.
func TestAPositivelyBadBindingWinsOverUnverifiable(t *testing.T) {
	a := newTestAgentRaw(t, []apiContainer{{
		Names:  []string{"/homelab-dashd"},
		Labels: map[string]string{labelProject: "dashboard", labelService: "homelab-dashd"},
		Ports:  []apiPort{{IP: "0.0.0.0", PrivatePort: 8090, PublicPort: 8090}},
	}})
	a.allowUnverified = true

	ex := a.exposureVerdict(context.Background())
	if ex.OK {
		t.Fatal("a LAN-published container passed because the verdict was also unverifiable")
	}
	if len(ex.Bindings) == 0 {
		t.Fatal("the refusal does not name the offending binding")
	}
}

// The override exists for "cannot tell", never for "definitely exposed". A knob
// that unlocked a known-open deploy button would be the hole it is meant to
// prevent, spelled as a setting.
func TestUnverifiedOverrideCannotUnlockAKnownExposure(t *testing.T) {
	a := newTestAgentRaw(t, []apiContainer{
		selfContainer(apiPort{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 8088}),
	})
	a.allowUnverified = true

	if ex := a.exposureVerdict(context.Background()); ex.OK {
		t.Fatal("DASH_ALLOW_UNVERIFIED_EXPOSURE unlocked a stack that is positively published to the LAN")
	}
	if _, err := a.validate(context.Background(), ActionRequest{Action: ActionDeploy, Stack: "vault-mcp"}); err == nil {
		t.Fatal("deploy was accepted on a positively exposed stack with the override set")
	}
}

// Only this stack's own publishing decides the verdict. vault-mcp has no
// published port and obsidian-vault has none either, but if one ever gains one
// that is not a reason to disarm the dashboard.
func TestExposureJudgesOnlyItsOwnStack(t *testing.T) {
	a := newTestAgentRaw(t, []apiContainer{
		selfContainer(apiPort{IP: "127.0.0.1", PrivatePort: 8080, PublicPort: 8088}),
		{
			Names:  []string{"/some-other-thing"},
			Labels: map[string]string{labelProject: "vault-mcp", labelService: "server"},
			Ports:  []apiPort{{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080}},
		},
	})
	if ex := a.exposureVerdict(context.Background()); !ex.OK {
		t.Fatalf("another stack's published port disarmed the dashboard: %s", ex.Reason)
	}
}
