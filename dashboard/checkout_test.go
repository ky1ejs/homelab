package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeGit lays out the parts of .git this code reads.
func writeGit(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	for name, body := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestReadCheckoutFromLooseRef(t *testing.T) {
	repo := writeGit(t, map[string]string{
		".git/HEAD":             "ref: refs/heads/main\n",
		".git/refs/heads/main":  testSHA + "\n",
		".git/config":           "[remote \"origin\"]\n\turl = https://github.com/ky1ejs/homelab.git\n\tfetch = +refs/heads/*\n",
		".git/refs/heads/other": "ffffffffffffffffffffffffffffffffffffffff\n",
	})

	c := readCheckout(repo)
	if c.Err != "" {
		t.Fatalf("readCheckout: %s", c.Err)
	}
	if c.Head != testSHA || c.Branch != "main" || c.Slug != "ky1ejs/homelab" {
		t.Fatalf("got %+v", c)
	}
}

// A fresh `git clone` PACKS its refs, so the checkout on the NAS -- the exact
// thing this runs against -- has no loose refs/heads/main at all. Reading only
// the loose form would have worked on every development machine and failed on
// the one host that matters.
func TestReadCheckoutFallsBackToPackedRefs(t *testing.T) {
	repo := writeGit(t, map[string]string{
		".git/HEAD": "ref: refs/heads/main\n",
		".git/packed-refs": "# pack-refs with: peeled fully-peeled sorted \n" +
			"ffffffffffffffffffffffffffffffffffffffff refs/heads/other\n" +
			testSHA + " refs/heads/main\n" +
			"^aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		".git/config": "[remote \"origin\"]\n\turl = git@github.com:ky1ejs/homelab.git\n",
	})

	c := readCheckout(repo)
	if c.Head != testSHA {
		t.Fatalf("Head = %q, want the packed ref %q (err: %s)", c.Head, testSHA, c.Err)
	}
	if c.Slug != "ky1ejs/homelab" {
		t.Fatalf("Slug = %q", c.Slug)
	}
}

// A checkout parked on a commit is the shape a botched manual recovery leaves
// behind, so it is a state to show rather than an error to raise -- but there is
// no branch to compare it against, and github.go must not guess one.
func TestDetachedHeadIsAStateNotAnError(t *testing.T) {
	repo := writeGit(t, map[string]string{
		".git/HEAD":   testSHA + "\n",
		".git/config": "[remote \"origin\"]\n\turl = https://github.com/ky1ejs/homelab\n",
	})

	c := readCheckout(repo)
	if c.Err != "" || c.Head != testSHA || c.Branch != "" {
		t.Fatalf("got %+v", c)
	}
	if b := newGitHubClient(time.Minute).Behind(t.Context(), c); b == nil || b.Status != "detached" {
		t.Fatalf("a detached checkout was compared anyway: %+v", b)
	}
}

func TestMissingGitIsReportedNotFatal(t *testing.T) {
	if c := readCheckout(t.TempDir()); c.Err == "" {
		t.Fatal("a directory that is not a checkout produced no error")
	}
}

// The slug is interpolated into an api.github.com URL, so anything that is not
// unambiguously a GitHub repository must produce nothing at all rather than a
// request somewhere this code did not intend to go.
func TestGitHubSlugAcceptsOnlyRealGitHubRepos(t *testing.T) {
	ok := map[string]string{
		"https://github.com/ky1ejs/homelab.git":    "ky1ejs/homelab",
		"https://github.com/ky1ejs/homelab":        "ky1ejs/homelab",
		"git@github.com:ky1ejs/homelab.git":        "ky1ejs/homelab",
		"ssh://git@github.com/ky1ejs/homelab.git":  "ky1ejs/homelab",
		"https://github.com/ky1ejs/home.lab-2.git": "ky1ejs/home.lab-2",
	}
	for url, want := range ok {
		if got := githubSlug(url); got != want {
			t.Errorf("githubSlug(%q) = %q, want %q", url, got, want)
		}
	}

	bad := []string{
		"",
		"https://gitlab.com/ky1ejs/homelab.git",
		"https://github.com.evil.example/ky1ejs/homelab.git",
		"https://github.com/ky1ejs",
		"https://github.com/ky1ejs/homelab/extra",
		"https://github.com/../../etc/passwd",
		"https://github.com/ky1ejs/..",
		"https://github.com/ky1ejs/homelab?x=1",
		"https://github.com/ky1ejs/homelab#frag",
		"/srv/git/homelab.git",
	}
	for _, url := range bad {
		if got := githubSlug(url); got != "" {
			t.Errorf("githubSlug(%q) = %q, want empty", url, got)
		}
	}
}

// The origin URL must come from the origin section and nowhere else. A fork
// remote's URL would send the update check at the wrong repository, and the
// answer would look entirely plausible.
func TestOriginSlugReadsOnlyTheOriginRemote(t *testing.T) {
	repo := writeGit(t, map[string]string{
		".git/HEAD":            "ref: refs/heads/main\n",
		".git/refs/heads/main": testSHA + "\n",
		".git/config": "[core]\n\tbare = false\n" +
			"[remote \"upstream\"]\n\turl = https://github.com/someone/else.git\n" +
			"[remote \"origin\"]\n\turl = https://github.com/ky1ejs/homelab.git\n" +
			"[branch \"main\"]\n\tremote = origin\n",
	})

	if got := readCheckout(repo).Slug; got != "ky1ejs/homelab" {
		t.Fatalf("Slug = %q, want ky1ejs/homelab", got)
	}
}

// No origin is not fatal -- everything else on the card is still true -- but it
// must not be silent, because the "behind by N" line simply will not appear.
func TestNoOriginIsReportedButNotFatal(t *testing.T) {
	repo := writeGit(t, map[string]string{
		".git/HEAD":            "ref: refs/heads/main\n",
		".git/refs/heads/main": testSHA + "\n",
		".git/config":          "[core]\n\tbare = false\n",
	})

	c := readCheckout(repo)
	if c.Head != testSHA {
		t.Fatalf("Head = %q, want it read anyway", c.Head)
	}
	if c.Slug != "" || c.Err == "" {
		t.Fatalf("a checkout with no origin was not reported: %+v", c)
	}
}
