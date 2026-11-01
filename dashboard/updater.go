// The updater role: the only process in this repo that writes to the checkout.
//
// It exists so that "the checkout is 3 commits behind" stops ending in "now SSH
// in and run homelab update". It does exactly two things, both against one
// remote and one branch fixed at startup:
//
//	preview   git fetch, then report HEAD..origin/main: the commits, the stacks
//	          they touch, and whether the tip is signed by a key HEAD trusts.
//	apply     the same checks again, then git merge --ff-only to the commit the
//	          preview showed -- and only that commit.
//
// The design, and the sentence in README.md#what-it-cannot-do it reverses, is
// obsidian-vault/DECISIONS.md#letting-the-dashboard-update-the-checkout. The
// short version:
//
//   - IT HOLDS NO DOCKER SOCKET. Advancing the checkout and acting on it stay in
//     different containers, so this one can change what a deploy WOULD do and
//     cannot run one.
//   - NOTHING REACHES IT BUT THE AGENT. Requests go web -> agent -> updater, on a
//     compose network the web role is not on, with a token the web role does not
//     hold. Routing through the agent puts apply behind the agent's mutation
//     lock, so an update cannot interleave with a deploy.
//   - IT INGESTS REPO CONTENT, so git runs with every repo-reachable route to
//     execution switched off: no hooks, no fsmonitor, no system or global
//     config, https only. See gitArgs.
//   - THE SIGNER LIST IS READ FROM HEAD, NOT FROM THE COMMIT BEING VERIFIED. A
//     commit cannot vouch for itself by adding its own key; a new signer arrives
//     the way every other change does, after a commit already trusted.
//
// There is deliberately no "reset", no "checkout <ref>" and no free-form
// argument. A checkout that has diverged is a state for a human with a shell.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// commitRe is a full commit id. Apply takes nothing shorter: an abbreviation is
// a request for git to guess, and the whole point of apply is that it acts on
// exactly the commit a human was shown.
var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// maxPreviewCommits caps the list a preview returns. The count stays exact.
const maxPreviewCommits = 100

type updater struct {
	repoDir string
	// remoteURL is what origin must resolve to, insteadOf rewrites included.
	remoteURL string
	// branch is both the local branch HEAD must be on and the branch fetched
	// from origin. A deploy checkout tracks one branch; this does not need to
	// support two names.
	branch string
	// signersPath is the keyring's path INSIDE THE REPO, read with
	// `git show HEAD:<path>` and never from the working tree.
	signersPath string
	token       string
	gitBin      string
	gpgBin      string

	// allowFileProtocol is for the tests alone, which serve origin from a
	// directory. Nothing in the environment can set it.
	allowFileProtocol bool

	// The agent's lock already serialises apply against deploys. This one is
	// for the updater's own invariant -- two fetches or a fetch and a merge must
	// not interleave on one .git -- and holds even if a second caller ever
	// appears.
	mu sync.Mutex
}

func updaterAddr() string { return envOr("DASHU_ADDR", ":8091") }

