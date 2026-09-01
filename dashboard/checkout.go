// Reading the checkout's git state without a git binary.
//
// WHY NOT SHELL OUT. Three reasons, and the third is the one that decides it:
//
//  1. Container Station gives QTS a docker binary but no git, which is why
//     bin/homelab borrows git from a pinned container. Doing that on every page
//     load would mean starting a container to answer "which commit is this".
//  2. This runs in the process holding the Docker socket. Every exec avoided
//     there is worth avoiding.
//  3. THE CHECKOUT IS MOUNTED READ-ONLY. Most interesting git commands write --
//     `git fetch` writes FETCH_HEAD, and even `git status` wants to refresh the
//     index. Reading the files directly is the only approach that does not need
//     the mount to change, and not changing that mount is the whole point of
//     the current trust boundary.
//
// So this reads .git the way git lays it out on disk, which is a stable format
// and a small one for the three facts needed: which commit, which branch, and
// which GitHub repo it came from. What it deliberately CANNOT answer is "how
// far behind are we" -- that needs a fetch, a fetch is a write, and the web
// role asks GitHub over HTTPS instead. See github.go.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readCheckout gathers what can be known about the checkout from its files.
//
// Never returns an error: a checkout it cannot read is a fact for the page to
// state, not a reason to fail a page load that would otherwise have worked.
func readCheckout(repoDir string) Checkout {
	var c Checkout

	gitDir, err := resolveGitDir(repoDir)
	if err != nil {
		c.Err = err.Error()
		return c
	}

	c.Head, c.Branch, err = readHead(gitDir)
	if err != nil {
		c.Err = err.Error()
		return c
	}

	c.Slug = originSlug(gitDir)
	if c.Slug == "" {
		// Not fatal, and not silent: everything else on the card is still true,
		// but the "behind by N" line cannot be filled in without it.
		c.Err = "no github.com origin remote in .git/config: cannot check for updates"
	}
	return c
}

// resolveGitDir handles both a normal clone and a worktree, where .git is a file
// containing a pointer rather than a directory. The NAS has a normal clone; the
// file form costs four lines and stops this being wrong the first time someone
// tests against a worktree on their Mac.
func resolveGitDir(repoDir string) (string, error) {
	p := filepath.Join(repoDir, ".git")
	info, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("no .git in %s: not a checkout", repoDir)
	}
	if info.IsDir() {
		return p, nil
	}

	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", p, err)
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return "", fmt.Errorf("%s is a file but does not name a gitdir", p)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(repoDir, target)
	}
	return target, nil
}

// readHead returns the commit HEAD resolves to and the branch it points at.
//
// A detached HEAD holds the sha directly and yields an empty branch, which is a
// state worth showing rather than erroring on: a checkout parked on a commit is
// exactly the shape a botched manual recovery leaves behind.
func readHead(gitDir string) (sha, branch string, err error) {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", "", fmt.Errorf("cannot read HEAD: %w", err)
	}
	head := strings.TrimSpace(string(data))

	ref, ok := strings.CutPrefix(head, "ref: ")
	if !ok {
		if !isSHA(head) {
			return "", "", fmt.Errorf("HEAD is neither a ref nor a commit id")
		}
		return head, "", nil
	}

	branch = strings.TrimPrefix(ref, "refs/heads/")
	sha, err = resolveRef(gitDir, ref)
	if err != nil {
		return "", branch, err
	}
	return sha, branch, nil
}

// resolveRef reads a ref from its loose file, falling back to packed-refs.
//
// The fallback is not an edge case: `git clone` packs refs, so a fresh checkout
// on the NAS -- the exact thing this runs against -- has no loose refs/heads/main
// at all until something writes one.
func resolveRef(gitDir, ref string) (string, error) {
	loose, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref)))
	if err == nil {
		if s := strings.TrimSpace(string(loose)); isSHA(s) {
			return s, nil
		}
	}

	f, err := os.Open(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return "", fmt.Errorf("%s is neither a loose ref nor in packed-refs", ref)
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		// "^<sha>" lines are the peeled target of an annotated tag, and belong to
		// the line above rather than naming a ref of their own.
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && name == ref && isSHA(sha) {
			return sha, nil
		}
	}
	return "", fmt.Errorf("%s is neither a loose ref nor in packed-refs", ref)
}

// originSlug pulls owner/repo out of the origin remote's URL.
//
// Only github.com is accepted, and that is a security property rather than a
// limitation: the slug is interpolated into an api.github.com URL by the web
// role, so a remote pointing somewhere else must produce nothing rather than a
// request to a host this code did not intend to contact.
func originSlug(gitDir string) string {
	f, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	defer f.Close()

	inOrigin := false
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if strings.HasPrefix(line, "[") {
			// Both spellings occur: git writes [remote "origin"], and a
			// hand-edited config may use a subsection on one line.
			inOrigin = strings.HasPrefix(line, `[remote "origin"]`)
			continue
		}
		if !inOrigin {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "url" {
			continue
		}
		return githubSlug(strings.TrimSpace(v))
	}
	return ""
}

// githubSlug normalises the two remote URL forms this repo is ever cloned with.
//
//	https://github.com/ky1ejs/homelab.git
//	git@github.com:ky1ejs/homelab.git
//
// Anything else returns "", including a github.com URL with extra path
// segments: two elements is the only shape that is a repository, and guessing
// at a third would mean building an API URL out of something unvalidated.
func githubSlug(url string) string {
	url = strings.TrimSuffix(strings.TrimSpace(url), ".git")

	var path string
	switch {
	case strings.HasPrefix(url, "https://github.com/"):
		path = strings.TrimPrefix(url, "https://github.com/")
	case strings.HasPrefix(url, "http://github.com/"):
		path = strings.TrimPrefix(url, "http://github.com/")
	case strings.HasPrefix(url, "ssh://git@github.com/"):
		path = strings.TrimPrefix(url, "ssh://git@github.com/")
	case strings.HasPrefix(url, "git@github.com:"):
		path = strings.TrimPrefix(url, "git@github.com:")
	default:
		return ""
	}

	owner, repo, ok := strings.Cut(strings.Trim(path, "/"), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return ""
	}
	if !isSlugPart(owner) || !isSlugPart(repo) {
		return ""
	}
	return owner + "/" + repo
}

// isSlugPart holds the character set GitHub allows in an owner or repository
// name. It is enforced because this value ends up in a URL path: a slug
// containing "../" or a query separator would be a request somewhere else.
func isSlugPart(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	// ".." passes the character test and is the one string that must not.
	return s != "." && s != ".."
}

func isSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
