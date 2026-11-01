// These tests ARE the updater's trust boundary, run against real git and gpg.
//
// A fake git would test the fake. What has to hold is that the real binaries,
// driven exactly as updater.go drives them, refuse every case below: an
// unsigned tip, a tip signed by a key HEAD does not list, a tip that lists its
// own key, origin moving after the preview, a dirty tree, a diverged branch, a
// rewritten remote, and a hook. golang:1.26-bookworm (the Dockerfile's build
// stage) and CI's runner both ship git and gpg, so these never skip.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is an origin, a developer clone that pushes to it, and the NAS
// checkout the updater works on.
type fixture struct {
	t       *testing.T
	root    string
	origin  string // bare repo, served as file://
	dev     string // where commits are made
	nas     string // the updater's REPO_DIR
	gnupg   string // holds both test keys
	trusted string // fingerprint listed in the signer file
	rogue   string // fingerprint NOT listed
	u       *updater
}

func requireTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s is required for the updater tests and is not on PATH: %v", name, err)
	}
	return p
}

func (f *fixture) sh(dir string, env []string, name string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.root,
		"GNUPGHOME=" + f.gnupg,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com",
	}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	return f.sh(dir, nil, "git", append([]string{"-c", "protocol.file.allow=always", "-c", "gpg.program=gpg"}, args...)...)
}

func (f *fixture) genKey(uid string) string {
	f.t.Helper()
	f.sh(f.root, nil, "gpg", "--batch", "--passphrase", "", "--quick-gen-key", uid, "ed25519", "sign", "never")
	colons := f.sh(f.root, nil, "gpg", "--batch", "--with-colons", "--list-keys", uid)
	for fpr := range primaryFingerprints(colons) {
		return fpr
	}
	f.t.Fatalf("no fingerprint for %s", uid)
	return ""
}

