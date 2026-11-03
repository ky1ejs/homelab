package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Snapshotter commits vault writes into the repo that vault-sync and
// vault-claude already share.
//
// Why this exists: snapshot commits bracket agent runs via Claude Code's
// SessionStart/Stop hooks, which fire only for vault-claude. Writes arriving
// through this server fire no hooks, so without an explicit commit a note
// captured by voice stays unversioned until the hourly backstop in vault-sync
// happens to notice — and then lands attributed to a human. See
// obsidian-vault/ARCHITECTURE.md#snapshots.
//
// A distinct author keeps `git log --author` an honest audit trail of which
// surface wrote what.
type Snapshotter struct {
	snapshotDir string
	vaultDir    string
	authorName  string
	authorEmail string
	lockTimeout time.Duration
	log         *slog.Logger
}

func NewSnapshotter(snapshotDir, vaultDir, name, email string, timeout time.Duration, log *slog.Logger) *Snapshotter {
	return &Snapshotter{
		snapshotDir: snapshotDir,
		vaultDir:    vaultDir,
		authorName:  name,
		authorEmail: email,
		lockTimeout: timeout,
		log:         log,
	}
}

// Commit stages and commits the given paths, and nothing else.
//
// Variadic because a move touches two paths and they belong in ONE commit: two
// commits would record a note deleted and an unrelated note created, which is
// exactly the history `git log --follow` cannot read as a rename. The notes
// whose links followed the move go in the same commit, so on this surface one
// revert undoes all of it. --only commits each path's whole work-tree content,
// though: a human edit to a relinked note that no snapshot has caught yet is
// committed here under this server's name, and a revert undoes it too.
//
// Paths the snapshot repo ignores are dropped first. snapshot.sh writes the
// attachment extensions into info/exclude when EXCLUDE_ATTACHMENTS=1, and
// naming one would fail the whole commit — leaving the notes an attachment
// move relinked uncommitted until the hourly backstop takes them under the
// wrong author.
//
// Errors are logged and swallowed by the caller on purpose, matching
// hook-snapshot.sh: a snapshot failure must never turn a successful write into
// a failed tool call. The note is already safely on disk by this point, and the
// hourly backstop will pick it up regardless.
func (s *Snapshotter) Commit(ctx context.Context, message string, relPaths ...string) error {
	if len(relPaths) == 0 {
		return errors.New("commit: no paths")
	}
	gitDir := filepath.Join(s.snapshotDir, "vault.git")
	if _, err := os.Stat(gitDir); err != nil {
		// vault-sync's snapshot.sh owns creating this repo, including the
		// core.bare dance and info/exclude. Initialising it from here would
		// race that and produce a repo configured differently.
		return fmt.Errorf("snapshot repo %s not present: %w", gitDir, err)
	}

	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	base := append(os.Environ(),
		"GIT_DIR="+gitDir,
		"GIT_WORK_TREE="+s.vaultDir,
	)
	// Every path here is a file name, never a pattern. Without this a note
	// called "Log [1].md" is a glob, and --only would commit "Log 1.md" too.
	// check-ignore takes plain paths already and refuses the setting.
	env := append(base[:len(base):len(base)], "GIT_LITERAL_PATHSPECS=1")

	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		cmd.Dir = s.vaultDir
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	relPaths, err = s.notIgnored(ctx, base, relPaths)
	if err != nil {
		return err
	}
	relPaths, err = s.droppedUntracked(ctx, env, relPaths)
	if err != nil {
		return err
	}
	if len(relPaths) == 0 {
		return nil // nothing here is versioned
	}

	// An untracked file cannot be committed by pathspec alone, so stage first.
	// `git add` on a path whose file is gone stages the deletion, which is what
	// makes the vacated half of a move part of the same commit.
	if out, err := run(append([]string{"add", "--"}, relPaths...)...); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}

	// --only commits just these paths even if the index holds other changes,
	// which keeps attribution precise: a human edit sitting in the work tree
	// does not get committed under this server's identity.
	args := []string{
		"-c", "user.name=" + s.authorName,
		"-c", "user.email=" + s.authorEmail,
		"commit", "--quiet", "--no-verify", "--only",
		"-m", message, "--",
	}
	args = append(args, relPaths...)
	if out, err := run(args...); err != nil {
		// "nothing to commit" is the normal outcome of re-saving identical
		// content and is not a failure.
		if strings.Contains(out, "nothing to commit") || strings.Contains(out, "no changes added") {
			return nil
		}
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	return nil
}

// notIgnored drops the paths the snapshot repo ignores. A tracked path is never
// reported as ignored, so a file committed before an exclusion was added keeps
// being committed.
func (s *Snapshotter) notIgnored(ctx context.Context, env []string, relPaths []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-z", "--stdin")
	cmd.Env = env
	cmd.Dir = s.vaultDir
	cmd.Stdin = strings.NewReader(strings.Join(relPaths, "\x00") + "\x00")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return relPaths, nil // none of them is ignored
	}
	if err != nil {
		return nil, fmt.Errorf("git check-ignore: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	ignored := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			ignored[p] = true
		}
	}
	var keep []string
	for _, p := range relPaths {
		if !ignored[p] {
			keep = append(keep, p)
		}
	}
	return keep, nil
}

// droppedUntracked leaves out the paths that are gone from the vault and were
// never committed: a note that arrived through Sync after the last snapshot and
// was then moved or deleted. There is nothing to record for one, and naming it
// fails `git add` ("did not match any files") and with it the whole commit.
func (s *Snapshotter) droppedUntracked(ctx context.Context, env []string, relPaths []string) ([]string, error) {
	var gone []string
	for _, p := range relPaths {
		if _, err := os.Lstat(filepath.Join(s.vaultDir, filepath.FromSlash(p))); errors.Is(err, fs.ErrNotExist) {
			gone = append(gone, p)
		}
	}
	if len(gone) == 0 {
		return relPaths, nil
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"ls-files", "-z", "--"}, gone...)...)
	cmd.Env = env
	cmd.Dir = s.vaultDir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	tracked := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			tracked[p] = true
		}
	}
	untracked := map[string]bool{}
	for _, p := range gone {
		if !tracked[p] {
			untracked[p] = true
		}
	}
	var keep []string
	for _, p := range relPaths {
		if !untracked[p] {
			keep = append(keep, p)
		}
	}
	return keep, nil
}

// lock takes the same flock that snapshot.sh uses, so this server, the session
// hooks and the hourly backstop cannot interleave two commits.
func (s *Snapshotter) lock(ctx context.Context) (func(), error) {
	path := filepath.Join(s.snapshotDir, ".snapshot.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(s.lockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("snapshot lock busy after %s", s.lockTimeout)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
