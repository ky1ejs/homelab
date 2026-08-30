package main

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthFromStatus(t *testing.T) {
	cases := map[string]string{
		"Up 3 days (healthy)":             "healthy",
		"Up 2 minutes (health: starting)": "starting",
		"Up 6 hours (unhealthy)":          "unhealthy",
		"Up 3 days":                       "",
		"Exited (0) 2 hours ago":          "",
		"Created":                         "",
		"":                                "",
		"Restarting (1) 5 seconds ago":    "",
	}
	for status, want := range cases {
		if got := healthFromStatus(status); got != want {
			t.Errorf("healthFromStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestContainerName(t *testing.T) {
	if got := containerName([]string{"/vault-sync"}); got != "vault-sync" {
		t.Errorf("got %q", got)
	}
	if got := containerName(nil); got != "" {
		t.Errorf("got %q for no names", got)
	}
}

// The badge has to answer for the stack's OWN image. obsidian-vault runs three
// containers off one image and vault-mcp runs its server next to a pinned
// Tailscale sidecar, so picking the first container's digest would sometimes
// compare the sidecar against the server's tag.
func TestRunningDigestPicksTheStacksOwnImage(t *testing.T) {
	s := &Stack{
		Image: "ghcr.io/ky1ejs/homelab/vault-mcp:latest",
		Containers: []Container{
			{Name: "vault-mcp-funnel", Image: "tailscale/tailscale:v1.102.2", RepoDigest: "sha256:sidecar"},
			{Name: "vault-mcp", Image: "ghcr.io/ky1ejs/homelab/vault-mcp:latest", RepoDigest: "sha256:server"},
		},
	}
	if got := runningDigest(s); got != "sha256:server" {
		t.Fatalf("runningDigest = %q, want the server's digest", got)
	}

	// A stack whose own image is not running yet has nothing to compare.
	s.Containers = s.Containers[:1]
	if got := runningDigest(s); got != "" {
		t.Fatalf("runningDigest = %q, want empty", got)
	}
}

// newTestWeb wires a web role to a stub agent, so the handler's authorisation
// can be tested without a Docker socket anywhere.
func newTestWeb(t *testing.T, token string) (*web, *[]ActionRequest) {
	t.Helper()

	var seen []ActionRequest
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ActionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req)
		_ = json.NewEncoder(w).Encode(ActionResult{Action: req.Action, Stack: req.Stack, Output: "done"})
	}))
	t.Cleanup(agentSrv.Close)

	return &web{
		agent: newAgentClient(agentSrv.URL, "agent-token"),
		auth:  newAuthenticator(token, time.Hour),
	}, &seen
}

func TestMutatingActionsNeedAuth(t *testing.T) {
	w, seen := newTestWeb(t, "s3cret")

	body := `{"action":"deploy","stack":"vault-mcp"}`
	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(body))
	rec := httptest.NewRecorder()
	w.handleAction(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated deploy got %d, want 403", rec.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("the agent was called anyway: %+v", *seen)
	}
}

func TestReadActionsDoNotNeedAuth(t *testing.T) {
	w, seen := newTestWeb(t, "s3cret")

	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(`{"action":"status","stack":"vault-mcp"}`))
	rec := httptest.NewRecorder()
	w.handleAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status got %d, want 200", rec.Code)
	}
	if len(*seen) != 1 || (*seen)[0].Action != ActionStatus {
		t.Fatalf("agent saw %+v", *seen)
	}
}