func newUpdaterServer() (*http.Server, error) {
	u, err := newUpdaterFromEnv()
	if err != nil {
		return nil, err
	}

	// Refuse root. This container's job is writing files that every other
	// stack then reads and deploys, and git as root in a checkout owned by
	// someone else is how a deploy checkout ends up with root-owned objects the
	// owner can no longer update. The compose file runs it as the checkout's
	// owner; this makes "forgot the user: line" a startup failure rather than a
	// slow corruption.
	if os.Getuid() == 0 {
		return nil, errors.New("the updater will not run as root: set UPDATER_UID in .env to the uid that owns the checkout (ls -ln on the NAS)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/v1/preview", u.requireToken(u.handlePreview))
	mux.HandleFunc("/v1/apply", u.requireToken(u.handleApply))

	return &http.Server{
		Addr:              updaterAddr(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

func newUpdaterFromEnv() (*updater, error) {
	repoDir := envOr("REPO_DIR", "/repo")
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		return nil, fmt.Errorf("REPO_DIR=%s is not a git checkout: %w", repoDir, err)
	}

	token := strings.TrimSpace(os.Getenv("DASH_UPDATER_TOKEN"))
	if token == "" {
		return nil, errors.New("DASH_UPDATER_TOKEN is empty: the updater will not run without one")
	}
	// The agent's token has no business here. Holding it would let this
	// container drive the one that has the Docker socket, which is precisely
	// the capability the split keeps out of it.
	if strings.TrimSpace(os.Getenv("DASH_AGENT_TOKEN")) != "" {
		return nil, errors.New("DASH_AGENT_TOKEN is set in the updater's environment: it must not be. See dashboard/docker-compose.yml")
	}

	branch := envOr("DASH_UPDATER_BRANCH", "main")
	if !refPathSafe(branch) {
		return nil, fmt.Errorf("DASH_UPDATER_BRANCH=%q is not a plain branch name", branch)
	}
	signers := envOr("DASH_UPDATER_SIGNERS", "dashboard/updater-signers.asc")
	if !repoPathSafe(signers) {
		return nil, fmt.Errorf("DASH_UPDATER_SIGNERS=%q must be a relative path inside the repo", signers)
	}

	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("no git in this image: %w", err)
	}
	gpgBin, err := exec.LookPath("gpg")
	if err != nil {
		return nil, fmt.Errorf("no gpg in this image: %w", err)
	}

	return &updater{
		repoDir:     repoDir,
		remoteURL:   envOr("DASH_UPDATER_REMOTE", "https://github.com/ky1ejs/homelab.git"),
		branch:      branch,
		signersPath: signers,
		token:       token,
		gitBin:      gitBin,
		gpgBin:      gpgBin,
	}, nil
}

// repoPathSafe accepts a clean relative path that stays inside the repo.
func repoPathSafe(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") || strings.ContainsAny(p, ":\\\x00\n") {
		return false
	}
	clean := path.Clean(p)
	return clean == p && clean != "." && !strings.HasPrefix(clean, "../") && clean != ".."
}

func (u *updater) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(u.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

// updateRequest is the whole of what the agent can say to the updater. Commit
// is required by apply and refused by preview.
type updateRequest struct {
	Commit string `json:"commit,omitempty"`
}

func (u *updater) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil || req.Commit != "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Detached for the same reason the agent detaches a mutation: a fetch cut
	// off halfway leaves a lock file in .git that blocks the next one.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	writeJSON(w, http.StatusOK, u.preview(ctx))
}

func (u *updater) handleApply(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil || !commitRe.MatchString(req.Commit) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	writeJSON(w, http.StatusOK, u.apply(ctx, req.Commit))
}

// --- the two verbs ----------------------------------------------------------

func (u *updater) preview(ctx context.Context) UpdateOutcome {
	if !u.mu.TryLock() {
		return UpdateOutcome{Err: "busy: another update or preview is running"}
	}
	defer u.mu.Unlock()

	out, head, target, err := u.fetchAndCompare(ctx)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	if head == target || out.Status == UpdateStatusDiverged {
		return out
	}
	out.Signature = u.verify(ctx, target)
	return out
}

func (u *updater) apply(ctx context.Context, reviewed string) UpdateOutcome {
	if !u.mu.TryLock() {
		return UpdateOutcome{Err: "busy: another update or preview is running"}
	}
	defer u.mu.Unlock()

	// Fetch again rather than trusting the preview's answer. The preview may be
	// an hour old, and "origin/main moved since you looked" is the one case
	// this verb exists to catch.
	out, head, target, err := u.fetchAndCompare(ctx)
	if err != nil {
		out.Err = err.Error()
		return out
	}

	// THE REVIEWED-SHA CHECK. The press means "take the commit I was shown",
	// not "take whatever is upstream now". Refused even when the new tip is a
	// descendant of the reviewed one: the commits added since were not shown.
	if target != reviewed {
		out.Err = fmt.Sprintf("origin/%s is now %s, not the %s you previewed: preview again",
			u.branch, short(target), short(reviewed))
		return out
	}
	if head == target {
		// Already there -- an SSH `homelab update` got in first. Not an error:
		// the checkout is at exactly the commit that was approved.
		return out
	}
	if out.Status == UpdateStatusDiverged {
		out.Err = "the checkout has commits origin does not, so it cannot fast-forward. Resolve it over SSH"
		return out
	}

	out.Signature = u.verify(ctx, target)
	if !out.Signature.OK {
		out.Err = "refusing to apply: " + out.Signature.Reason
		return out
	}

	if _, err := u.git(ctx, nil, "merge", "--ff-only", "--no-stat", "--quiet", target); err != nil {
		out.Err = "fast-forward failed: " + err.Error()
		return out
	}

	// Read HEAD back rather than assuming. The merge exiting 0 is git's claim;
	// the commit HEAD names is the fact the page will report.
	now, err := u.git(ctx, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(now) != target {
		out.Err = fmt.Sprintf("merge reported success but HEAD is %q, not %s", strings.TrimSpace(now), short(target))
		return out
	}

	out.Status = UpdateStatusApplied
	log.Printf("update applied: %s -> %s (%d commits, signed by %s)",
		short(head), short(target), out.Count, out.Signature.Signer)
	return out
}

// fetchAndCompare runs every precondition, fetches, and fills in what HEAD is
// missing. It returns the two commits so callers do not re-derive them.
func (u *updater) fetchAndCompare(ctx context.Context) (UpdateOutcome, string, string, error) {
	var out UpdateOutcome

	if err := u.checkOwnership(); err != nil {
		return out, "", "", err
	}

	// The branch HEAD is on, not merely the commit. A detached HEAD or another
	// branch means someone did something by hand, and fast-forwarding "main"
	// over it would move a branch they are not looking at.
	branch, err := u.git(ctx, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return out, "", "", fmt.Errorf("HEAD is detached: check out %s over SSH first", u.branch)
	}
	if b := strings.TrimSpace(branch); b != u.branch {
		return out, "", "", fmt.Errorf("the checkout is on %q, not %q", b, u.branch)
	}

	// The URL git will actually dial, with any url.<x>.insteadOf in the local
	// config applied. Comparing remote.origin.url alone would pass a config
	// that rewrites it somewhere else at fetch time.
	got, err := u.git(ctx, nil, "ls-remote", "--get-url", "origin")
	if err != nil {
		return out, "", "", fmt.Errorf("cannot read origin's URL: %w", err)
	}
	if g := strings.TrimSpace(got); g != u.remoteURL {
		return out, "", "", fmt.Errorf("origin resolves to %q, not %q (DASH_UPDATER_REMOTE)", g, u.remoteURL)
	}

	// Tracked files only. .env is untracked and ignored, and is expected to
	// differ; an edited compose file is a hand-made change that a fast-forward
	// would either refuse or, worse, quietly keep beside the new version.
	dirty, err := u.git(ctx, nil, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return out, "", "", fmt.Errorf("cannot read the working tree's state: %w", err)
	}
	if d := strings.TrimSpace(dirty); d != "" {
		return out, "", "", fmt.Errorf("the working tree has uncommitted changes to tracked files, so the updater will not touch it:\n%s", d)
	}

	// An explicit refspec, so the local remote.origin.fetch cannot redirect
	// what lands where. --no-tags and no submodules: neither is part of what a
	// deploy reads.
	refspec := "+refs/heads/" + u.branch + ":refs/remotes/origin/" + u.branch
	if _, err := u.git(ctx, nil, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules",
		"--no-write-fetch-head", "origin", refspec); err != nil {
		return out, "", "", fmt.Errorf("git fetch failed: %w", err)
	}

	head, err := u.git(ctx, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return out, "", "", fmt.Errorf("cannot resolve HEAD: %w", err)
	}
	target, err := u.git(ctx, nil, "rev-parse", "--verify", "refs/remotes/origin/"+u.branch+"^{commit}")
	if err != nil {
		return out, "", "", fmt.Errorf("cannot resolve origin/%s after fetching: %w", u.branch, err)
	}
	head, target = strings.TrimSpace(head), strings.TrimSpace(target)
	out.Head, out.Target = head, target

	if head == target {
		out.Status = UpdateStatusCurrent
		return out, head, target, nil
	}

	// --is-ancestor exits 1 for "no" and something else for an error; both are
	// "cannot fast-forward" from here, but the message should say which.
	if _, err := u.git(ctx, nil, "merge-base", "--is-ancestor", head, target); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			out.Status = UpdateStatusDiverged
			return out, head, target, nil
		}
		return out, "", "", fmt.Errorf("cannot compare HEAD with origin/%s: %w", u.branch, err)
	}
	out.Status = UpdateStatusBehind

	count, err := u.git(ctx, nil, "rev-list", "--count", head+".."+target)
	if err != nil {
		return out, "", "", err
	}
	out.Count, _ = strconv.Atoi(strings.TrimSpace(count))

	lines, err := u.git(ctx, nil, "log", "--no-decorate", "--format=%H%x1f%s",
		"--max-count="+strconv.Itoa(maxPreviewCommits), head+".."+target)
	if err != nil {
		return out, "", "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		sha, subj, ok := strings.Cut(line, "\x1f")
		if ok {
			out.Commits = append(out.Commits, CommitSummary{SHA: sha, Subject: subj})
		}
	}
	out.Truncated = out.Count > len(out.Commits)

	stacks, err := u.stacksTouched(ctx, head, target)
	if err != nil {
		return out, "", "", err
	}
	out.Stacks = stacks

	return out, head, target, nil
}

