package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newImportPair returns a vault and a separate scratch root, the way
// vault-claude sees them: two mounts, no relationship between the paths.
func newImportPair(t *testing.T) (*Vault, *Vault) {
	t.Helper()
	return newTestVault(t), newTestVault(t)
}

func writeAt(t *testing.T, v *Vault, rel string, body string) {
	t.Helper()
	p := filepath.Join(v.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The case the whole tool exists for, and the one that was impossible before:
// an image downloaded by the research agent reaching the vault.
func TestImportBringsAnImageIntoTheVault(t *testing.T) {
	vault, scratch := newImportPair(t)
	writeAt(t, scratch, "flies/images/adams-dry.jpg", "\xff\xd8\xffbytes")

	from, to, err := vault.ImportAttachment(scratch, "flies/images/adams-dry.jpg", "6. Attachments/adams-dry.jpg")
	if err != nil {
		t.Fatalf("ImportAttachment: %v", err)
	}
	if from != filepath.FromSlash("flies/images/adams-dry.jpg") {
		t.Errorf("from = %q", from)
	}
	if to != filepath.FromSlash("6. Attachments/adams-dry.jpg") {
		t.Errorf("to = %q", to)
	}

	got, err := os.ReadFile(filepath.Join(vault.root, "6. Attachments", "adams-dry.jpg"))
	if err != nil {
		t.Fatalf("not in the vault: %v", err)
	}
	if string(got) != "\xff\xd8\xffbytes" {
		t.Errorf("contents differ: %q", got)
	}

	// A copy, not a move: the source mount is read-only in production, and
	// scratch-sweep.sh must stay the only thing that removes anything there.
	if _, err := os.Stat(filepath.Join(scratch.root, "flies/images/adams-dry.jpg")); err != nil {
		t.Errorf("the source was removed: %v", err)
	}
}

// No leftovers on the failure path — these land in the vault, which ob sync
// watches and snapshot.sh commits.
func TestImportLeavesNoTempFileWhenItRefuses(t *testing.T) {
	vault, scratch := newImportPair(t)
	writeAt(t, scratch, "a.jpg", "x")
	writeAt(t, vault, "Attachments/a.jpg", "mine")

	if _, _, err := vault.ImportAttachment(scratch, "a.jpg", "Attachments/a.jpg"); err == nil {
		t.Fatal("expected a refusal rather than an overwrite")
	}
	entries, err := os.ReadDir(filepath.Join(vault.root, "Attachments"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	if body, _ := os.ReadFile(filepath.Join(vault.root, "Attachments/a.jpg")); string(body) != "mine" {
		t.Errorf("the existing file was replaced: %q", body)
	}
}

// Markdown must not come through here. The agent has Read and Write for notes,
// and routing one through this tool would skip the PostToolUse stamp that gives
// an agent's writes their attribution.
func TestImportRefusesMarkdown(t *testing.T) {
	vault, scratch := newImportPair(t)
	writeAt(t, scratch, "flies/patterns.md", "# notes")

	if _, _, err := vault.ImportAttachment(scratch, "flies/patterns.md", "Research/patterns.md"); err == nil {
		t.Fatal("markdown was imported; it must go through Read and Write so it gets stamped")
	}
}

// The deny list applies to the DESTINATION exactly as it does for a move.
func TestImportObeysTheVaultDenyList(t *testing.T) {
	vault, scratch := newImportPair(t)
	writeAt(t, scratch, "a.png", "x")

	for _, dst := range []string{
		".claude/a.png",
		".obsidian/a.png",
		"notes/.hidden/a.png",
		"../escape.png",
		"/absolute.png",
	} {
		if _, _, err := vault.ImportAttachment(scratch, "a.png", dst); err == nil {
			t.Errorf("%s: allowed as an import destination", dst)
		}
	}
}

// And to the SOURCE, in its own root. A scratch volume holds the research
// agent's own policy files, and they are not attachments to be filed.
func TestImportObeysTheDenyListOnTheSource(t *testing.T) {
	vault, scratch := newImportPair(t)
	writeAt(t, scratch, ".claude/settings.json", "{}")
	writeAt(t, scratch, "sub/.hidden/a.png", "x")

	for _, src := range []string{
		".claude/settings.json",
		"sub/.hidden/a.png",
		"../outside.png",
		"/etc/passwd",
	} {
		if _, _, err := vault.ImportAttachment(scratch, src, "Attachments/x.png"); err == nil {
			t.Errorf("%s: allowed as an import source", src)
		}
	}
}

// Same rule as Move: an import never converts a file. Renaming .jpg to .md
// would put a binary on the note path, which is where a stamp-a-binary bug
// would live.
func TestImportCannotChangeTheExtension(t *testing.T) {
	vault, scratch := newImportPair(t)
	writeAt(t, scratch, "a.png", "x")

	for _, dst := range []string{"Attachments/a.md", "Attachments/a.jpg", "Attachments/a.txt"} {
		if _, _, err := vault.ImportAttachment(scratch, "a.png", dst); err == nil {
			t.Errorf("%s: extension change allowed", dst)
		}
	}
}

// Only the types move_file may relocate. A .sh or a .json in the scratch volume
// is not something to file into the vault.
func TestImportRefusesNonAttachmentTypes(t *testing.T) {
	vault, scratch := newImportPair(t)
	for _, name := range []string{"a.sh", "a.json", "a.js", "a.yaml"} {
		writeAt(t, scratch, name, "x")
		if _, _, err := vault.ImportAttachment(scratch, name, "Attachments/"+name); err == nil {
			t.Errorf("%s: imported, and only attachments may be", name)
		}
	}
}

// A symlink in the scratch volume is not a file to be copied, even when it
// stays inside that root: the bytes imported should be the bytes downloaded.
func TestImportRefusesASymlinkedSource(t *testing.T) {
	vault, scratch := newImportPair(t)
	writeAt(t, scratch, "real.png", "x")
	link := filepath.Join(scratch.root, "link.png")
	if err := os.Symlink(filepath.Join(scratch.root, "real.png"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, _, err := vault.ImportAttachment(scratch, "link.png", "Attachments/x.png"); err == nil {
		t.Fatal("a symlinked source was imported")
	}
}

func TestImportRefusesAMissingSource(t *testing.T) {
	vault, scratch := newImportPair(t)
	_, _, err := vault.ImportAttachment(scratch, "nothing.png", "Attachments/x.png")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// With no scratch root configured the tool is not registered at all, but the
// vault method must refuse rather than panic if it is ever reached.
func TestImportWithoutAScratchRootIsRefused(t *testing.T) {
	vault := newTestVault(t)
	if _, _, err := vault.ImportAttachment(nil, "a.png", "Attachments/a.png"); err == nil {
		t.Fatal("expected a refusal with no import root")
	}
}

// IMPORT_DIR is meaningless on the surface that serves claude.ai, and pointing
// it at the vault itself is what move_file is for.
func TestImportDirRejectedOnTheHTTPSurfaceAndOnTheVault(t *testing.T) {
	t.Setenv("IMPORT_DIR", "/scratch")
	t.Setenv("OAUTH_ISSUER", "")
	t.Setenv("MCP_ALLOW_NO_AUTH", "1")

	if _, err := loadConfig(false); err == nil {
		t.Error("IMPORT_DIR without -stdio should be fatal")
	}
	if _, err := loadConfig(true); err != nil {
		t.Errorf("IMPORT_DIR with -stdio should load: %v", err)
	}

	t.Setenv("VAULT_DIR", "/scratch")
	if _, err := loadConfig(true); err == nil {
		t.Error("IMPORT_DIR equal to VAULT_DIR should be fatal")
	}
}
