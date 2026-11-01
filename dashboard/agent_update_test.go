// The agent's half of the updater: the update verbs pass the same gate as every
// other verb, take the same lock as a deploy, and reach the updater with the
// updater's token -- never the agent's own.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

type updaterCall struct {
	path, auth string
	body       updateRequest
}

// withFakeUpdater points the agent at a stub updater that records each call and
// answers with out.
func withFakeUpdater(t *testing.T, a *agent, out UpdateOutcome) *[]updaterCall {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []updaterCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body updateRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, updaterCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	a.updater = newUpdaterClient(srv.URL, "updater-secret")
	return &calls
}

func TestUpdateVerbsRejectHostileInput(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp", "dashboard"}, nil)
	withFakeUpdater(t, a, UpdateOutcome{})

	cases := map[string]ActionRequest{
		"preview with a stack":       {Action: ActionUpdatePreview, Stack: "vault-mcp"},
		"preview with a service":     {Action: ActionUpdatePreview, Service: "vault-sync"},
		"preview with a commit":      {Action: ActionUpdatePreview, Commit: testCommit},
		"apply with no commit":       {Action: ActionUpdateApply},
		"apply with a short sha":     {Action: ActionUpdateApply, Commit: testCommit[:7]},
		"apply with uppercase hex":   {Action: ActionUpdateApply, Commit: strings.ToUpper(testCommit)},
		"apply with a ref name":      {Action: ActionUpdateApply, Commit: "origin/main"},
		"apply with HEAD":            {Action: ActionUpdateApply, Commit: "HEAD"},
		"apply with a flag":          {Action: ActionUpdateApply, Commit: "--force" + testCommit[7:]},
		"apply with a trailing line": {Action: ActionUpdateApply, Commit: testCommit + "\n"},
		"apply with a stack":         {Action: ActionUpdateApply, Commit: testCommit, Stack: "vault-mcp"},
		"commit on a deploy":         {Action: ActionDeploy, Stack: "vault-mcp", Commit: testCommit},
		"commit on a read":           {Action: ActionStatus, Stack: "vault-mcp", Commit: testCommit},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := a.validate(context.Background(), req); err == nil {
				t.Fatalf("validate accepted %+v", req)
			}
		})
	}
}

func TestUpdateVerbsAcceptTheRealThing(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)
	withFakeUpdater(t, a, UpdateOutcome{})
	for _, req := range []ActionRequest{
		{Action: ActionUpdatePreview},
		{Action: ActionUpdateApply, Commit: testCommit},
	} {
		if _, err := a.validate(context.Background(), req); err != nil {
			t.Errorf("validate rejected %+v: %v", req, err)
		}
	}
}

func TestUpdateVerbsSayWhenTheUpdaterIsNotConfigured(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)
	_, err := a.validate(context.Background(), ActionRequest{Action: ActionUpdatePreview})
	if err == nil || !strings.Contains(err.Error(), "DASH_UPDATER_URL") {
		t.Fatalf("err = %v, want it to name DASH_UPDATER_URL", err)
	}
}

// The checkout must not move under a deploy that is reading it.
func TestUpdateApplyWaitsForTheDeployLock(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)
	calls := withFakeUpdater(t, a, UpdateOutcome{Status: UpdateStatusApplied})

	a.mu.Lock()
	a.busy = "deploy vault-mcp"
	defer a.mu.Unlock()

	res, status := a.run(context.Background(), ActionRequest{Action: ActionUpdateApply, Commit: testCommit})
	if status != http.StatusConflict || !strings.Contains(res.Err, "deploy vault-mcp") {
		t.Fatalf("got %d %q, want 409 naming the deploy", status, res.Err)
	}
	if len(*calls) != 0 {
		t.Fatalf("the updater was called while a deploy held the lock: %+v", *calls)
	}

	// A preview changes nothing a deploy reads, so it is not held up.
	if _, status := a.run(context.Background(), ActionRequest{Action: ActionUpdatePreview}); status != http.StatusOK {
		t.Fatalf("preview during a deploy got %d, want 200", status)
	}
}

func TestUpdateVerbsUseTheUpdaterToken(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)
	calls := withFakeUpdater(t, a, UpdateOutcome{
		Status:  UpdateStatusBehind,
		Head:    strings.Repeat("1", 40),
		Target:  testCommit,
		Count:   1,
		Commits: []CommitSummary{{SHA: testCommit, Subject: "Bump vault-mcp"}},
		Stacks:  []string{"vault-mcp"},
		Signature: &SignatureVerdict{
			OK: true, Signer: "GitHub <noreply@github.com>", Fingerprint: "968479A1AFF927E37D1A566BB5690EEEBB952194",
		},
	})

	res, _ := a.run(context.Background(), ActionRequest{Action: ActionUpdatePreview})
	if res.Err != "" || res.Update == nil || res.Update.Target != testCommit {
		t.Fatalf("preview result = %+v", res)
	}
	if !strings.Contains(res.Output, "Bump vault-mcp") || !strings.Contains(res.Output, "Signature: good") {
		t.Fatalf("output = %q, want the commit and the signature verdict", res.Output)
	}

	a.run(context.Background(), ActionRequest{Action: ActionUpdateApply, Commit: testCommit})

	if len(*calls) != 2 {
		t.Fatalf("updater saw %d calls, want 2", len(*calls))
	}
	for _, c := range *calls {
		if c.auth != "Bearer updater-secret" {
			t.Errorf("%s sent Authorization %q, want the updater's token", c.path, c.auth)
		}
	}
	if (*calls)[0].path != "/v1/preview" || (*calls)[0].body.Commit != "" {
		t.Errorf("preview call = %+v", (*calls)[0])
	}
	if (*calls)[1].path != "/v1/apply" || (*calls)[1].body.Commit != testCommit {
		t.Errorf("apply call = %+v", (*calls)[1])
	}
}

func TestUpdaterRefusalIsAFailedAction(t *testing.T) {
	a := newTestAgent(t, []string{"vault-mcp"}, nil)
	withFakeUpdater(t, a, UpdateOutcome{Err: "origin/main is now abc, not the def you previewed: preview again"})

	res, _ := a.run(context.Background(), ActionRequest{Action: ActionUpdateApply, Commit: testCommit})
	if res.ExitCode == 0 || !strings.Contains(res.Err, "preview again") {
		t.Fatalf("result = %+v, want a failure carrying the updater's reason", res)
	}
}

// The exposure premise gates the update verbs exactly as it gates a deploy.
func TestUpdateVerbsNeedTheExposurePremise(t *testing.T) {
	a := newTestAgentRaw(t, []apiContainer{selfContainer(apiPort{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 8088})})
	calls := withFakeUpdater(t, a, UpdateOutcome{})
	for _, req := range []ActionRequest{
		{Action: ActionUpdatePreview},
		{Action: ActionUpdateApply, Commit: testCommit},
	} {
		if _, err := a.validate(context.Background(), req); err == nil {
			t.Errorf("%s accepted with the web port on 0.0.0.0", req.Action)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("the updater was reached: %+v", *calls)
	}
}

func TestAgentRefusesASharedUpdaterToken(t *testing.T) {
	repo := newTestAgent(t, []string{"vault-mcp"}, nil).repoDir
	t.Setenv("REPO_DIR", repo)
	t.Setenv("DASH_AGENT_TOKEN", "same")
	t.Setenv("DASH_UPDATER_URL", "http://homelab-dashu:8091")
	t.Setenv("DASH_UPDATER_TOKEN", "same")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	if _, err := newAgentServer(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("err = %v, want a refusal of one token for both hops", err)
	}
}
