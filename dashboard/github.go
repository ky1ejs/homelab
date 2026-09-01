// "Is the checkout behind what CI has published?"
//
// This closes a wrong answer the page used to give confidently. The checkout is
// mounted read-only, so `homelab update` is not reachable from the browser --
// which means pressing deploy pulls a NEW IMAGE AGAINST WHATEVER COMPOSE FILE
// IS ALREADY ON DISK, and until now nothing said so. A stack whose compose file
// changed upstream would deploy the new image into the old configuration and
// report success.
//
// Answered by asking GitHub to compare the checkout's HEAD against the branch
// it tracks. That is a deliberate choice over running git: answering it locally
// needs `git fetch`, a fetch WRITES (FETCH_HEAD, and the objects it downloads),
// and the checkout is mounted read-only precisely so the web-facing half of
// this stack cannot change what the deploy buttons execute. Asking a remote
// over HTTPS needs no write at all.
//
// This lives in the WEB role, not the agent, for the same reason registry.go
// does: it is an outbound call to a third party, and there is no reason to make
// one from the process holding the Docker socket. The agent supplies the two
// local facts (which commit, which repo) and nothing else.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxCommitsShown caps what the page draws. The point of the list is to answer
// "what am I about to apply", and thirty subject lines on a phone is already
// past the point where anyone reads them; the count above it stays exact.
const maxCommitsShown = 30

// githubAPI is a var only so the tests can point it at an httptest server.
// Nothing reads it from the environment: the one thing this client talks to
// should not be configurable by whatever is in the container's .env.
var githubAPI = "https://api.github.com"

type githubClient struct {
	http *http.Client
	ttl  time.Duration

	mu    sync.Mutex
	cache map[string]cachedCompare
}

type cachedCompare struct {
	status BehindStatus
	at     time.Time
}

func newGitHubClient(ttl time.Duration) *githubClient {
	return &githubClient{
		http:  &http.Client{Timeout: 15 * time.Second},
		ttl:   ttl,
		cache: map[string]cachedCompare{},
	}
}

// Behind compares the checkout against the branch it tracks.
//
// Cached with the same reasoning as registry.go's digest cache, and with a
// sharper edge: this is an UNAUTHENTICATED GitHub API call, which is rate
// limited to 60 per hour per source address. A page that refreshes itself every
// DASH_REFRESH would burn that in an hour of being left open on a phone. At the
// default fifteen-minute TTL it costs four calls an hour, and the answer only
// changes when someone pushes.
//
// Failures are cached too, for the same reason the registry's are: a NAS with no
// upstream should not spend fifteen seconds of every page load rediscovering it.
func (gc *githubClient) Behind(ctx context.Context, c Checkout) *BehindStatus {
	if c.Slug == "" || c.Head == "" {
		return nil
	}
	branch := c.Branch
	if branch == "" {
		// Detached HEAD. There is no tracking branch to compare against, and
		// guessing "main" would produce a confident answer about a question the
		// checkout is not asking.
		return &BehindStatus{
			Status:    "detached",
			CheckedAt: time.Now(),
			Err:       "the checkout is not on a branch, so there is nothing to compare it against",
		}
	}

	key := c.Slug + "/" + c.Head + "..." + branch

	// A COPY, not the cached pointer. The caller narrows Stacks to the stacks
	// that actually exist, and this value is shared across concurrent page
	// loads -- handing out the same pointer would make that a data race, which
	// is exactly what CI's `go test -race` is there to catch.
	gc.mu.Lock()
	if hit, ok := gc.cache[key]; ok && time.Since(hit.at) < gc.ttl {
		gc.mu.Unlock()
		out := hit.status
		return &out
	}
	gc.mu.Unlock()

	st := gc.fetchCompare(ctx, c.Slug, c.Head, branch)

	// Never cache our own cancellation -- the same trap registry.go documents.
	// A reload partway through a page load cancels this, and caching that would
	// pin the line to "context canceled" for a full TTL while GitHub was fine,
	// with the cache hit being instant so nothing ever retried.
	if st.Err != "" && ctx.Err() != nil {
		return st
	}

	gc.mu.Lock()
	gc.cache[key] = cachedCompare{status: *st, at: time.Now()}
	gc.mu.Unlock()

	return st
}

