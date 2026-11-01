package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// gitRepo is a snapshot repo laid out as snapshot.sh lays it out: the git dir
// in SNAPSHOT_DIR/vault.git, the vault as its work tree, and the attachment
// extensions in info/exclude, as with EXCLUDE_ATTACHMENTS=1.
type gitRepo struct {
	t                 *testing.T
	snapDir, vaultDir string
}

func newGitRepo(t *testing.T, vaultDir string) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &gitRepo{t: t, snapDir: t.TempDir(), vaultDir: vaultDir}
	r.git("init", "--quiet")
	if err := os.WriteFile(filepath.Join(r.snapDir, "vault.git", "info", "exclude"), []byte("*.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Human", "-c", "user.email=h@example.com"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+filepath.Join(r.snapDir, "vault.git"), "GIT_WORK_TREE="+r.vaultDir)
	cmd.Dir = r.vaultDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// lastCommit is the subject and the files of HEAD.
func (r *gitRepo) lastCommit() (string, []string) {
	r.t.Helper()
	subject := r.git("log", "-1", "--format=%s")
	files := strings.Split(r.git("show", "--name-only", "--format=", "HEAD"), "\n")
	sort.Strings(files)
	return subject, files
}

func (r *gitRepo) snapshotter() *Snapshotter {
	return NewSnapshotter(r.snapDir, r.vaultDir, "vault-mcp", "mcp@example.com", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Under the default EXCLUDE_ATTACHMENTS=1, naming an attachment used to fail
// the whole commit, so the notes an attachment move relinked waited for the
// hourly backstop and landed under a human's name.
func TestCommitLeavesOutIgnoredPaths(t *testing.T) {
	v := newTestVault(t)
	r := newGitRepo(t, v.root)
	write(t, v, "Log.md", "![[scan.png]]\n")
	r.git("add", "Log.md")
	r.git("commit", "--quiet", "-m", "seed")
	write(t, v, "receipt.png", "png")
	write(t, v, "Log.md", "![[receipt.png]]\n")

	if err := r.snapshotter().Commit(context.Background(), "move", "scan.png", "receipt.png", "Log.md"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if subject, files := r.lastCommit(); subject != "move" || strings.Join(files, ",") != "Log.md" {
		t.Errorf("HEAD = %q %v, want move [Log.md]", subject, files)
	}
}

// A note name is a file name, not a pattern: "Log [1].md" must not match
// "Log 1.md", whose uncommitted edit is a human's.
func TestCommitPathsAreLiteral(t *testing.T) {
	v := newTestVault(t)
	r := newGitRepo(t, v.root)
	write(t, v, "Log [1].md", "one\n")
	write(t, v, "Log 1.md", "human\n")
	r.git("add", "--", "Log [1].md", "Log 1.md")
	r.git("commit", "--quiet", "-m", "seed")
	write(t, v, "Log [1].md", "one, relinked\n")
	write(t, v, "Log 1.md", "human, still editing\n")

	if err := r.snapshotter().Commit(context.Background(), "relink", "Log [1].md"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := r.git("show", "--name-only", "--format=", "HEAD"); got != "Log [1].md" {
		t.Errorf("HEAD committed %q, want only Log [1].md", got)
	}
}

// move_file end to end on the voice surface: what the model is told, and what
// the snapshot repo records.
func TestMoveFileToolUpdatesLinksAndCommitsThem(t *testing.T) {
	s := newSurface(t, false)
	r := newGitRepo(t, s.vault.root)
	s.cfg.snapshot = true
	s.cfg.stampAgent = "voice"
	s.snaps = r.snapshotter()
	write(t, s.vault, "Attachments/scan.png", "png")
	write(t, s.vault, "Trip.md", "![[scan.png]]\n")
	write(t, s.vault, "Other.md", "nothing\n")
	r.git("add", "Trip.md", "Other.md")
	r.git("commit", "--quiet", "-m", "seed")

	msg, isErr := callText(t, voiceSession(t, s), "move_file", map[string]any{"from": "Attachments/scan.png", "to": "Attachments/receipt.png"})

	if isErr || !strings.Contains(msg, "Updated 1 link in Trip.md.") {
		t.Fatalf("move_file said %q (error %v)", msg, isErr)
	}
	subject, files := r.lastCommit()
	if subject != "voice: move Attachments/scan.png to Attachments/receipt.png (1 link updated)" || strings.Join(files, ",") != "Trip.md" {
		t.Errorf("HEAD = %q %v", subject, files)
	}
}