// stacksTouched names the top-level directories the update changes that hold a
// docker-compose.yml at the target -- the stacks to deploy afterwards. Read
// from git objects, so it is right before the merge as well as after it.
func (u *updater) stacksTouched(ctx context.Context, head, target string) ([]string, error) {
	names, err := u.git(ctx, nil, "diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z", head, target)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var stacks []string
	for _, f := range strings.Split(names, "\x00") {
		dir, _, ok := strings.Cut(f, "/")
		if !ok || seen[dir] || !nameRe.MatchString(dir) {
			continue
		}
		seen[dir] = true
		if _, err := u.git(ctx, nil, "cat-file", "-e", target+":"+dir+"/docker-compose.yml"); err == nil {
			stacks = append(stacks, dir)
		}
	}
	sort.Strings(stacks)
	return stacks, nil
}

// checkOwnership turns git's "dubious ownership" refusal, and the permission
// errors that follow a wrong uid, into one sentence that names the fix.
func (u *updater) checkOwnership() error {
	info, err := os.Stat(filepath.Join(u.repoDir, ".git"))
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return fmt.Errorf("the checkout's .git is owned by uid %d and this container runs as %d: set UPDATER_UID=%d in dashboard/.env and recreate",
			st.Uid, uid, st.Uid)
	}
	return nil
}

