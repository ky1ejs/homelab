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
	"testing"
)

// newTestAgent builds an agent over a throwaway checkout containing the given
// stacks, plus a fake Docker daemon serving the given containers.
func newTestAgent(t *testing.T, stacks []string, containers []apiContainer) *agent {
	t.Helper()

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real executable, so a test that reaches exec actually runs something
	// harmless and observable instead of failing on a missing file.
	script := "#!/bin/sh\necho \"argv: $*\"\n"
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

	return &agent{repoDir: repo, selfStack: "dashboard", docker: d, token: "secret"}
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
			if _, err := a.validate(tc.req); err == nil {
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
		if _, err := a.validate(req); err != nil {
			t.Errorf("validate rejected %+v: %v", req, err)
		}
	}
}

// The dashboard must not be able to deploy or restart itself: the command would
// kill the process running it partway through and report nothing.
func TestSelfStackIsNotMutable(t *testing.T) {
	a := newTestAgent(t, []string{"dashboard", "vault-mcp"}, nil)

	for _, action := range []Action{ActionDeploy, ActionRestart} {
		if _, err := a.validate(ActionRequest{Action: action, Stack: "dashboard"}); err == nil {
			t.Errorf("%s against the dashboard's own stack was accepted", action)
		}
	}
	// Reading about itself is fine and useful.
	if _, err := a.validate(ActionRequest{Action: ActionStatus, Stack: "dashboard"}); err != nil {
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
	if err := os.WriteFile(filepath.Join(a.repoDir, "bin", "homelab"), []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
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

// Stacks come from the checkout, so a directory without a compose file is not
// one and a new stack needs no edit here.
func TestStackDiscovery(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp", "fishing"}, nil)
	if err := os.MkdirAll(filepath.Join(a.repoDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(a.stackNames(), ",")
	if got != "fishing,vault-mcp" {
		t.Fatalf("stackNames() = %q, want \"fishing,vault-mcp\"", got)
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
