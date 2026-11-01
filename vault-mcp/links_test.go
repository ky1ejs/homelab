package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Links that follow a move. The cases are the forms Obsidian itself writes and
// reads, so a failure here is a link that would have broken in the vault.

func mustMove(t *testing.T, v *Vault, from, to string) *MoveResult {
	t.Helper()
	res, err := v.MoveWithLinks(context.Background(), from, to)
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
		"[up](./Esox.md)", // still explicitly relative, so it cannot be read as a name
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
//
// The other Pike has the LONGER path, so a bare [[Pike]] would resolve back to
// the moved note under the tie-break. Only uniqueName forces the full path.
func TestRewriteUsesFullPathWhenNameIsShared(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "4. Inbox/Trout.md", "# Trout\n")
	write(t, v, "Archive/Old/Pike.md", "# Old pike\n")
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
	} else if strings.Contains(got, "agent-created:") {
		t.Errorf("rewriting a link is an edit, not a creation:\n%s", got)
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
	// Not reading them means not reporting them either: a path in the result
	// is spoken to the caller, and voice must never hear one from Private/.
	var said []string
	said = append(said, res.Relinked...)
	said = append(said, res.Unfixable...)
	for _, sk := range res.Skipped {
		said = append(said, sk.Path)
	}
	said = append(said, moveSummary(res))
	for _, s := range said {
		if strings.Contains(s, "Private/") {
			t.Errorf("result names an excluded note: %q", s)
		}
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
	for _, p := range []string{"Pike.md", "Notes/Pike.md", "Notes/Deep/Trout.md", "Attachments/scan.png", "Chapter 1.2.md", "A/Bass.md", "Fish/Long/Bass.md"} {
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
		{"Bass", "Log.md", "A/Bass.md"},                       // shortest path
		{"Bass", "Fish/Long/Log.md", "Fish/Long/Bass.md"},     // own folder beats shorter
	}
	for _, c := range cases {
		got, ok := ix.resolve(c.target, c.source)
		if !ok || got.rel != c.want {
			t.Errorf("resolve(%q from %q) = %q, %v; want %q", c.target, c.source, got.rel, ok, c.want)
		}
	}
	for _, miss := range []string{"../Pike", "./Trout", "Salmon"} {
		if got, ok := ix.resolve(miss, "Log.md"); ok {
			t.Errorf("resolve(%q) = %q, want no match", miss, got.rel)
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
	plans, err := v.planRelinks(context.Background(), write(t, v, "Pike.md", "# Pike\n"), filepath.Join(v.root, "Esox.md"), before, after, res)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %+v, %v", plans, err)
	}

	write(t, v, "Log.md", "[[Pike]]\nadded in Obsidian\n")
	if n, _, err := v.applyRelink(plans[0], "Pike.md", "Esox.md", before, after); err != nil || n != 1 {
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

func withMoveHook(t *testing.T, f func(stage string)) {
	t.Helper()
	moveHook = f
	t.Cleanup(func() { moveHook = nil })
}

// `ob sync` can land a file at the destination while the move is reading the
// vault. The move must then fail with the source exactly as it was: links
// computed for a folder and name it never got would be wrong where it stayed,
// and a retry could not repair them.
func TestMoveOntoANameTakenDuringPlanningLeavesTheSourceAlone(t *testing.T) {
	v := newTestVault(t)
	v.SetStampAgent("claude-agent")
	write(t, v, "Attachments/map.png", "png")
	before := "back to [[Pike#Top]] [m](../Attachments/map.png)\n"
	write(t, v, "4. Inbox/Pike.md", before)
	withMoveHook(t, func(stage string) {
		if stage == "planned" {
			write(t, v, "Projects/Fish/Esox.md", "# landed by sync\n")
		}
	})

	if _, err := v.MoveWithLinks(context.Background(), "4. Inbox/Pike", "Projects/Fish/Esox"); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if got := read(t, v, "4. Inbox/Pike.md"); got != before {
		t.Errorf("source changed by a move that failed:\n%s", got)
	}
}

// A link whose file is in an excluded folder can only be kept pointing at it
// by writing that file's path. That would put the hidden folder's layout into
// a note voice can read, so the link is reported instead.
func TestExcludedTargetIsNeverWrittenIntoANote(t *testing.T) {
	v := newTestVault(t, "Clippings")
	write(t, v, "Clippings/Deep/Pike.md", "# hidden\n")
	write(t, v, "Projects/Esox.md", "# Esox\n")
	write(t, v, "Log.md", "[[Pike]]\n")

	res := mustMove(t, v, "Projects/Esox", "Pike")

	if got := read(t, v, "Log.md"); got != "[[Pike]]\n" {
		t.Errorf("Log.md = %q, want it left alone", got)
	}
	if len(res.Unfixable) != 1 || res.Unfixable[0] != "Log.md" {
		t.Errorf("Unfixable = %v, want [Log.md]", res.Unfixable)
	}
	if s := moveSummary(res); strings.Contains(s, "Clippings") {
		t.Errorf("summary names the excluded folder: %q", s)
	}
}

// When the tie-break chose the link's file, Obsidian may have chosen another.
// Rewriting would pin the guess, so the link is reported instead.
func TestAmbiguousNameLinkIsReportedNotRewritten(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Archive/Pike.md", "# one\n")
	write(t, v, "Notes/Deep/Pike.md", "# two\n")
	write(t, v, "Log.md", "[[Pike]]\n")

	res := mustMove(t, v, "Archive/Pike", "Archive/Esox")

	if got := read(t, v, "Log.md"); got != "[[Pike]]\n" {
		t.Errorf("Log.md = %q, want it left alone", got)
	}
	if len(res.Unfixable) != 1 || res.Unfixable[0] != "Log.md" {
		t.Errorf("Unfixable = %v", res.Unfixable)
	}
}

// Every markdown destination written must parse back to the file it names.
func TestMarkdownRewritesRoundTrip(t *testing.T) {
	cases := []struct{ from, to, link, want string }{
		{"Pike", "C#", "[x](Pike.md)", "[x](C%23.md)"},
		{"Pike", "Q3%20plan", "[x](Pike.md)", "[x](Q3%2520plan.md)"},
		{"My Pike", "Esox", "[x](<My%20Pike.md>)", "[x](<Esox.md>)"},
		{"Pike", "C# notes", "[x](<Pike.md>)", "[x](<C%23 notes.md>)"},
		{"Fishing Notes", "Fly Notes", "[x](Fishing%20Notes.md)", "[x](Fly%20Notes.md)"},
	}
	for _, c := range cases {
		v := newTestVault(t)
		write(t, v, c.from+".md", "# note\n")
		write(t, v, "Log.md", c.link+"\n")

		mustMove(t, v, c.from, c.to)

		if got := read(t, v, "Log.md"); got != c.want+"\n" {
			t.Errorf("%s -> %s: %s became %q, want %q", c.from, c.to, c.link, got, c.want)
		}
	}
}

// A file whose name a wiki link cannot carry is a link the user has to fix.
func TestUnlinkableNameIsReportedEndToEnd(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Attachments/scan.png", "png")
	write(t, v, "Trip.md", "![[scan.png]]\n")

	res := mustMove(t, v, "Attachments/scan.png", "Attachments/scan#1.png")

	if got := read(t, v, "Trip.md"); got != "![[scan.png]]\n" {
		t.Errorf("Trip.md = %q; [[scan#1.png]] would be a heading link", got)
	}
	if len(res.Unfixable) != 1 || res.Unfixable[0] != "Trip.md" {
		t.Errorf("Unfixable = %v", res.Unfixable)
	}
	if s := moveSummary(res); !strings.Contains(s, "need fixing in Obsidian: Trip.md") {
		t.Errorf("summary does not name the note: %q", s)
	}
}

func TestCodeMaskingEdgeCases(t *testing.T) {
	cases := []struct{ before, after string }{
		// A stray backtick does not hide the next paragraph.
		{"press the ` key\n\nsee [[Pike]]\n", "press the ` key\n\nsee [[Esox]]\n"},
		// ...nor shift the pairing of the spans after it.
		{"a ` b\n\n`[[Pike]]` real [[Pike]] `x`\n", "a ` b\n\n`[[Pike]]` real [[Esox]] `x`\n"},
		// An escaped backtick opens nothing.
		{"\\`[[Pike]]` x\n", "\\`[[Esox]]` x\n"},
		// Backticks after the opening run make a code span, not a fence.
		{"```js``` is inline\n\nsee [[Pike]]\n", "```js``` is inline\n\nsee [[Esox]]\n"},
	}
	for _, c := range cases {
		v := newTestVault(t)
		write(t, v, "Pike.md", "# Pike\n")
		write(t, v, "Docs.md", c.before)

		mustMove(t, v, "Pike", "Esox")

		if got := read(t, v, "Docs.md"); got != c.after {
			t.Errorf("%q became %q, want %q", c.before, got, c.after)
		}
	}
}

// A link in frontmatter is a YAML scalar, and the new path is escaped for the
// style it sits in. Where no escape exists, the link is reported.
func TestFrontmatterRewritesStayValidYAML(t *testing.T) {
	cases := []struct{ to, before, after string }{
		{"Kyle's Pike", "fish: '[[Pike]]'", "fish: '[[Kyle''s Pike]]'"},
		{`Pike "big"`, `fish: "[[Pike]]"`, `fish: "[[Pike \"big\"]]"`},
		{`Pike\fish`, `fish: "[[Pike]]"`, `fish: "[[Pike\\fish]]"`},
		{"Kyle's Pike", "title: Kyle's [[Pike]]", "title: Kyle's [[Kyle's Pike]]"},
	}
	for _, c := range cases {
		v := newTestVault(t)
		write(t, v, "Pike.md", "# Pike\n")
		write(t, v, "Trip.md", "---\n"+c.before+"\n---\n\nBody [[Pike]].\n")

		res := mustMove(t, v, "Pike", c.to)

		got := read(t, v, "Trip.md")
		if !strings.Contains(got, "\n"+c.after+"\n") || !strings.Contains(got, "Body [["+c.to+"]].") {
			t.Errorf("rename to %q: frontmatter %q gave:\n%s\nwant line %q", c.to, c.before, got, c.after)
		}
		if len(res.Unfixable) != 0 {
			t.Errorf("rename to %q: Unfixable = %v", c.to, res.Unfixable)
		}
	}

	// A plain scalar has no escape for a comma: [[Smith, John]] would become
	// a two-item flow sequence.
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Trip.md", "---\nup: [[Pike]]\n---\n\nBody [[Pike]].\n")

	res := mustMove(t, v, "Pike", "Smith, John")

	got := read(t, v, "Trip.md")
	if !strings.Contains(got, "\nup: [[Pike]]\n") || !strings.Contains(got, "Body [[Smith, John]].") {
		t.Errorf("plain scalar:\n%s", got)
	}
	if len(res.Unfixable) != 1 || res.Unfixable[0] != "Trip.md" {
		t.Errorf("Unfixable = %v", res.Unfixable)
	}
}

// A symlinked note is neither read nor written. Writing would replace the
// link with a regular file holding whatever it pointed at.
func TestSymlinkedNotesAreLeftAlone(t *testing.T) {
	v := newTestVault(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("[[Pike]]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, v, "Pike.md", "# Pike\n")
	link := filepath.Join(v.root, "Link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	res := mustMove(t, v, "Pike", "Esox")

	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("Link.md is no longer a symlink (%v)", err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "[[Pike]]\n" {
		t.Errorf("file outside the vault changed: %q", b)
	}
	for _, r := range res.Relinked {
		if r == "Link.md" {
			t.Errorf("Relinked = %v", res.Relinked)
		}
	}
}

func relinkSetup(t *testing.T) (*Vault, []relinkPlan, *linkIndex, *linkIndex) {
	t.Helper()
	v := newTestVault(t)
	from := write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Log.md", "[[Pike]]\n")
	before, err := v.buildLinkIndex()
	if err != nil {
		t.Fatal(err)
	}
	after := before.clone()
	after.remove("Pike.md")
	after.add("Esox.md")
	plans, err := v.planRelinks(context.Background(), from, filepath.Join(v.root, "Esox.md"), before, after, &MoveResult{})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %+v, %v", plans, err)
	}
	return v, plans, before, after
}

func TestSecondConcurrentEditIsReportedNotOverwritten(t *testing.T) {
	v, plans, before, after := relinkSetup(t)
	write(t, v, "Log.md", "[[Pike]]\nfirst edit\n")
	withMoveHook(t, func(stage string) {
		if stage == "replanned" {
			write(t, v, "Log.md", "[[Pike]]\nsecond edit\n")
		}
	})
	res := &MoveResult{}

	v.applyRelinks(plans, "Pike.md", "Esox.md", before, after, res)

	if got := read(t, v, "Log.md"); got != "[[Pike]]\nsecond edit\n" {
		t.Errorf("Log.md = %q, want the second edit kept", got)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != "changed in Obsidian during the move" {
		t.Errorf("Skipped = %+v", res.Skipped)
	}
}

func TestUnwritableNoteIsReported(t *testing.T) {
	v, plans, before, after := relinkSetup(t)
	p := filepath.Join(v.root, "Log.md")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	res := &MoveResult{}

	v.applyRelinks(plans, "Pike.md", "Esox.md", before, after, res)

	if len(res.Skipped) != 1 || res.Skipped[0].Reason != "could not be written" {
		t.Errorf("Skipped = %+v", res.Skipped)
	}
}

// A folder that cannot be read leaves a hole in the index, which could make a
// shared name look unique. The check is skipped and the result says so.
func TestUnreadableFolderSkipsTheLinkCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder whatever its mode")
	}
	v := newTestVault(t)
	write(t, v, "Pike.md", "# Pike\n")
	write(t, v, "Log.md", "[[Pike]]\n")
	locked := filepath.Join(v.root, "Locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	res := mustMove(t, v, "Pike", "Esox")

	if res.LinksUnchecked == "" {
		t.Errorf("result = %+v, want LinksUnchecked", res)
	}
	if got := read(t, v, "Log.md"); got != "[[Pike]]\n" {
		t.Errorf("Log.md = %q", got)
	}
}

func TestMoveSummaryReportsWhatWasNotDone(t *testing.T) {
	unchecked := moveSummary(&MoveResult{From: "Pike.md", To: "Esox.md", LinksUnchecked: "part of the vault could not be read"})
	if !strings.Contains(unchecked, "Links to it were not checked: part of the vault could not be read.") {
		t.Errorf("unchecked summary = %q", unchecked)
	}

	var skipped []SkippedNote
	var unfixable []string
	for _, n := range []string{"A", "B", "C", "D", "E", "F", "G"} {
		skipped = append(skipped, SkippedNote{Path: n + ".md", Reason: "protected file"})
		unfixable = append(unfixable, n+"2.md")
	}
	got := moveSummary(&MoveResult{From: "Pike.md", To: "Esox.md", Skipped: skipped, Unfixable: unfixable})
	// Every one is named: these are the notes someone has to go and fix.
	for _, want := range []string{"G.md (protected file)", "G2.md", "need fixing in Obsidian"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}