// --- signatures -------------------------------------------------------------

// verify checks target's signature against the keyring committed at HEAD.
//
// What a pass means, stated so nobody reads more into it: the tip was signed by
// a key in that file. For GitHub's web-flow key, that is "made on github.com by
// someone with write access" -- a merge button, not a person. It rules out a
// commit pushed with a stolen git credential and no signing key, and nothing
// more. DECISIONS.md says so too.
//
// Only the tip is verified. A fast-forward ends with the working tree at the
// tip's tree, and the tip's signature covers that tree; an unsigned commit in
// the middle contributes nothing the tip does not also vouch for.
func (u *updater) verify(ctx context.Context, target string) *SignatureVerdict {
	v := &SignatureVerdict{}

	keyring, err := u.git(ctx, nil, "show", "HEAD:"+u.signersPath)
	if err != nil || strings.TrimSpace(keyring) == "" {
		v.Reason = fmt.Sprintf("the checkout's HEAD has no %s, so there is no key to verify against. Run `homelab update` over SSH once", u.signersPath)
		return v
	}

	home, err := os.MkdirTemp("", "homelab-gnupg-")
	if err != nil {
		v.Reason = "cannot create a keyring directory: " + err.Error()
		return v
	}
	defer os.RemoveAll(home)
	// no-autostart: nothing here needs gpg-agent, and an agent started for a
	// temporary home outlives it as an orphan daemon in the container.
	if err := os.WriteFile(filepath.Join(home, "gpg.conf"), []byte("no-autostart\n"), 0o600); err != nil {
		v.Reason = "cannot write gpg.conf: " + err.Error()
		return v
	}
	gpgEnv := []string{"GNUPGHOME=" + home}

	if _, _, err := u.run(ctx, u.gpgBin, gpgEnv, strings.NewReader(keyring), "--batch", "--quiet", "--import"); err != nil {
		v.Reason = fmt.Sprintf("%s at HEAD does not import as an OpenPGP keyring: %v", u.signersPath, err)
		return v
	}
	listing, _, err := u.run(ctx, u.gpgBin, gpgEnv, nil, "--batch", "--with-colons", "--list-keys")
	if err != nil {
		v.Reason = "cannot list the imported keys: " + err.Error()
		return v
	}
	trusted := primaryFingerprints(listing)
	if len(trusted) == 0 {
		v.Reason = fmt.Sprintf("%s at HEAD holds no keys", u.signersPath)
		return v
	}

	// --raw prints gpg's machine-readable status lines, on stderr. The exit code
	// alone is not enough: git exits 0 for a good signature from ANY key gpg
	// can see, and the fingerprint check below is what ties it to this file.
	_, status, _ := u.run(ctx, u.gitBin, gpgEnv, nil, append(u.gitArgs(), "verify-commit", "--raw", target)...)
	return judgeSignature(status, trusted)
}