func (f *fixture) write(dir, rel, content string) {
	f.t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// commit makes a commit in dev touching rel, signed by key ("" for unsigned),
// pushes it, and returns its sha.
func (f *fixture) commit(rel, content, key string) string {
	f.t.Helper()
	f.write(f.dev, rel, content)
	f.git(f.dev, "add", "-A")
	args := []string{"commit", "-q", "-m", "change " + rel}
	if key != "" {
		args = append([]string{"-c", "user.signingkey=" + key}, append(args, "-S")...)
	} else {
		args = append(args, "--no-gpg-sign")
	}
	f.git(f.dev, args...)
	f.git(f.dev, "push", "-q", "origin", "main")
	return f.git(f.dev, "rev-parse", "HEAD")
}

func (f *fixture) head() string {
	f.t.Helper()
	return f.git(f.nas, "rev-parse", "HEAD")
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gitBin := requireTool(t, "git")
	gpgBin := requireTool(t, "gpg")

	root := t.TempDir()
	f := &fixture{
		t:      t,
		root:   root,
		origin: filepath.Join(root, "origin.git"),
		dev:    filepath.Join(root, "dev"),
		nas:    filepath.Join(root, "nas"),
		gnupg:  filepath.Join(root, "g"),
	}
	if err := os.Mkdir(f.gnupg, 0o700); err != nil {
		t.Fatal(err)
	}
	// The agent this starts for signing is the test's, not the updater's: the
	// updater writes no-autostart and never signs.
	t.Cleanup(func() {
		c := exec.Command("gpgconf", "--kill", "all")
		c.Env = []string{"GNUPGHOME=" + f.gnupg, "PATH=" + os.Getenv("PATH")}
		_ = c.Run()
	})

	f.trusted = f.genKey("Trusted <trusted@example.com>")
	f.rogue = f.genKey("Rogue <rogue@example.com>")

	f.git(root, "init", "-q", "--bare", "-b", "main", f.origin)
	f.git(root, "init", "-q", "-b", "main", f.dev)
	f.git(f.dev, "remote", "add", "origin", "file://"+f.origin)

	keyring := f.sh(root, nil, "gpg", "--batch", "--armor", "--export", f.trusted)
	f.write(f.dev, "dashboard/updater-signers.asc", keyring+"\n")
	f.write(f.dev, "vault-mcp/docker-compose.yml", "services: {}\n")
	f.write(f.dev, "dashboard/docker-compose.yml", "services: {}\n")
	f.write(f.dev, "docs/notes.md", "hello\n")
	f.write(f.dev, ".gitignore", ".env\n")
	f.git(f.dev, "add", "-A")
	f.git(f.dev, "-c", "user.signingkey="+f.trusted, "commit", "-q", "-S", "-m", "initial")
	f.git(f.dev, "push", "-q", "origin", "main")

	f.git(root, "clone", "-q", "file://"+f.origin, f.nas)
	// Untracked and ignored, like the real .env: must not count as dirty.
	f.write(f.nas, "vault-mcp/.env", "MCP_TOKEN=x\n")

	f.u = &updater{
		repoDir:           f.nas,
		remoteURL:         "file://" + f.origin,
		branch:            "main",
		signersPath:       "dashboard/updater-signers.asc",
		token:             "updater-secret",
		gitBin:            gitBin,
		gpgBin:            gpgBin,
		allowFileProtocol: true,
	}
	return f
}

func TestUpdaterPreviewsAndAppliesASignedFastForward(t *testing.T) {
	f := newFixture(t)
	before := f.head()
	target := f.commit("vault-mcp/docker-compose.yml", "services: {a: {}}\n", f.trusted)

	p := f.u.preview(context.Background())
	if p.Err != "" {
		t.Fatalf("preview: %s", p.Err)
	}
	if p.Status != UpdateStatusBehind || p.Count != 1 || p.Target != target || p.Head != before {
		t.Fatalf("preview = %+v", p)
	}
	if len(p.Stacks) != 1 || p.Stacks[0] != "vault-mcp" {
		t.Fatalf("stacks = %v, want [vault-mcp]", p.Stacks)
	}
	if p.Signature == nil || !p.Signature.OK || p.Signature.Fingerprint != f.trusted {
		t.Fatalf("signature = %+v, want OK by %s", p.Signature, f.trusted)
	}
	if f.head() != before {
		t.Fatal("preview moved HEAD")
	}

	a := f.u.apply(context.Background(), target)
	if a.Err != "" || a.Status != UpdateStatusApplied {
		t.Fatalf("apply = %+v", a)
	}
	if f.head() != target {
		t.Fatalf("HEAD = %s, want %s", f.head(), target)
	}
	if got, _ := os.ReadFile(filepath.Join(f.nas, "vault-mcp/.env")); string(got) != "MCP_TOKEN=x\n" {
		t.Fatalf(".env was touched: %q", got)
	}

	again := f.u.preview(context.Background())
	if again.Status != UpdateStatusCurrent || again.Signature != nil {
		t.Fatalf("after apply, preview = %+v", again)
	}
}

func TestUpdaterRefusesAnUnsignedTip(t *testing.T) {
	f := newFixture(t)
	before := f.head()
	target := f.commit("docs/notes.md", "unsigned\n", "")

	p := f.u.preview(context.Background())
	if p.Signature == nil || p.Signature.OK {
		t.Fatalf("preview signature = %+v, want a refusal", p.Signature)
	}
	a := f.u.apply(context.Background(), target)
	if a.Err == "" || !strings.Contains(a.Err, "not signed") {
		t.Fatalf("apply error = %q, want it to say the commit is not signed", a.Err)
	}
	if f.head() != before {
		t.Fatal("an unsigned tip was applied")
	}
}

func TestUpdaterRefusesAKeyHEADDoesNotList(t *testing.T) {
	f := newFixture(t)
	before := f.head()
	target := f.commit("docs/notes.md", "rogue\n", f.rogue)

	a := f.u.apply(context.Background(), target)
	if a.Err == "" || a.Signature == nil || a.Signature.OK {
		t.Fatalf("apply = %+v, want a signature refusal", a)
	}
	if f.head() != before {
		t.Fatal("a tip signed by an unlisted key was applied")
	}
}

// THE CASE THAT DECIDES WHERE THE KEYRING IS READ FROM. A commit that adds its
// own signer to the file and signs with that signer must not verify: the file
// is read at HEAD, which does not list the key yet.
func TestUpdaterRefusesATipThatListsItsOwnKey(t *testing.T) {
	f := newFixture(t)
	before := f.head()
	both := f.sh(f.root, nil, "gpg", "--batch", "--armor", "--export", f.trusted, f.rogue)
	target := f.commit("dashboard/updater-signers.asc", both+"\n", f.rogue)

	a := f.u.apply(context.Background(), target)
	if a.Err == "" || a.Signature == nil || a.Signature.OK {
		t.Fatalf("apply = %+v, want a refusal", a)
	}
	if f.head() != before {
		t.Fatal("a tip vouching for itself was applied")
	}

	// And the legitimate route works: the trusted key signs the change that
	// adds the new one, after which the new one is accepted.
	f.git(f.dev, "reset", "-q", "--hard", before)
	f.git(f.dev, "push", "-q", "--force", "origin", "main")
	added := f.commit("dashboard/updater-signers.asc", both+"\n", f.trusted)
	if a := f.u.apply(context.Background(), added); a.Err != "" {
		t.Fatalf("applying the trusted key's change: %s", a.Err)
	}
	next := f.commit("docs/notes.md", "now trusted\n", f.rogue)
	if a := f.u.apply(context.Background(), next); a.Err != "" {
		t.Fatalf("the newly listed key was refused: %s", a.Err)
	}
}

// The reviewed-sha guard: apply takes the commit the human saw, never "whatever
// origin is now", even when the new tip descends from it.
func TestUpdaterRefusesWhenOriginMovedSinceThePreview(t *testing.T) {
	f := newFixture(t)
	before := f.head()
	reviewed := f.commit("docs/notes.md", "one\n", f.trusted)
	if p := f.u.preview(context.Background()); p.Target != reviewed {
		t.Fatalf("preview target = %s, want %s", p.Target, reviewed)
	}
	f.commit("docs/notes.md", "two\n", f.trusted)

	a := f.u.apply(context.Background(), reviewed)
	if !strings.Contains(a.Err, "preview again") {
		t.Fatalf("apply error = %q, want a request to preview again", a.Err)
	}
	if f.head() != before {
		t.Fatal("apply moved HEAD after origin changed under it")
	}
}

func TestUpdaterRefusesADirtyTrackedFile(t *testing.T) {
	f := newFixture(t)
	target := f.commit("docs/notes.md", "upstream\n", f.trusted)
	f.write(f.nas, "vault-mcp/docker-compose.yml", "services: {edited: {}}\n")

	a := f.u.apply(context.Background(), target)
	if !strings.Contains(a.Err, "uncommitted changes") {
		t.Fatalf("apply error = %q, want a dirty-tree refusal", a.Err)
	}
}

func TestUpdaterRefusesADivergedCheckout(t *testing.T) {
	f := newFixture(t)
	f.write(f.nas, "docs/local.md", "made on the NAS\n")
	f.git(f.nas, "add", "-A")
	f.git(f.nas, "commit", "-q", "--no-gpg-sign", "-m", "local")
	local := f.head()
	target := f.commit("docs/notes.md", "upstream\n", f.trusted)

	if p := f.u.preview(context.Background()); p.Status != UpdateStatusDiverged {
		t.Fatalf("preview status = %q, want diverged", p.Status)
	}
	a := f.u.apply(context.Background(), target)
	if a.Err == "" {
		t.Fatalf("apply on a diverged checkout succeeded: %+v", a)
	}
	if f.head() != local {
		t.Fatal("apply moved a diverged checkout")
	}
}

func TestUpdaterRefusesTheWrongBranchOrRemote(t *testing.T) {
	t.Run("other branch", func(t *testing.T) {
		f := newFixture(t)
		f.git(f.nas, "checkout", "-q", "-b", "experiment")
		if p := f.u.preview(context.Background()); !strings.Contains(p.Err, "experiment") {
			t.Fatalf("preview error = %q, want it to name the branch", p.Err)
		}
	})
	t.Run("detached", func(t *testing.T) {
		f := newFixture(t)
		f.git(f.nas, "checkout", "-q", "--detach")
		if p := f.u.preview(context.Background()); !strings.Contains(p.Err, "detached") {
			t.Fatalf("preview error = %q, want detached", p.Err)
		}
	})
	t.Run("origin points elsewhere", func(t *testing.T) {
		f := newFixture(t)
		f.git(f.nas, "remote", "set-url", "origin", "https://example.com/evil.git")
		if p := f.u.preview(context.Background()); !strings.Contains(p.Err, "resolves to") {
			t.Fatalf("preview error = %q, want it to name the URL", p.Err)
		}
	})
	t.Run("insteadOf rewrites origin at fetch time", func(t *testing.T) {
		// A real, fetchable repo, so the only thing that can refuse is the URL
		// check -- not a fetch that happens to fail.
		f := newFixture(t)
		evil := filepath.Join(f.root, "evil.git")
		f.git(f.root, "clone", "-q", "--bare", "file://"+f.origin, evil)
		f.git(f.nas, "config", "url.file://"+evil+".insteadOf", "file://"+f.origin)
		if p := f.u.preview(context.Background()); !strings.Contains(p.Err, "resolves to") || !strings.Contains(p.Err, "evil") {
			t.Fatalf("preview error = %q, want the rewritten URL refused", p.Err)
		}
	})
}

// A hook in .git/hooks, or one pointed at by the local config, must not run.
// Either would be a command executing in the container that writes the checkout.
func TestUpdaterRunsNoHooks(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(f.root, "hook-ran")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	for _, dir := range []string{filepath.Join(f.nas, ".git", "hooks"), filepath.Join(f.root, "hooks")} {
		f.write(dir, "post-merge", hook)
		f.write(dir, "reference-transaction", hook)
		if err := os.Chmod(filepath.Join(dir, "post-merge"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, "reference-transaction"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.git(f.nas, "config", "core.hooksPath", filepath.Join(f.root, "hooks"))

	target := f.commit("docs/notes.md", "hooked\n", f.trusted)
	if a := f.u.apply(context.Background(), target); a.Err != "" {
		t.Fatalf("apply: %s", a.Err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran during apply")
	}
}

func TestUpdaterChildEnvironmentCarriesNoToken(t *testing.T) {
	t.Setenv("DASH_UPDATER_TOKEN", "updater-secret")
	t.Setenv("DASH_AGENT_TOKEN", "agent-secret")
	for _, kv := range updaterChildEnv() {
		if strings.Contains(kv, "secret") || strings.HasPrefix(kv, "DASH_") {
			t.Errorf("child environment carries %q", kv)
		}
	}
}

func TestUpdaterRefusesTheAgentToken(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPO_DIR", repo)
	t.Setenv("DASH_UPDATER_TOKEN", "u")
	t.Setenv("DASH_AGENT_TOKEN", "a")
	if _, err := newUpdaterFromEnv(); err == nil || !strings.Contains(err.Error(), "DASH_AGENT_TOKEN") {
		t.Fatalf("err = %v, want a refusal naming DASH_AGENT_TOKEN", err)
	}
}

func TestJudgeSignature(t *testing.T) {
	const fpr = "968479A1AFF927E37D1A566BB5690EEEBB952194"
	trusted := map[string]bool{fpr: true}
	good := "[GNUPG:] NEWSIG\n" +
		"[GNUPG:] GOODSIG B5690EEEBB952194 GitHub <noreply@github.com>\n" +
		"[GNUPG:] VALIDSIG " + fpr + " 2026-11-01 1793532583 0 4 0 1 8 00 " + fpr + "\n" +
		"[GNUPG:] TRUST_UNDEFINED 0 pgp\n"

	if v := judgeSignature(good, trusted); !v.OK || v.Signer != "GitHub <noreply@github.com>" || v.Fingerprint != fpr {
		t.Fatalf("good signature judged %+v", v)
	}

	refused := map[string]string{
		"unsigned":          "",
		"no public key":     "[GNUPG:] ERRSIG 374EAD6B18FBEFBC 1 8 00 1786566177 9 39EB\n[GNUPG:] NO_PUBKEY 374EAD6B18FBEFBC\n",
		"bad":               "[GNUPG:] BADSIG B5690EEEBB952194 GitHub\n",
		"expired key":       strings.Replace(good, "GOODSIG", "EXPKEYSIG", 1),
		"revoked key":       strings.Replace(good, "GOODSIG", "REVKEYSIG", 1),
		"good but unlisted": strings.ReplaceAll(good, fpr, "0000000000000000000000000000000000000000"),
		"two signatures":    good + good,
		"no VALIDSIG":       "[GNUPG:] GOODSIG B5690EEEBB952194 GitHub\n",
		"error text only":   "error: gpg.ssh.allowedSignersFile needs to be configured\n",
	}
	for name, status := range refused {
		if v := judgeSignature(status, trusted); v.OK || v.Reason == "" {
			t.Errorf("%s: judged %+v, want a refusal with a reason", name, v)
		}
	}
}

func TestRepoPathSafe(t *testing.T) {
	for _, p := range []string{"dashboard/updater-signers.asc", "signers.asc"} {
		if !repoPathSafe(p) {
			t.Errorf("%q rejected", p)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x", "-x", "a//b", "./a", "HEAD:x", "a\nb", "."} {
		if repoPathSafe(p) {
			t.Errorf("%q accepted", p)
		}
	}
}

// The committed keyring must import and hold exactly the keys its header
// documents. A file that fails to import refuses every update, and one that
// grew an unexplained key widens who can move the checkout.
func TestCommittedSignerFile(t *testing.T) {
	requireTool(t, "gpg")
	data, err := os.ReadFile("updater-signers.asc")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "gpg.conf"), []byte("no-autostart\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := &updater{repoDir: home, gpgBin: "gpg"}
	env := []string{"GNUPGHOME=" + home}
	if _, _, err := u.run(context.Background(), "gpg", env, strings.NewReader(string(data)), "--batch", "--quiet", "--import"); err != nil {
		t.Fatalf("import: %v", err)
	}
	listing, _, err := u.run(context.Background(), "gpg", env, nil, "--batch", "--with-colons", "--list-keys")
	if err != nil {
		t.Fatal(err)
	}
	got := primaryFingerprints(listing)
	want := map[string]bool{"968479A1AFF927E37D1A566BB5690EEEBB952194": true}
	if len(got) != len(want) {
		t.Fatalf("keyring holds %v, want exactly %v", got, want)
	}
	for fpr := range want {
		if !got[fpr] {
			t.Fatalf("keyring holds %v, missing %s", got, fpr)
		}
		if !strings.Contains(string(data), fpr) {
			t.Errorf("%s is in the keyring but not documented in its header", fpr)
		}
	}
}