func TestAuthenticatedMutationReachesTheAgent(t *testing.T) {
	w, seen := newTestWeb(t, "s3cret")

	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(`{"action":"deploy","stack":"vault-mcp"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	w.handleAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authorised deploy got %d, want 200", rec.Code)
	}
	if len(*seen) != 1 || (*seen)[0].Action != ActionDeploy {
		t.Fatalf("agent saw %+v", *seen)
	}
}

// Read-only mode must say why, not just refuse.
func TestReadOnlyModeExplainsItself(t *testing.T) {
	w, _ := newTestWeb(t, "")

	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(`{"action":"deploy","stack":"vault-mcp"}`))
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	w.handleAction(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "DASH_TOKEN") {
		t.Fatalf("the refusal does not mention DASH_TOKEN: %s", rec.Body.String())
	}
}

func TestUnknownActionIsRejectedBeforeTheAgent(t *testing.T) {
	w, seen := newTestWeb(t, "s3cret")

	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(`{"action":"exec","stack":"vault-mcp"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	w.handleAction(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("an unknown action was forwarded: %+v", *seen)
	}
}

func TestGETCannotRunAnAction(t *testing.T) {
	w, seen := newTestWeb(t, "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/action?action=deploy&stack=vault-mcp", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	w.handleAction(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", rec.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("a GET reached the agent: %+v", *seen)
	}
}

// The page must render every state, including the ones where nothing works --
// "the agent is down" is one of the more useful things it can say.
func TestPageRenders(t *testing.T) {
	tmpl := mustTemplate(t)

	cases := map[string]pageData{
		"empty":      {},
		"read-only":  {ReadOnly: true, Now: time.Now()},
		"agent down": {Err: "connection refused", Now: time.Now()},
		"docker down": {
			Now:   time.Now(),
			State: State{DockerErr: "permission denied", Stacks: []Stack{{Name: "vault-mcp"}}},
		},
		"stack list unreadable": {
			Now:   time.Now(),
			State: State{StacksErr: "cannot list stacks: fork/exec: no such file"},
		},
		"unmanaged container behind": {
			Now: time.Now(),
			State: State{Unmanaged: []Container{{
				Name: "home-assistant", Image: "ghcr.io/home-assistant/home-assistant:stable",
				State: "running", Update: &UpdateStatus{State: UpdateAvailable, Running: "sha256:aaaa", Latest: "sha256:bbbb"},
			}}},
		},
		"full": {
			SignedIn: true,
			Now:      time.Now(),
			State: State{
				TakenAt: time.Now(),
				Stacks: []Stack{
					{
						Name: "obsidian-vault", EnvPresent: true, EnvMode: "600",
						Image: "ghcr.io/o/homelab/obsidian-vault:latest", HasPreflight: true,
						Update:     &UpdateStatus{State: UpdateAvailable, Running: "sha256:aaaa", Latest: "sha256:bbbb"},
						Containers: []Container{{Name: "vault-sync", Service: "vault-sync", State: "running", Status: "Up 3 days (healthy)", Health: "healthy"}},
					},
					{Name: "dashboard", Self: true, EnvPresent: true, EnvMode: "644"},
				},
				Unmanaged: []Container{{Name: "home-assistant", Image: "ghcr.io/ha:stable", State: "running"}},
			},
		},
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var sb strings.Builder
			if err := tmpl.Execute(&sb, data); err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.Contains(sb.String(), "homelab") {
				t.Error("the page did not render its own name")
			}
		})
	}
}

// Containers this repo does not manage still get a version check. Home
// Assistant is the case this exists for: nothing here can deploy it, but being
// three releases behind is worth seeing from the same page.
func TestUnmanagedContainersGetAnUpdateCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:newer")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	rc := newRegistryClient(time.Minute)
	rc.scheme = "http"
	web := &web{registry: rc}

	st := State{Unmanaged: []Container{
		{Name: "home-assistant", Image: host + "/home-assistant/home-assistant:stable", RepoDigest: "sha256:older"},
		{Name: "esphome", Image: host + "/esphome/esphome:latest", RepoDigest: "sha256:newer"},
		// No image reference at all must not panic or invent an answer.
		{Name: "mystery"},
	}}
	web.annotate(context.Background(), &st)

	if got := st.Unmanaged[0].Update; got == nil || got.State != UpdateAvailable {
		t.Errorf("home-assistant update = %+v, want available", got)
	}
	if got := st.Unmanaged[1].Update; got == nil || got.State != UpdateCurrent {
		t.Errorf("esphome update = %+v, want current", got)
	}
	if got := st.Unmanaged[2].Update; got != nil {
		t.Errorf("a container with no image got %+v, want no verdict", got)
	}
}

// Command output and container names are text, never markup. One of the stacks
// on this host is an agent that writes files; the page must not execute what it
// is shown.
func TestPageEscapesUntrustedText(t *testing.T) {
	tmpl := mustTemplate(t)

	var sb strings.Builder
	err := tmpl.Execute(&sb, pageData{
		Now: time.Now(),
		State: State{Stacks: []Stack{{
			Name:       "vault-mcp",
			Image:      `ghcr.io/o/r:latest"><script>alert(1)</script>`,
			Containers: []Container{{Name: `<img src=x onerror=alert(1)>`, State: "running"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "<script>alert(1)</script>") {
		t.Error("a script tag survived into the page")
	}
	if strings.Contains(sb.String(), "<img src=x onerror=") {
		t.Error("an img tag survived into the page")
	}
}

func mustTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := parseUI()
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// Actions that hand back content need the token even though they change
// nothing. `logs obsidian-vault vault-claude` returns the always-on Claude
// agent's output, which is vault content -- not the container list the open
// page already shows.
func TestSensitiveReadsNeedAuth(t *testing.T) {
	for _, body := range []string{
		`{"action":"logs","stack":"obsidian-vault","service":"vault-claude"}`,
		`{"action":"url","stack":"vault-mcp"}`,
	} {
		t.Run(body, func(t *testing.T) {
			w, seen := newTestWeb(t, "s3cret")

			req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(body))
			rec := httptest.NewRecorder()
			w.handleAction(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("unauthenticated request got %d, want 403", rec.Code)
			}
			if len(*seen) != 0 {
				t.Fatalf("it reached the agent anyway: %+v", *seen)
			}
		})
	}
}

// ...and still work once signed in.
func TestSensitiveReadsWorkWithTheToken(t *testing.T) {
	w, seen := newTestWeb(t, "s3cret")

	req := httptest.NewRequest(http.MethodPost, "/action",
		strings.NewReader(`{"action":"logs","stack":"obsidian-vault","service":"vault-claude"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	w.handleAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if len(*seen) != 1 || (*seen)[0].Service != "vault-claude" {
		t.Fatalf("agent saw %+v, want the service forwarded", *seen)
	}
}

// The status page stays open: it is the thing you glance at.
func TestHarmlessReadsStayOpen(t *testing.T) {
	for _, body := range []string{
		`{"action":"status","stack":"vault-mcp"}`,
		`{"action":"ps","stack":"vault-mcp"}`,
		`{"action":"env-check","stack":"vault-mcp"}`,
	} {
		w, _ := newTestWeb(t, "s3cret")
		req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(body))
		rec := httptest.NewRecorder()
		w.handleAction(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s got %d, want 200", body, rec.Code)
		}
	}
}

// A listen address with no colon must be reported, not panicked on.
func TestProbeSelfRejectsAMalformedAddress(t *testing.T) {
	t.Setenv("DASH_ADDR", "8080") // a bare port: the likeliest way to get it wrong

	err := probeSelf("web")
	if err == nil {
		t.Fatal("a colon-less DASH_ADDR was accepted")
	}
	if !strings.Contains(err.Error(), "8080") {
		t.Fatalf("error = %q, want it to name the offending value", err)
	}
}