// compareResponse is the subset of GitHub's compare payload this reads.
//
// https://docs.github.com/en/rest/commits/commits#compare-two-commits
type compareResponse struct {
	// Status describes HEAD relative to BASE. Called with base=the checkout and
	// head=the branch, so GitHub's "ahead" means the BRANCH is ahead -- i.e. the
	// checkout is BEHIND. That inversion is the one thing in this file worth
	// reading twice, and translate() is where it is undone.
	Status      string `json:"status"`
	AheadBy     int    `json:"ahead_by"`
	BehindBy    int    `json:"behind_by"`
	TotalCommit int    `json:"total_commits"`
	Commits     []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	} `json:"commits"`
	Files []struct {
		Filename string `json:"filename"`
	} `json:"files"`
}

func (gc *githubClient) fetchCompare(ctx context.Context, slug, head, branch string) *BehindStatus {
	st := &BehindStatus{CheckedAt: time.Now(), Status: "unknown"}

	// Both halves are escaped even though checkout.go already validated the slug
	// and the sha. Defence in depth on a string that becomes a URL is cheap, and
	// the branch name has been through no validation at all -- it is whatever
	// .git/HEAD said.
	endpoint := fmt.Sprintf("%s/repos/%s/compare/%s...%s",
		githubAPI, slug, url.PathEscape(head), url.PathEscape(branch))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "homelab-dash/"+Version)

	resp, err := gc.http.Do(req)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Name the rate limit specifically. It is the failure this will actually
		// hit, it resolves on its own, and "403" alone would send someone looking
		// for a credential problem that does not exist -- nothing here
		// authenticates, because the repo is public.
		if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-Ratelimit-Remaining") == "0" {
			st.Err = "GitHub's unauthenticated rate limit is exhausted; this resolves itself within the hour"
			return st
		}
		st.Err = fmt.Sprintf("GitHub compare returned %s", resp.Status)
		return st
	}

	// Bounded: a comparison spanning a large range carries every commit and
	// every changed file, and this process renders a page rather than needing
	// the whole document.
	var cmp compareResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&cmp); err != nil {
		st.Err = "cannot read GitHub's answer: " + err.Error()
		return st
	}

	translate(st, cmp)
	return st
}

// translate turns GitHub's base-relative verdict into the checkout's own.
func translate(st *BehindStatus, cmp compareResponse) {
	switch cmp.Status {
	case "identical":
		st.Status = "current"
	case "ahead":
		// The branch is ahead of the checkout, so the checkout is behind.
		st.Status = "behind"
		st.Count = cmp.AheadBy
	case "behind":
		// The checkout has commits the branch does not. Someone committed on the
		// NAS, which nothing in this repo does -- worth showing rather than
		// flattening into "current".
		st.Status = "ahead"
		st.Count = cmp.BehindBy
	case "diverged":
		st.Status = "diverged"
		st.Count = cmp.AheadBy
	default:
		st.Status = "unknown"
		st.Err = "GitHub returned an unrecognised comparison status " + cmp.Status
	}

	for i, c := range cmp.Commits {
		if i >= maxCommitsShown {
			break
		}
		st.Commits = append(st.Commits, CommitSummary{SHA: c.SHA, Subject: subject(c.Commit.Message)})
	}
	// Newest first. GitHub returns the comparison oldest-first, and the useful
	// order for "what am I about to apply" is the other one.
	for i, j := 0, len(st.Commits)-1; i < j; i, j = i+1, j-1 {
		st.Commits[i], st.Commits[j] = st.Commits[j], st.Commits[i]
	}

	// Top-level directories of everything the comparison touched. These are not
	// yet stack names -- ".github" and "bin" land here too -- and narrowing them
	// to stacks that exist is the caller's job, because this client does not
	// know what is deployed. See web.go's annotate().
	seen := map[string]bool{}
	for _, f := range cmp.Files {
		if dir, _, ok := strings.Cut(f.Filename, "/"); ok && !seen[dir] {
			seen[dir] = true
			st.Stacks = append(st.Stacks, dir)
		}
	}
	sort.Strings(st.Stacks)
}

// subject is the first line of a commit message, which is all the page shows.
func subject(msg string) string {
	line, _, _ := strings.Cut(msg, "\n")
	return strings.TrimSpace(line)
}
