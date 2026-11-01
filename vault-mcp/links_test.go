package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Links that follow a move. The cases are the forms Obsidian itself writes and
// reads, so a failure here is a link that would have broken in the vault.

func mustMove(t *testing.T, v *Vault, from, to string) *MoveResult {
	t.Helper()
	res, err := v.MoveWithLinks(from, to)
	if err != nil {
		t.Fatalf("MoveWithLinks(%q, %q): %v", from, to, err)
	}
	return res
}

func TestRenameRewritesEveryWikiLinkForm(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "4. Inbox/Pike.md", "# Pike\n")
	write(t, v, "Log.md", strings.Join([]string{
		"plain [[Pike]]",
		"alias [[Pike|the big one]]",
		"heading [[Pike#Tackle]]",
		"block [[Pike#^abc123]]",
		"both [[Pike#Tackle|tackle]]",
		"embed ![[Pike]]",
		"extension [[Pike.md]]",
		"path [[4. Inbox/Pike]]",
		"| table | [[Pike\\|alias]] |",
		"",
	}, "\n"))

	res := mustMove(t, v, "4. Inbox/Pike", "4. Inbox/Northern Pike")

	want := strings.Join([]string{
		"plain [[Northern Pike]]",
		"alias [[Northern Pike|the big one]]",
		"heading [[Northern Pike#Tackle]]",
		"block [[Northern Pike#^abc123]]",
		"both [[Northern Pike#Tackle|tackle]]",
		"embed ![[Northern Pike]]",
		"extension [[Northern Pike.md]]",
		"path [[4. Inbox/Northern Pike]]",
		"| table | [[Northern Pike\\|alias]] |",
		"",
	}, "\n")
	if got := read(t, v, "Log.md"); got != want {
		t.Errorf("Log.md after rename:\n%s\nwant:\n%s", got, want)
	}
	if res.Links != 9 || len(res.Relinked) != 1 || res.Relinked[0] != "Log.md" {
		t.Errorf("result = %+v", res)
	}
}

