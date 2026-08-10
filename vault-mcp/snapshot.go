package main

import (
	"context"
	"errors"
	"fmt"
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

// Commit stages and commits a single path.
//
// Errors are logged and swallowed by the caller on purpose, matching
// hook-snapshot.sh: a snapshot failure must never turn a successful write into
// a failed tool call. The note is already safely on disk by this point, and the
// hourly backstop will pick it up regardless.
func (s *Snapshotter) Commit(ctx context.Context, relPath, message string) error {
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

	env := append(os.Environ(),
		"GIT_DIR="+gitDir,
		"GIT_WORK_TREE="+s.vaultDir,
	)

	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		cmd.Dir = s.vaultDir
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// An untracked file cannot be committed by pathspec alone, so stage first.
	if out, err := run("add", "--", relPath); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}

	// --only commits just this path even if the index holds other changes,
	// which keeps attribution precise: a human edit sitting in the work tree
	// does not get committed under this server's identity.
	args := []string{
		"-c", "user.name=" + s.authorName,
		"-c", "user.email=" + s.authorEmail,
		"commit", "--quiet", "--no-verify", "--only",
		"-m", message, "--", relPath,
	}
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
