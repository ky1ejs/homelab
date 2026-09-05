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

func TestUptimeIsUptimeAndNotAge(t *testing.T) {
	cases := map[string]string{
		"Up 3 days (healthy)":             "3 days",
		"Up 2 minutes (health: starting)": "2 minutes",
		"Up 5 seconds":                    "5 seconds",
		"Up About a minute":               "About a minute",
		"Restarting (1) 4 seconds ago":    "Restarting (1) 4 seconds ago",
		"Exited (1) 2 hours ago":          "Exited (1) 2 hours ago",
		"Created":                         "Created",
		"":                                "",
	}
	for status, want := range cases {
		if got := uptime(status); got != want {
			t.Errorf("uptime(%q) = %q, want %q", status, got, want)
		}
	}
}

// The crash loop this column was changed for: an agent restarting every few
// seconds used to render as "3d" in a green row, because the cell showed the
// container's creation time and a restart does not change it.
func TestCrashLoopingContainerDoesNotRenderAsUpForDays(t *testing.T) {
	tmpl := mustTemplate(t)

	var sb strings.Builder
	err := tmpl.Execute(&sb, pageData{
		Now: time.Now(),
		State: State{Stacks: []Stack{{
			Name: "obsidian-vault",
			Containers: []Container{{
				Name: "vault-claude", Service: "vault-claude", State: "running",
				Status:  "Up 4 seconds",
				Created: time.Now().Add(-72 * time.Hour).Unix(),
			}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	page := sb.String()
	if !strings.Contains(page, "4 seconds") {
		t.Error("the page did not show how long the container had actually been up")
	}
	if strings.Contains(page, ">3d<") {
		t.Error("the up column still reports the container's age as its uptime")
	}
}

func TestUptimeClassWarnsOnlyForSomethingRunningAndYoung(t *testing.T) {
	cases := []struct {
		state, status, want string
	}{
		{"running", "Up 4 seconds", "warn"},
		{"running", "Up 3 days (healthy)", "muted"},
		{"running", "Up 2 minutes", "muted"},
		// The word appears, but in a container that is not running: the state
		// column is already saying that, in red.
		{"exited", "Exited (1) 30 seconds ago", "muted"},
		{"restarting", "Restarting (1) 4 seconds ago", "muted"},
	}
	for _, c := range cases {
		if got := uptimeClass(c.state, c.status); got != c.want {
			t.Errorf("uptimeClass(%q, %q) = %q, want %q", c.state, c.status, got, c.want)
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
func newTestWeb(t *testing.T, allowed string, readOnly bool) (*web, *[]ActionRequest) {
	t.Helper()

	var seen []ActionRequest
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ActionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, req)
		_ = json.NewEncoder(w).Encode(ActionResult{Action: req.Action, Stack: req.Stack, Output: "done"})
	}))
	t.Cleanup(agentSrv.Close)

	auth, err := newAuthenticator(allowed, false, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	return &web{
		agent: newAgentClient(agentSrv.URL, "agent-token"),
		auth:  auth,
	}, &seen
}

// signedInRequest is a POST as `tailscale serve` would deliver it from the page.
func signedInRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(body))
	r.Header.Set(identityHeader, "kyle@example.com")
	r.Header.Set(csrfHeader, "1")
	return r
}

func TestMutatingActionsNeedAuth(t *testing.T) {
	w, seen := newTestWeb(t, "kyle@example.com", false)

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

// The identity header alone is not enough. Without the action header this is a
// shape a cross-site request could produce, and it is now the only thing
// stopping one.
func TestMutatingActionsNeedTheActionHeaderToo(t *testing.T) {
	w, seen := newTestWeb(t, "kyle@example.com", false)

	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(`{"action":"deploy","stack":"vault-mcp"}`))
	req.Header.Set(identityHeader, "kyle@example.com")
	rec := httptest.NewRecorder()
	w.handleAction(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a deploy with identity but no %s got %d, want 403", csrfHeader, rec.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("the agent was called anyway: %+v", *seen)
	}
}

// "No auth" means no identity. The action header is required of every verb --
// see TestEveryActionNeedsTheActionHeader -- so this carries it and nothing else.
func TestReadActionsDoNotNeedAuth(t *testing.T) {
	w, seen := newTestWeb(t, "kyle@example.com", false)

	req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(`{"action":"status","stack":"vault-mcp"}`))
	req.Header.Set(csrfHeader, "1")
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
	w, seen := newTestWeb(t, "kyle@example.com", false)

	rec := httptest.NewRecorder()
	w.handleAction(rec, signedInRequest(`{"action":"deploy","stack":"vault-mcp"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("authorised deploy got %d, want 200", rec.Code)
	}
	if len(*seen) != 1 || (*seen)[0].Action != ActionDeploy {
		t.Fatalf("agent saw %+v", *seen)
	}
}

// Read-only mode must say why, not just refuse.
func TestReadOnlyModeExplainsItself(t *testing.T) {
	w, _ := newTestWeb(t, "", true)

	rec := httptest.NewRecorder()
	w.handleAction(rec, signedInRequest(`{"action":"deploy","stack":"vault-mcp"}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "DASH_READ_ONLY") {
		t.Fatalf("the refusal does not name DASH_READ_ONLY: %s", rec.Body.String())
	}
}

// A refusal has to say WHICH thing is wrong. All three of these are
// misconfigurations someone has to go and fix, and "forbidden" would send them
// looking in the wrong place.
func TestRefusalsNameTheirCause(t *testing.T) {
	w, _ := newTestWeb(t, "kyle@example.com", false)

	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"no identity", map[string]string{csrfHeader: "1"}, identityHeader},
		{"wrong login", map[string]string{identityHeader: "stranger@example.com", csrfHeader: "1"}, "DASH_ALLOWED_LOGINS"},
		{"funnelled", map[string]string{identityHeader: "kyle@example.com", csrfHeader: "1", funnelHeader: "true"}, "Funnel"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(`{"action":"deploy","stack":"vault-mcp"}`))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			w.handleAction(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("the refusal does not mention %q: %s", tc.want, rec.Body.String())
			}
		})
	}
}

