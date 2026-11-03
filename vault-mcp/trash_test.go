package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustTrash(t *testing.T, v *Vault, ref string, allow bool, linkedFrom ...string) *TrashResult {
	t.Helper()
	res, err := v.Trash(context.Background(), ref, allow, linkedFrom...)
	if err != nil {
		t.Fatalf("Trash(%q): %v", ref, err)
	}
	return res
}

func exists(t *testing.T, v *Vault, rel string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(v.root, filepath.FromSlash(rel)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// A delete is a rename into .trash under the file's own path, and the bytes
// arrive untouched: no stamp, no rewrite. That is what makes it recoverable.
func TestTrashMovesTheFileUnderItsOwnPath(t *testing.T) {
	v := newTestVault(t)
	v.SetStampAgent("claude-voice")
	body := "---\ntags: [fish]\n---\n# Pike\n"
	write(t, v, "Projects/Fishing/Pike.md", body)

	res := mustTrash(t, v, "Projects/Fishing/Pike", false)

	if res.Path != "Projects/Fishing/Pike.md" || res.Trash != ".trash/Projects/Fishing/Pike.md" {
		t.Errorf("result = %+v", res)
	}
	if exists(t, v, "Projects/Fishing/Pike.md") {
		t.Error("the note is still at its old path")
	}
	if got := read(t, v, ".trash/Projects/Fishing/Pike.md"); got != body {
		t.Errorf("trashed note changed:\n%s", got)
	}
	// Folders are never removed, even when the delete empties one.
	if !exists(t, v, "Projects/Fishing") {
		t.Error("the emptied folder was removed")
	}
}

func TestTrashTakesBasesAndAttachments(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Views/Hatches.base", "views:\n  - type: table\n")
	writeBytes(t, v, "Attachments/scan.png", []byte{0x89, 'P', 'N', 'G', 0, 1, 2})

	mustTrash(t, v, "Views/Hatches.base", false)
	mustTrash(t, v, "Attachments/scan.png", false)

	if got := read(t, v, ".trash/Views/Hatches.base"); got != "views:\n  - type: table\n" {
		t.Errorf("base = %q", got)
	}
	if got := read(t, v, ".trash/Attachments/scan.png"); got != "\x89PNG\x00\x01\x02" {
		t.Errorf("attachment = %q", got)
	}
}

// A second delete of the same path must not replace the first one's copy:
// that would be the one place this server destroys content.
func TestTrashNumbersACollision(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "first\n")
	mustTrash(t, v, "Pike", false)
	write(t, v, "Pike.md", "second\n")
	mustTrash(t, v, "Pike", false)
	write(t, v, "Pike.md", "third\n")
	res := mustTrash(t, v, "Pike", false)

	if res.Trash != ".trash/Pike 2.md" {
		t.Errorf("third delete went to %s", res.Trash)
	}
	for rel, want := range map[string]string{".trash/Pike.md": "first\n", ".trash/Pike 1.md": "second\n", ".trash/Pike 2.md": "third\n"} {
		if got := read(t, v, rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

func TestTrashKeepsDotsInTitles(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Chapter 1.2.md", "x\n")
	write(t, v, ".trash/Chapter 1.2.md", "older\n")

	res := mustTrash(t, v, "Chapter 1.2", false)

	if res.Trash != ".trash/Chapter 1.2 1.md" {
		t.Errorf("trash = %s", res.Trash)
	}
}

// The safe default: a file other notes link to is not deleted, and the refusal
// names every one of them so the user can decide.
func TestTrashRefusesALinkedFile(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Catskills/Airbnbs.md", "# list\n")
	write(t, v, "Catskills/Cabin.md", "source: [[Airbnbs]]\n")
	write(t, v, "Trips.md", "[x](Catskills/Airbnbs.md#Top)\n")
	write(t, v, "Unrelated.md", "Airbnbs are mentioned but not linked\n")

	_, err := v.Trash(context.Background(), "Catskills/Airbnbs", false)

	var linkErr *BacklinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("err = %v, want a BacklinkError", err)
	}
	if strings.Join(linkErr.Linkers, ",") != "Catskills/Cabin.md,Trips.md" {
		t.Errorf("linkers = %v", linkErr.Linkers)
	}
	if !exists(t, v, "Catskills/Airbnbs.md") || exists(t, v, ".trash") {
		t.Error("a refused delete changed the vault")
	}
}

func TestTrashWithAllowBrokenLinksReportsThem(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Scan.md", "# s\n")
	write(t, v, "Log.md", "![[Scan]]\n")

	// The override names the notes it lets break. Without that list the
	// caller has not seen the refusal, so it gets one.
	var linkErr *BacklinkError
	if _, err := v.Trash(context.Background(), "Scan", true); !errors.As(err, &linkErr) {
		t.Fatalf("allow_broken_links alone = %v, want a BacklinkError", err)
	}
	res := mustTrash(t, v, "Scan", true, "log.md")

	if strings.Join(res.BrokenLinks, ",") != "Log.md" {
		t.Errorf("BrokenLinks = %v", res.BrokenLinks)
	}
	if got := deleteSummary(res); !strings.Contains(got, "Links to it are now broken in Log.md.") {
		t.Errorf("summary = %q", got)
	}
	if got := read(t, v, "Log.md"); got != "![[Scan]]\n" {
		t.Errorf("a delete rewrote a linking note:\n%s", got)
	}
}

// Links that do not resolve to this file are not backlinks: a link to its own
// heading inside it, a same-named note elsewhere that the link resolves to
// instead, and a link inside code.
func TestTrashIgnoresLinksThatAreNotToIt(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Old/Pike.md", "see [[#Tackle]] and [[Old/Pike]]\n")
	write(t, v, "Pike.md", "# the root one\n")
	write(t, v, "Log.md", "[[Pike]] and `[[Old/Pike]]`\n")

	res := mustTrash(t, v, "Old/Pike", false)

	if len(res.BrokenLinks) != 0 {
		t.Errorf("BrokenLinks = %v", res.BrokenLinks)
	}
}

// A note an attachment is embedded in links to it as much as a wiki link does.
func TestTrashRefusesAnEmbeddedAttachment(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Attachments/map.png", "png")
	write(t, v, "Trip.md", "![[map.png|300]]\n")

	_, err := v.Trash(context.Background(), "Attachments/map.png", false)

	var linkErr *BacklinkError
	if !errors.As(err, &linkErr) || strings.Join(linkErr.Linkers, ",") != "Trip.md" {
		t.Fatalf("err = %v", err)
	}
}

// AGENTS.md and CLAUDE.md are readable, so a link in one counts.
func TestTrashCountsLinksInProtectedNotes(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Rules.md", "# r\n")
	write(t, v, "CLAUDE.md", "follow [[Rules]]\n")

	_, err := v.Trash(context.Background(), "Rules", false)

	var linkErr *BacklinkError
	if !errors.As(err, &linkErr) || strings.Join(linkErr.Linkers, ",") != "CLAUDE.md" {
		t.Fatalf("err = %v", err)
	}
	// edit_note is denied on it, so the refusal must not offer that fix.
	if strings.Join(linkErr.ObsidianOnly, ",") != "CLAUDE.md" {
		t.Errorf("ObsidianOnly = %v", linkErr.ObsidianOnly)
	}
}

// A link added after the refusal is not one the user agreed to break.
func TestTrashRefusesALinkerMissingFromLinkedFrom(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Log.md", "[[Pike]]\n")
	write(t, v, "Later.md", "[[Pike]]\n")

	_, err := v.Trash(context.Background(), "Pike", true, "Log.md")

	var linkErr *BacklinkError
	if !errors.As(err, &linkErr) || strings.Join(linkErr.Linkers, ",") != "Later.md,Log.md" {
		t.Fatalf("err = %v, want a BacklinkError naming both", err)
	}
	if !exists(t, v, "Pike.md") {
		t.Fatal("a refused delete moved the note")
	}
}

// [[Pike]] with two Pikes and neither beside the linking note is decided by a
// tie-break that may not match Obsidian. Deleting either Pike may break it, so
// it counts for both, and the refusal says why.
func TestTrashCountsAnAmbiguousLinkForEveryCandidate(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "A/Pike.md", "# a\n")
	write(t, v, "Bb/Pike.md", "# b\n")
	write(t, v, "C/Log.md", "[[Pike]]\n")

	for _, ref := range []string{"A/Pike", "Bb/Pike"} {
		_, err := v.Trash(context.Background(), ref, false)
		var linkErr *BacklinkError
		if !errors.As(err, &linkErr) || strings.Join(linkErr.Linkers, ",") != "C/Log.md" {
			t.Fatalf("Trash(%q) = %v, want a BacklinkError naming C/Log.md", ref, err)
		}
		if strings.Join(linkErr.Ambiguous, ",") != "C/Log.md" {
			t.Errorf("Trash(%q) Ambiguous = %v", ref, linkErr.Ambiguous)
		}
	}
}

// A bare title means the file at the vault root. When another visible file
// has the same name the caller may mean that one, so it must say which.
func TestTrashRefusesABareTitleOthersShare(t *testing.T) {
	v := newTestVault(t, "Private")
	write(t, v, "Pike.md", "# root\n")
	write(t, v, "Fishing/Pike.md", "# fishing\n")

	if _, err := v.Trash(context.Background(), "Pike", false); !errors.Is(err, ErrAmbiguousName) {
		t.Fatalf("err = %v, want ErrAmbiguousName", err)
	}
	mustTrash(t, v, "Fishing/Pike", false)

	// A same-named file in an excluded folder is not something the caller
	// could have meant, and refusing over it would reveal that it exists.
	write(t, v, "Private/Pike.md", "# hidden\n")
	mustTrash(t, v, "Pike", false)
}

// A folder symlinked inside the vault reaches the file under a path the link
// index and the snapshot repo do not know, so the check would find nothing.
func TestTrashRefusesAPathThroughASymlinkedFolder(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Real/Note.md", "# n\n")
	write(t, v, "Linker.md", "[[Real/Note]]\n")
	if err := os.Symlink(filepath.Join(v.root, "Real"), filepath.Join(v.root, "Alias")); err != nil {
		t.Fatal(err)
	}

	if _, err := v.Trash(context.Background(), "Alias/Note", true); !errors.Is(err, ErrLinkedFolder) {
		t.Fatalf("err = %v, want ErrLinkedFolder", err)
	}
	if !exists(t, v, "Real/Note.md") || exists(t, v, ".trash") {
		t.Error("a refused delete changed the vault")
	}
}

// A canvas shows a file by its exact vault path. Deleting the file leaves an
// empty card, so the canvas counts, and only Obsidian can edit it.
func TestTrashCountsACanvasThatShowsIt(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Attachments/map.png", "png")
	write(t, v, "Boards/Trip.canvas", `{"nodes":[{"id":"1","type":"file","file":"Attachments/map.png","x":0,"y":0,"width":400,"height":400}],"edges":[]}`)
	write(t, v, "Boards/Other.canvas", `{"nodes":[{"id":"1","type":"text","text":"Attachments/map.png"}]}`)

	_, err := v.Trash(context.Background(), "Attachments/map.png", false)

	var linkErr *BacklinkError
	if !errors.As(err, &linkErr) || strings.Join(linkErr.Linkers, ",") != "Boards/Trip.canvas" {
		t.Fatalf("err = %v, want a BacklinkError naming Boards/Trip.canvas", err)
	}
	if strings.Join(linkErr.ObsidianOnly, ",") != "Boards/Trip.canvas" {
		t.Errorf("ObsidianOnly = %v", linkErr.ObsidianOnly)
	}
}

// Sync carries only the file types its settings allow, so an attachment's
// restore is not promised.
func TestDeleteSummaryHedgesSyncForAttachments(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Books/guide.epub", "epub")

	got := deleteSummary(mustTrash(t, v, "Books/guide.epub", false))

	if !strings.Contains(got, "If Obsidian Sync carries this file type") || !strings.Contains(got, ".trash/Books/guide.epub") {
		t.Errorf("summary = %q", got)
	}
}

// An exclusion promises this server never opens those notes; a delete does not
// break that. The result says they were not checked, and never names one.
func TestTrashDoesNotReadExcludedNotes(t *testing.T) {
	v := newTestVault(t, "Private")
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Private/Diary.md", "[[Pike]]\n")

	res := mustTrash(t, v, "Pike", false)

	if !res.ExcludedUnchecked {
		t.Error("result does not say excluded folders went unchecked")
	}
	if got := deleteSummary(res); strings.Contains(got, "Private") || !strings.Contains(got, "cannot see were not checked") {
		t.Errorf("summary = %q", got)
	}
}

// A check that could not run is not a clean check. It refuses like a linked
// file would, and takes the same override.
func TestTrashRefusesWhenLinksCannotBeChecked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable folders")
	}
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Locked/Note.md", "[[Pike]]\n")
	locked := filepath.Join(v.root, "Locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	_, err := v.Trash(context.Background(), "Pike", false)
	var checkErr *LinkCheckError
	if !errors.As(err, &checkErr) {
		t.Fatalf("err = %v, want a LinkCheckError", err)
	}
	if !exists(t, v, "Pike.md") {
		t.Fatal("a refused delete moved the note")
	}

	res := mustTrash(t, v, "Pike", true)
	if res.LinksUnchecked == "" || !strings.Contains(deleteSummary(res), "were not checked") {
		t.Errorf("result = %+v", res)
	}
}

// The deny list applies to a delete exactly as to a move: removing AGENTS.md
// or CLAUDE.md revokes the standing instructions as surely as editing them.
func TestTrashDeniedOnProtectedPaths(t *testing.T) {
	v := newTestVault(t, "Private")
	for _, rel := range []string{
		"CLAUDE.md", "AGENTS.md", "Projects/CLAUDE.md",
		".claude/settings.md", ".obsidian/app.json", ".obsidian/snippets/x.md",
		".trash/old.md", "Projects/.hidden/x.md",
		"notes.txt", "script.sh", ".mcp.json",
		"Private/Diary.md",
	} {
		write(t, v, rel, "keep\n")
	}
	for _, ref := range []string{
		"CLAUDE.md", "AGENTS.md", "Projects/CLAUDE.md",
		".claude/settings.md", ".obsidian/app.json", ".obsidian/snippets/x",
		".trash/old", "Projects/.hidden/x",
		"notes.txt", "script.sh", ".mcp.json",
		"Private/Diary",
		"../outside", "/etc/passwd",
	} {
		if _, err := v.Trash(context.Background(), ref, true); !errors.Is(err, ErrDenied) && !errors.Is(err, ErrExcluded) && !errors.Is(err, ErrOutside) {
			t.Errorf("Trash(%q) = %v, want refused", ref, err)
		}
	}
	if exists(t, v, ".trash/CLAUDE.md") || read(t, v, "CLAUDE.md") != "keep\n" {
		t.Error("a protected file was moved")
	}
}

func TestTrashRefusesMissingFilesAndSymlinks(t *testing.T) {
	v := newTestVault(t)
	if _, err := v.Trash(context.Background(), "Nothing", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	write(t, v, "Real.md", "x\n")
	if err := os.Symlink(filepath.Join(v.root, "Real.md"), filepath.Join(v.root, "Link.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := v.Trash(context.Background(), "Link", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("symlink: %v", err)
	}
	if !exists(t, v, "Link.md") || !exists(t, v, "Real.md") {
		t.Error("a refused delete moved something")
	}
}

// A symlink at .trash, or anywhere under it, would carry the note to wherever
// it points — outside the vault included. Nothing moves, and nothing is
// created at the far end.
func TestTrashRefusesASymlinkedTrashFolder(t *testing.T) {
	for _, link := range []string{".trash", ".trash/Projects"} {
		t.Run(link, func(t *testing.T) {
			v := newTestVault(t)
			outside := t.TempDir()
			write(t, v, "Projects/Pike.md", "# Pike\n")
			if err := os.MkdirAll(filepath.Dir(filepath.Join(v.root, link)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(v.root, link)); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}

			if _, err := v.Trash(context.Background(), "Projects/Pike", false); !errors.Is(err, ErrTrashUnsafe) {
				t.Fatalf("err = %v, want ErrTrashUnsafe", err)
			}
			if !exists(t, v, "Projects/Pike.md") {
				t.Error("the note moved")
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Errorf("created %d entries outside the vault", len(entries))
			}
		})
	}
}

// `ob sync` can land a file at the trash destination between choosing it and
// the rename. rename(2) would replace it; the delete takes the next free name.
func TestTrashNeverOverwritesInTrash(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "mine\n")
	withMoveHook(t, func(stage string) {
		if stage == "trash-planned" && !exists(t, v, ".trash/Pike.md") {
			write(t, v, ".trash/Pike.md", "landed meanwhile\n")
		}
	})

	res := mustTrash(t, v, "Pike", false)

	if res.Trash != ".trash/Pike 1.md" {
		t.Errorf("Trash = %q, want .trash/Pike 1.md", res.Trash)
	}
	if read(t, v, ".trash/Pike.md") != "landed meanwhile\n" || read(t, v, ".trash/Pike 1.md") != "mine\n" {
		t.Error("a delete replaced a file in .trash")
	}
}

// A trashed note drops out of search and listing, which skip dotted folders,
// and nothing here can bring it back: .trash is denied to every write, move
// included. read_note still opens it by exact path, as it opens any readable
// file — readable() hides only .claude/.
func TestTrashedNotesLeaveSearchAndCannotBeRestoredHere(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "unique-word\n")
	mustTrash(t, v, "Pike", false)

	if hits, _ := v.Search("unique-word", 10); len(hits) != 0 {
		t.Errorf("search found %v", hits)
	}
	if _, _, err := v.Move(".trash/Pike", "Pike"); !errors.Is(err, ErrDenied) {
		t.Errorf("move out of .trash = %v, want ErrDenied", err)
	}
}

// delete_file end to end on the voice surface: what the model is told, and what
// the snapshot repo records — a deletion, since .trash is excluded as
// snapshot.sh excludes it.
func TestDeleteFileToolTrashesAndCommitsTheDeletion(t *testing.T) {
	s := newSurface(t, false)
	r := newGitRepo(t, s.vault.root)
	if err := os.WriteFile(filepath.Join(r.snapDir, "vault.git", "info", "exclude"), []byte(".trash/\n*.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.snapshot = true
	s.cfg.stampAgent = "voice"
	s.snaps = r.snapshotter()
	write(t, s.vault, "Dupes/Packing.md", "# old list\n")
	write(t, s.vault, "Trip.md", "[[Packing]]\n")
	r.git("add", "Dupes/Packing.md", "Trip.md")
	r.git("commit", "--quiet", "-m", "seed")
	session := voiceSession(t, s)

	msg, isErr := callText(t, session, "delete_file", map[string]any{"path": "Dupes/Packing"})
	if !isErr || !strings.Contains(msg, "Nothing was deleted") || !strings.Contains(msg, "Trip.md") || !strings.Contains(msg, "linked_from set to exactly: Trip.md") {
		t.Fatalf("linked delete said %q (error %v)", msg, isErr)
	}

	msg, isErr = callText(t, session, "delete_file", map[string]any{"path": "Dupes/Packing", "allow_broken_links": true, "linked_from": []string{"Trip.md"}})
	if isErr || !strings.Contains(msg, "Deleted Dupes/Packing.md.") || !strings.Contains(msg, ".trash/Dupes/Packing.md") {
		t.Fatalf("delete_file said %q (error %v)", msg, isErr)
	}
	subject, files := r.lastCommit()
	if subject != "voice: delete Dupes/Packing.md (moved to .trash/Dupes/Packing.md)" || strings.Join(files, ",") != "Dupes/Packing.md" {
		t.Errorf("HEAD = %q %v", subject, files)
	}
	if status := r.git("show", "--name-status", "--format=", "HEAD"); status != "D\tDupes/Packing.md" {
		t.Errorf("HEAD records %q, want the deletion", status)
	}
}

// A note no snapshot ever took has no history. Deleting it records nothing,
// and the delete is not reported as a snapshot failure.
func TestDeleteFileOfAnUncommittedNoteCommitsNothing(t *testing.T) {
	s := newSurface(t, false)
	r := newGitRepo(t, s.vault.root)
	s.cfg.snapshot = true
	s.snaps = r.snapshotter()
	write(t, s.vault, "Seed.md", "# seed\n")
	r.git("add", "Seed.md")
	r.git("commit", "--quiet", "-m", "seed")
	write(t, s.vault, "Fresh.md", "# arrived after the last snapshot\n")
	res := mustTrash(t, s.vault, "Fresh", false)

	if err := s.snaps.Commit(context.Background(), "delete", res.Path); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if subject, _ := r.lastCommit(); subject != "seed" {
		t.Errorf("HEAD = %q, want seed", subject)
	}
}
