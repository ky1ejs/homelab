package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubGitHub stands in for api.github.com and records what was asked.
func stubGitHub(t *testing.T, handler http.HandlerFunc) *[]string {
	t.Helper()

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	old := githubAPI
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = old })

	return &paths
}

func testCheckout() Checkout {
	return Checkout{Head: testSHA, Branch: "main", Slug: "ky1ejs/homelab"}
}

// THE INVERSION. GitHub's compare status describes HEAD relative to BASE, and
// this client calls it with base=the checkout and head=the branch. So GitHub
// saying "ahead" means the checkout is BEHIND. Getting this backwards would tell
// someone their checkout was current at the exact moment it was not, which is
// the wrong answer this whole feature exists to stop giving.
func TestGitHubAheadMeansTheCheckoutIsBehind(t *testing.T) {
	stubGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":        "ahead",
			"ahead_by":      3,
			"behind_by":     0,
			"total_commits": 3,
			"commits": []map[string]any{
				{"sha": "aaa", "commit": map[string]string{"message": "oldest\n\nbody"}},
				{"sha": "bbb", "commit": map[string]string{"message": "middle"}},
				{"sha": "ccc", "commit": map[string]string{"message": "newest"}},
			},
			"files": []map[string]string{
				{"filename": "vault-mcp/main.go"},
				{"filename": "vault-mcp/README.md"},
				{"filename": ".github/workflows/build-vault-mcp.yml"},
				{"filename": "README.md"},
			},
		})
	})

	b := newGitHubClient(time.Minute).Behind(t.Context(), testCheckout())
	if b.Status != "behind" || b.Count != 3 {
		t.Fatalf("got status %q count %d, want behind/3", b.Status, b.Count)
	}

	// Newest first: the useful order for "what am I about to apply".
	if len(b.Commits) != 3 || b.Commits[0].Subject != "newest" || b.Commits[2].Subject != "oldest" {
		t.Fatalf("commits are in the wrong order: %+v", b.Commits)
	}
	// Subject only. A commit body on this page is noise, and this repo writes
	// long ones.
	if strings.Contains(b.Commits[2].Subject, "body") {
		t.Fatalf("the commit body leaked into the subject: %q", b.Commits[2].Subject)
	}
	// Top-level directories, not yet narrowed to stacks -- that is the caller's
	// job, and knownStacks is what does it.
	if got := strings.Join(b.Stacks, ","); got != ".github,vault-mcp" {
		t.Fatalf("touched dirs = %q", got)
	}
}

// The other three verdicts, including the one that means someone committed on
// the NAS. Flattening that into "current" would hide the state that makes
// `homelab update` refuse to fast-forward.
func TestCompareVerdictsAreTranslated(t *testing.T) {
	cases := map[string]string{
		"identical": "current",
		"ahead":     "behind",
		"behind":    "ahead",
		"diverged":  "diverged",
	}
	for gh, want := range cases {
		t.Run(gh, func(t *testing.T) {
			stubGitHub(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": gh, "ahead_by": 1, "behind_by": 2})
			})
			if got := newGitHubClient(time.Minute).Behind(t.Context(), testCheckout()); got.Status != want {
				t.Fatalf("GitHub %q became %q, want %q", gh, got.Status, want)
			}
		})
	}
}

// The rate limit is the failure this will actually hit -- unauthenticated calls
// get 60 an hour -- and it resolves on its own. A bare "403" would send someone
// looking for a credential problem that does not exist, because nothing here
// authenticates.
func TestRateLimitIsNamedRatherThanReportedAs403(t *testing.T) {
	stubGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ratelimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	})

	b := newGitHubClient(time.Minute).Behind(t.Context(), testCheckout())
	if !strings.Contains(b.Err, "rate limit") {
		t.Fatalf("the rate limit was not named: %q", b.Err)
	}
}

// Caching is not a nicety here: the page refreshes itself every DASH_REFRESH,
// and this is an unauthenticated API with a 60/hour budget. Without it, a phone
// left on this page exhausts the limit inside an hour.
func TestCompareIsCachedForTheTTL(t *testing.T) {
	paths := stubGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "identical"})
	})

	gc := newGitHubClient(time.Hour)
	for range 5 {
		gc.Behind(t.Context(), testCheckout())
	}
	if len(*paths) != 1 {
		t.Fatalf("made %d calls for the same checkout, want 1", len(*paths))
	}

	// A moved checkout is a different question and must not be answered from
	// the cache -- that would show a stale "current" straight after an update.
	moved := testCheckout()
	moved.Head = "fedcba9876543210fedcba9876543210fedcba98"
	gc.Behind(t.Context(), moved)
	if len(*paths) != 2 {
		t.Fatalf("a moved checkout was answered from the cache: %d calls", len(*paths))
	}
}

// The cache hands out copies. web.go narrows Stacks in place on the value it
// gets back, and doing that to a shared pointer would be a data race across
// concurrent page loads -- which is what CI's `go test -race` exists to catch.
func TestCachedResultsAreCopies(t *testing.T) {
	stubGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ahead", "ahead_by": 1,
			"files": []map[string]string{{"filename": "vault-mcp/main.go"}, {"filename": "bin/homelab"}},
		})
	})

	gc := newGitHubClient(time.Hour)
	first := gc.Behind(t.Context(), testCheckout())
	first.Stacks = knownStacks(first.Stacks, []Stack{{Name: "vault-mcp"}})
	first.Status = "clobbered"

	second := gc.Behind(t.Context(), testCheckout())
	if second.Status != "behind" {
		t.Fatalf("mutating one result changed the cached one: %q", second.Status)
	}
	if len(second.Stacks) != 2 {
		t.Fatalf("the cached Stacks slice was narrowed in place: %+v", second.Stacks)
	}
}

// knownStacks turns "these commits touch .github and bin" into an answer to
// "what do I deploy after applying this".
func TestKnownStacksKeepsOnlyDeployableStacks(t *testing.T) {
	got := knownStacks(
		[]string{".github", "bin", "obsidian-vault", "vault-mcp"},
		[]Stack{{Name: "obsidian-vault"}, {Name: "vault-mcp"}, {Name: "dashboard"}},
	)
	if strings.Join(got, ",") != "obsidian-vault,vault-mcp" {
		t.Fatalf("knownStacks = %+v", got)
	}
}

// Nothing to ask about is not an error to display.
func TestNoSlugOrHeadMeansNoComparison(t *testing.T) {
	gc := newGitHubClient(time.Minute)
	if b := gc.Behind(t.Context(), Checkout{Head: testSHA}); b != nil {
		t.Fatalf("compared with no slug: %+v", b)
	}
	if b := gc.Behind(t.Context(), Checkout{Slug: "ky1ejs/homelab"}); b != nil {
		t.Fatalf("compared with no head: %+v", b)
	}
}