// The case the README used to warn about only in prose: a folder move in a
// shortest-path vault breaks nothing, and so must CHANGE nothing. A note
// rewritten to identical links would still be stamped and committed, which is
// noise in the snapshot repo and a sync round-trip on every device.
func TestFolderMoveLeavesNameLinksAlone(t *testing.T) {
	v := newTestVault(t)
	v.SetStampAgent("claude-agent")
	write(t, v, "4. Inbox/Pike.md", "# Pike\n")
	before := "see [[Pike]] and ![[Pike#Tackle]] and [x](Pike.md)\n"
	write(t, v, "Log.md", before)

	res := mustMove(t, v, "4. Inbox/Pike", "Projects/Fishing/Pike")

	if got := read(t, v, "Log.md"); got != before {
		t.Errorf("Log.md was rewritten by a move that broke none of its links:\n%s", got)
	}
	if res.Links != 0 || len(res.Relinked) != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestFolderMoveRewritesPathLinks(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "4. Inbox/Pike.md", "# Pike\n")
	write(t, v, "Log.md", "[[4. Inbox/Pike|pike]] and [[/4. Inbox/Pike]] and [notes](4.%20Inbox/Pike.md#Tackle \"title\")\n")

	mustMove(t, v, "4. Inbox/Pike", "Projects/Fishing Notes/Pike")

	want := "[[Projects/Fishing Notes/Pike|pike]] and [[/Projects/Fishing Notes/Pike]] and [notes](Projects/Fishing%20Notes/Pike.md#Tackle \"title\")\n"
	if got := read(t, v, "Log.md"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarkdownLinkForms(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Notes/Pike.md", "# Pike\n")
	write(t, v, "Notes/Log.md", strings.Join([]string{
		"[rel](./Pike.md)",
		"[up](../Notes/Pike.md)",
		"[angle](<Notes/Pike.md>)",
		"![img](Pike.md)",
		"[web](https://example.com/Pike.md)",
		"[self](#Pike)",
		"",
	}, "\n"))

	mustMove(t, v, "Notes/Pike", "Notes/Esox")

	want := strings.Join([]string{
		"[rel](./Esox.md)",
		"[up](Esox.md)",
		"[angle](<Notes/Esox.md>)",
		"![img](Esox.md)",
		"[web](https://example.com/Pike.md)",
		"[self](#Pike)",
		"",
	}, "\n")
	if got := read(t, v, "Notes/Log.md"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The moved note's own relative links are relative to where it USED to be.
func TestMovedNoteKeepsItsRelativeLinks(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Attachments/map.png", "png")
	write(t, v, "Lakes/Wells.md", "# Wells\n")
	write(t, v, "4. Inbox/Pike.md", "![map](../Attachments/map.png)\nsee [lake](../Lakes/Wells.md) and [[Wells]] and [[#Tackle]]\n")

	res := mustMove(t, v, "4. Inbox/Pike", "Projects/Fishing/2026/Pike")

	want := "![map](../../../Attachments/map.png)\nsee [lake](../../../Lakes/Wells.md) and [[Wells]] and [[#Tackle]]\n"
	if got := read(t, v, "Projects/Fishing/2026/Pike.md"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if len(res.Relinked) != 1 || res.Relinked[0] != "Projects/Fishing/2026/Pike.md" || res.Links != 2 {
		t.Errorf("result = %+v", res)
	}
}

func TestRenamedNoteLinksToItself(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n\nback to [[Pike#Top]] or [[#Top]]\n")

	mustMove(t, v, "Pike", "Esox")

	if got := read(t, v, "Esox.md"); !strings.Contains(got, "back to [[Esox#Top]] or [[#Top]]") {
		t.Errorf("self link not updated:\n%s", got)
	}
}

func TestAttachmentRenameRewritesEmbeds(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Attachments/scan.png", "png")
	write(t, v, "Trip.md", "![[scan.png]]\n![[scan.png|300]]\n![](Attachments/scan.png)\n")

	mustMove(t, v, "Attachments/scan.png", "Attachments/receipt.png")

	want := "![[receipt.png]]\n![[receipt.png|300]]\n![](Attachments/receipt.png)\n"
	if got := read(t, v, "Trip.md"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBaseRenameRewritesEmbeds(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Flies.base", "views: []\n")
	write(t, v, "Home.md", "![[Flies.base]]\n")

	mustMove(t, v, "Flies.base", "Fly Box.base")

	if got := read(t, v, "Home.md"); got != "![[Fly Box.base]]\n" {
		t.Errorf("got %q", got)
	}
}

// A bare name is written only when it is unique in the whole vault. Rule 3's
// tie-break is an approximation of Obsidian's, and a rewrite never bets on it.
func TestRewriteUsesFullPathWhenNameIsShared(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "4. Inbox/Trout.md", "# Trout\n")
	write(t, v, "Archive/Pike.md", "# Old pike\n")
	write(t, v, "Log.md", "[[Trout]]\n")

	mustMove(t, v, "4. Inbox/Trout", "Projects/Pike")

	if got := read(t, v, "Log.md"); got != "[[Projects/Pike]]\n" {
		t.Errorf("got %q", got)
	}
}

// A move can break a link to a DIFFERENT note: one that resolved by name to
// Archive/Pike before, and would resolve to the moved note after.
func TestMoveThatShadowsAnotherNoteRewritesItsLinks(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Archive/Pike.md", "# Old pike\n")
	write(t, v, "4. Inbox/Draft.md", "# Draft\n")
	write(t, v, "Log.md", "[[Pike]]\n")

	mustMove(t, v, "4. Inbox/Draft", "Log/Pike") // "Log/Pike.md" is shorter than "Archive/Pike.md"

	if got := read(t, v, "Log.md"); got != "[[Archive/Pike]]\n" {
		t.Errorf("got %q, want the link pinned to the note it always meant", got)
	}
}

func TestCodeIsNeverRewritten(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	before := "`[[Pike]]` and ``a `[[Pike]]` b``\n```\n[[Pike]]\n```\n~~~~md\n[x](Pike.md)\n~~~~\nreal [[Pike]]\n"
	write(t, v, "Docs.md", before)

	mustMove(t, v, "Pike", "Esox")

	want := strings.Replace(before, "real [[Pike]]", "real [[Esox]]", 1)
	if got := read(t, v, "Docs.md"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFrontmatterLinksAreRewritten(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Trip.md", "---\nfish: \"[[Pike]]\"\nalso:\n  - \"[[Pike|pike]]\"\n---\n\nBody.\n")

	mustMove(t, v, "Pike", "Esox")

	got := read(t, v, "Trip.md")
	if !strings.Contains(got, "fish: \"[[Esox]]\"") || !strings.Contains(got, "- \"[[Esox|pike]]\"") {
		t.Errorf("frontmatter links not updated:\n%s", got)
	}
}

func TestRewrittenNotesAreStamped(t *testing.T) {
	v := newTestVault(t)
	v.SetStampAgent("claude-agent")
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Log.md", "[[Pike]]\n")
	write(t, v, "Other.md", "nothing here\n")

	mustMove(t, v, "Pike", "Esox")

	if got := read(t, v, "Log.md"); !strings.Contains(got, "agent: claude-agent") || !strings.Contains(got, "[[Esox]]") {
		t.Errorf("rewritten note not stamped:\n%s", got)
	}
	if got := read(t, v, "Other.md"); got != "nothing here\n" {
		t.Errorf("untouched note was written:\n%s", got)
	}
}

// AGENTS.md and CLAUDE.md are never written, by a move or anything else. A
// link in one is reported so the user can fix it by hand.
func TestProtectedNotesAreReportedNotWritten(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "AGENTS.md", "file under [[Pike]]\n")

	res := mustMove(t, v, "Pike", "Esox")

	if got := read(t, v, "AGENTS.md"); got != "file under [[Pike]]\n" {
		t.Errorf("AGENTS.md was written:\n%s", got)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Path != "AGENTS.md" {
		t.Errorf("Skipped = %+v", res.Skipped)
	}
}

// An exclusion promises this server never opens those notes. A move does not
// break that promise; the result says the folder was not checked instead.
func TestExcludedNotesAreNotRead(t *testing.T) {
	v := newTestVault(t, "Private")
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Private/Diary.md", "[[Pike]]\n")

	res := mustMove(t, v, "Pike", "Esox")

	if got := read(t, v, "Private/Diary.md"); got != "[[Pike]]\n" {
		t.Errorf("excluded note was rewritten:\n%s", got)
	}
	if !res.ExcludedUnchecked {
		t.Error("result does not say excluded folders went unchecked")
	}
}

func TestBrokenLinksAreLeftAlone(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	before := "[[Nothing]] and [[Pike Notes]] and [x](missing.md)\n"
	write(t, v, "Log.md", before)

	mustMove(t, v, "Pike", "Esox")

	if got := read(t, v, "Log.md"); got != before {
		t.Errorf("got %q", got)
	}
}

func TestResolveRules(t *testing.T) {
	ix := newLinkIndex()
	for _, p := range []string{"Pike.md", "Notes/Pike.md", "Notes/Deep/Trout.md", "Attachments/scan.png", "Chapter 1.2.md"} {
		ix.add(p)
	}
	cases := []struct{ target, source, want string }{
		{"Pike", "Log.md", "Pike.md"},                         // root path wins
		{"Pike", "Notes/Log.md", "Pike.md"},                   // still root path, before relative
		{"./Pike", "Notes/Log.md", "Notes/Pike.md"},           // explicitly relative
		{"../Pike", "Notes/Deep/Log.md", "Notes/Pike.md"},     // up one
		{"Trout", "Log.md", "Notes/Deep/Trout.md"},            // by name
		{"deep/trout", "Log.md", "Notes/Deep/Trout.md"},       // tail of a path, any case
		{"scan.png", "Log.md", "Attachments/scan.png"},        // attachment by name
		{"Chapter 1.2", "Log.md", "Chapter 1.2.md"},           // a dot in a title is not an extension
		{"/Notes/Pike", "Notes/Deep/Log.md", "Notes/Pike.md"}, // leading slash is the root
	}
	for _, c := range cases {
		got, _, _, ok := ix.resolve(c.target, c.source)
		if !ok || got != c.want {
			t.Errorf("resolve(%q from %q) = %q, %v; want %q", c.target, c.source, got, ok, c.want)
		}
	}
	for _, miss := range []string{"../Pike", "./Trout", "Salmon"} {
		if got, _, _, ok := ix.resolve(miss, "Log.md"); ok {
			t.Errorf("resolve(%q) = %q, want no match", miss, got)
		}
	}
}

func TestMoveSummaryNamesWhatChanged(t *testing.T) {
	got := moveSummary(&MoveResult{
		From: "Pike.md", To: "Esox.md", Links: 3,
		Relinked:          []string{"A.md", "B.md"},
		Skipped:           []SkippedNote{{Path: "AGENTS.md", Reason: "protected file"}},
		ExcludedUnchecked: true,
	})
	for _, want := range []string{"Moved Pike.md to Esox.md.", "Updated 3 links in A.md, B.md.", "AGENTS.md (protected file)", "were not checked"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}

// Obsidian editing a note between the plan and the write must not lose that
// edit. The plan is redone against the note as it is now.
func TestRelinkReplansAfterAConcurrentEdit(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Log.md", "[[Pike]]\n")

	before, err := v.buildLinkIndex()
	if err != nil {
		t.Fatal(err)
	}
	after := before.clone()
	after.remove("Pike.md")
	after.add("Esox.md")
	res := &MoveResult{}
	plans := v.planRelinks(write(t, v, "Pike.md", "# Pike\n"), filepath.Join(v.root, "Esox.md"), before, after, res)
	if len(plans) != 1 {
		t.Fatalf("plans = %+v", plans)
	}

	write(t, v, "Log.md", "[[Pike]]\nadded in Obsidian\n")
	if n, err := v.applyRelink(plans[0], "Pike.md", "Esox.md", before, after); err != nil || n != 1 {
		t.Fatalf("applyRelink = %d, %v", n, err)
	}
	if got := read(t, v, "Log.md"); got != "[[Esox]]\nadded in Obsidian\n" {
		t.Errorf("got %q", got)
	}
}

// A note at the vault root matches a bare name by its path, but the link was
// still written as a name and stays one.
func TestRootNoteKeepsNameStyleWhenMovedAndRenamed(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Log.md", "[[Pike]]\n")

	mustMove(t, v, "Pike", "Projects/Esox")

	if got := read(t, v, "Log.md"); got != "[[Esox]]\n" {
		t.Errorf("got %q", got)
	}
}