func TestUnknownActionIsRejectedBeforeTheAgent(t *testing.T) {
	w, seen := newTestWeb(t, "kyle@example.com", false)

	req := signedInRequest(`{"action":"exec","stack":"vault-mcp"}`)
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
	w, seen := newTestWeb(t, "kyle@example.com", false)

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
		"exposed to the lan": {
			Now: time.Now(),
			State: State{
				Exposure: Exposure{Reason: "this stack publishes a port beyond loopback",
					Bindings: []string{"homelab-dash 0.0.0.0:8088->8080"}},
			},
		},
		"no tailnet identity": {
			Now:         time.Now(),
			IdentityErr: "no Tailscale-User-Login header",
			State:       State{Exposure: Exposure{OK: true}},
		},
		"checkout behind": {
			SignedIn: true, Login: "kyle@example.com", Now: time.Now(),
			State: State{
				Exposure: Exposure{OK: true},
				Checkout: Checkout{Head: "0123456789abcdef0123456789abcdef01234567", Branch: "main", Upstream: "main", Slug: "ky1ejs/homelab",
					Behind: &BehindStatus{Status: "behind", Count: 2, Stacks: []string{"vault-mcp"},
						Commits: []CommitSummary{{SHA: "aaaa", Subject: "a change"}, {SHA: "bbbb", Subject: "another"}}}},
			},
		},
		"checkout behind, truncated": {
			Now: time.Now(),
			State: State{
				Exposure: Exposure{OK: true},
				Checkout: Checkout{Head: "0123456789abcdef0123456789abcdef01234567", Branch: "main", Upstream: "main",
					Behind: &BehindStatus{Status: "behind", Count: 400, Truncated: true,
						Commits: []CommitSummary{{SHA: "aaaa", Subject: "one of many"}}}},
			},
		},
		"checkout tracks nothing": {
			Now: time.Now(),
			State: State{
				Exposure: Exposure{OK: true},
				Checkout: Checkout{Head: "0123456789abcdef0123456789abcdef01234567", Branch: "hotfix",
					Behind: &BehindStatus{Status: "no-upstream", Err: "\"hotfix\" tracks no branch on origin"}},
			},
		},
		"checkout diverged": {
			Now: time.Now(),
			State: State{
				Exposure: Exposure{OK: true},
				Checkout: Checkout{Head: "0123456789abcdef0123456789abcdef01234567", Branch: "main",
					Behind: &BehindStatus{Status: "diverged", Count: 1}},
			},
		},
		"checkout unreadable": {
			Now:   time.Now(),
			State: State{Exposure: Exposure{OK: true}, Checkout: Checkout{Err: "no .git in /repo: not a checkout"}},
		},
		"full": {
			SignedIn: true,
			Login:    "kyle@example.com",
			Now:      time.Now(),
			State: State{
				TakenAt:  time.Now(),
				Exposure: Exposure{OK: true, Bindings: []string{"homelab-dash 127.0.0.1:8088->8080"}},
				Checkout: Checkout{Head: "0123456789abcdef0123456789abcdef01234567", Branch: "main", Slug: "ky1ejs/homelab",
					Behind: &BehindStatus{Status: "current"}},
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
			// The page never shows a secret because there is no longer one to
			// show, and it must not start echoing the identity header into an
			// attribute either.
			if strings.Contains(sb.String(), "type=\"password\"") {
				t.Error("the page still draws a credential field")
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

// Actions that hand back content need an identity even though they change
// nothing. `logs obsidian-vault vault-claude` returns the always-on Claude
// agent's output, which is vault content -- not the container list the open
// page already shows.
func TestSensitiveReadsNeedAuth(t *testing.T) {
	for _, body := range []string{
		`{"action":"logs","stack":"obsidian-vault","service":"vault-claude"}`,
		`{"action":"url","stack":"vault-mcp"}`,
	} {
		t.Run(body, func(t *testing.T) {
			w, seen := newTestWeb(t, "kyle@example.com", false)

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

// ...and still work for an allowed tailnet login.
func TestSensitiveReadsWorkOnceIdentified(t *testing.T) {
	w, seen := newTestWeb(t, "kyle@example.com", false)

	req := signedInRequest(`{"action":"logs","stack":"obsidian-vault","service":"vault-claude"}`)
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
// Open means "needs no identity", NOT "needs no action header" -- see the next
// test. These carry the header and no identity at all.
func TestHarmlessReadsStayOpen(t *testing.T) {
	for _, body := range []string{
		`{"action":"status","stack":"vault-mcp"}`,
		`{"action":"ps","stack":"vault-mcp"}`,
		`{"action":"env-check","stack":"vault-mcp"}`,
	} {
		w, _ := newTestWeb(t, "kyle@example.com", false)
		req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(body))
		req.Header.Set(csrfHeader, "1")
		rec := httptest.NewRecorder()
		w.handleAction(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s got %d, want 200", body, rec.Code)
		}
	}
}

// EVERY action needs the header, including the ungated ones.
//
// Without this the endpoint accepts a CORS simple request: the body is never
// checked for Content-Type, so a cross-origin form post of
// {"action":"preflight","stack":"dashboard"} needs no preflight and the browser
// sends it. The attacker cannot read the response, but each of these verbs forks
// bash and docker compose inside the container holding the daemon socket, and
// DASH_MAX_READS is 4 -- so a page a tailnet user visits could hold the read
// pool shut and make every button on the dashboard return 429.
func TestEveryActionNeedsTheActionHeader(t *testing.T) {
	for _, body := range []string{
		`{"action":"status","stack":"vault-mcp"}`,
		`{"action":"preflight","stack":"dashboard"}`,
		`{"action":"env-check","stack":"vault-mcp"}`,
		`{"action":"deploy","stack":"vault-mcp"}`,
	} {
		w, seen := newTestWeb(t, "kyle@example.com", false)
		req := httptest.NewRequest(http.MethodPost, "/action", strings.NewReader(body))
		req.Header.Set(identityHeader, "kyle@example.com")
		rec := httptest.NewRecorder()
		w.handleAction(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without %s got %d, want 403", body, csrfHeader, rec.Code)
		}
		if len(*seen) != 0 {
			t.Errorf("%s reached the agent without %s: %+v", body, csrfHeader, *seen)
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