// primaryFingerprints reads `gpg --with-colons --list-keys`: the fpr record
// that follows each pub record is that key's primary fingerprint.
func primaryFingerprints(colons string) map[string]bool {
	out := map[string]bool{}
	afterPub := false
	for _, line := range strings.Split(colons, "\n") {
		f := strings.Split(line, ":")
		switch {
		case f[0] == "pub":
			afterPub = true
		case f[0] == "fpr" && afterPub && len(f) > 9:
			out[strings.ToUpper(f[9])] = true
			afterPub = false
		case f[0] == "sub":
			afterPub = false
		}
	}
	return out
}

// judgeSignature reads gpg's status lines and decides. Fails closed: anything
// other than exactly one good, valid signature by a listed primary key is a
// refusal, and an unrecognised shape is a refusal with the raw lines attached.
func judgeSignature(status string, trusted map[string]bool) *SignatureVerdict {
	v := &SignatureVerdict{}
	var good, valid int
	var primary string
	for _, line := range strings.Split(status, "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "[GNUPG:] "))
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "GOODSIG":
			good++
			if len(f) > 2 {
				v.Signer = strings.Join(f[2:], " ")
			}
		case "VALIDSIG":
			valid++
			if len(f) > 10 {
				primary = strings.ToUpper(f[10])
			}
		case "BADSIG", "ERRSIG", "EXPSIG", "EXPKEYSIG", "REVKEYSIG", "NO_PUBKEY":
			v.Reason = "the signature does not verify against any key in the signer file (" + f[0] + ")"
			return v
		}
	}
	switch {
	case good == 0 && valid == 0 && strings.TrimSpace(status) == "":
		v.Reason = "the commit is not signed with an OpenPGP key"
		return v
	case good != 1 || valid != 1 || primary == "":
		v.Reason = "gpg's answer was not a single good signature: " + strings.TrimSpace(status)
		return v
	case !trusted[primary]:
		v.Reason = "signed by " + primary + ", which is not in the signer file"
		return v
	}
	v.OK = true
	v.Fingerprint = primary
	return v
}

// --- git --------------------------------------------------------------------

// gitArgs are prepended to every git invocation. Command-line -c beats every
// config file, so these hold whatever .git/config says.
//
//	core.hooksPath=/dev/null    no hook can run: the directory cannot hold files
//	core.fsmonitor=false        fsmonitor is a configured command, run on status
//	protocol.*                  https and nothing else; no ext::, no file://
//	credential.helper=          clears the helper list: the repo is public, and
//	                            a configured helper is a command that runs
//	gpg.program=gpg             the binary looked up at startup, not a local
//	                            config's choice of "verifier"
//	gpg.ssh.allowedSignersFile  /dev/null, so an SSH signature verifies against
//	                            nothing rather than erroring on the missing key
//	submodule.recurse=false     no submodule is fetched or checked out
//
// diff.external and textconv are switched off per command with --no-ext-diff
// and --no-textconv. .gitattributes filters need a driver defined in config,
// which repo content cannot do, and GIT_CONFIG_NOSYSTEM/GLOBAL below leave only
// the local config -- writable by the checkout's owner and nothing else.
func (u *updater) gitArgs() []string {
	args := []string{
		"-C", u.repoDir,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "credential.helper=",
		"-c", "gpg.program=" + u.gpgBin,
		"-c", "gpg.ssh.allowedSignersFile=/dev/null",
		"-c", "submodule.recurse=false",
		"--no-pager",
	}
	if u.allowFileProtocol {
		args = append(args, "-c", "protocol.file.allow=always")
	}
	return args
}

func (u *updater) git(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	stdout, _, err := u.run(ctx, u.gitBin, extraEnv, nil, append(u.gitArgs(), args...)...)
	return stdout, err
}

// run execs one command with a minimal environment. stdout and stderr are kept
// apart so a warning on stderr can never be parsed as a commit id; stderr rides
// along in the error. The environment is built, never inherited: the token must
// not reach a child, and nothing from the container's env should steer git.
func (u *updater) run(ctx context.Context, bin string, extraEnv []string, stdin *strings.Reader, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = u.repoDir
	cmd.Env = append(updaterChildEnv(), extraEnv...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
	}
	return stdout.String(), stderr.String(), err
}

func updaterChildEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/tmp",
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		// A fast-forward writes a reflog entry, which wants an identity.
		"GIT_COMMITTER_NAME=homelab-updater",
		"GIT_COMMITTER_EMAIL=homelab-updater@localhost",
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
