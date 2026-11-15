package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The bug these exist for: search returned "1. Atlas/Fishing/Fishing Shopping
// List.md", and read_note refused every way a voice session asked for it.
func TestReadFindsANoteByNameWhenThePathMisses(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "1. Atlas/Fishing/Fishing Shopping List.md", "rods\n")

	for _, ref := range []string{
		"1. Atlas/Fishing/Fishing Shopping List", // the path, which always worked
		"Atlas/Fishing/Fishing Shopping List.md", // the "1. " dropped
		"Atlas/Fishing/Fishing Shopping List",
		"Fishing Shopping List", // a bare title
		"fishing shopping list", // case, as an Obsidian link ignores it
		"Fishing/Fishing Shopping List",
	} {
		rel, byName, body, err := v.readWindow(ref, 0, 0)
		if err != nil {
			t.Errorf("readWindow(%q): %v", ref, err)
			continue
		}
		if rel != "1. Atlas/Fishing/Fishing Shopping List.md" || body != "rods\n" {
			t.Errorf("readWindow(%q) = %q, %q", ref, rel, body)
		}
		if want := ref != "1. Atlas/Fishing/Fishing Shopping List"; byName != want {
			t.Errorf("readWindow(%q) byName = %v, want %v", ref, byName, want)
		}
	}
}

// A reference that is a path still means that path. A root note shares its
// name with a nested one all the time, and reading it must not become an
// ambiguity error.
func TestLiteralPathWinsOverANameMatch(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Pike.md", "root\n")
	write(t, v, "Fishing/Pike.md", "nested\n")

	rel, byName, body, err := v.readWindow("Pike", 0, 0)
	if err != nil || rel != "Pike.md" || byName || body != "root\n" {
		t.Errorf("readWindow = %q, %v, %q, %v; want the root note, not by name", rel, byName, body, err)
	}
}

func TestAmbiguousNameNamesEveryMatch(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "2. Areas/Pike.md", "a\n")
	write(t, v, "1. Atlas/Fishing/Pike.md", "b\n")

	_, err := v.Read("Pike")
	var amb *AmbiguousRefError
	if !errors.As(err, &amb) {
		t.Fatalf("Read = %v, want AmbiguousRefError", err)
	}
	want := []string{"1. Atlas/Fishing/Pike.md", "2. Areas/Pike.md"}
	if amb.Total != 2 || !reflect.DeepEqual(amb.Matches, want) {
		t.Errorf("matches = %d %v, want %v", amb.Total, amb.Matches, want)
	}
	// More of the path settles it.
	if body, err := v.Read("Fishing/Pike"); err != nil || body != "b\n" {
		t.Errorf("Read(Fishing/Pike) = %q, %v", body, err)
	}
}

func TestAmbiguityListIsCapped(t *testing.T) {
	v := newTestVault(t)
	for _, d := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		write(t, v, d+"/Log.md", "x\n")
	}
	_, err := v.Read("Log")
	var amb *AmbiguousRefError
	if !errors.As(err, &amb) || amb.Total != 7 || len(amb.Matches) != maxRefMatches {
		t.Fatalf("Read = %v (%+v), want 7 matches, %d named", err, amb, maxRefMatches)
	}
}

func TestNoMatchIsStillNotFound(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "1. Atlas/Fishing/Fishing Shopping List.md", "rods\n")
	for _, ref := range []string{"Fly Shopping List", "Atlas/Hiking/Fishing Shopping List", "List"} {
		if _, err := v.Read(ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Read(%q) = %v, want ErrNotFound", ref, err)
		}
	}
}

// The lookup must not be a way into what the vault-wide search hides, nor an
// oracle for it: an excluded twin neither matches nor makes a name ambiguous.
func TestNameLookupHonoursExclusions(t *testing.T) {
	v := newTestVault(t, "4. Inbox", "Private/Diary")
	write(t, v, "4. Inbox/clipping.md", "secret\n")
	write(t, v, "Private/Diary.md", "secret\n")
	write(t, v, "4. Inbox/Pike.md", "hidden twin\n")
	write(t, v, "Fishing/Pike.md", "visible\n")

	for _, ref := range []string{"clipping", "Inbox/clipping", "Diary"} {
		if _, err := v.Read(ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Read(%q) = %v, want ErrNotFound", ref, err)
		}
		if _, err := v.SearchNote(ref, "secret", 0); !errors.Is(err, ErrNotFound) {
			t.Errorf("SearchNote(%q) = %v, want ErrNotFound", ref, err)
		}
		if _, err := v.Edit(ref, "secret", "y"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Edit(%q) = %v, want ErrNotFound", ref, err)
		}
	}
	// The literal path keeps its own answer.
	if _, err := v.Read("4. Inbox/clipping"); !errors.Is(err, ErrExcluded) {
		t.Errorf("Read(4. Inbox/clipping) = %v, want ErrExcluded", err)
	}
	if body, err := v.Read("Pike"); err != nil || body != "visible\n" {
		t.Errorf("Read(Pike) = %q, %v; want the visible note alone", body, err)
	}
}

func TestNameLookupSkipsDottedFolders(t *testing.T) {
	v := newTestVault(t)
	write(t, v, ".claude/Policy.md", "x\n")
	write(t, v, ".trash/Old note.md", "x\n")
	for _, ref := range []string{"Policy", "Old note"} {
		if _, err := v.Read(ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Read(%q) = %v, want ErrNotFound", ref, err)
		}
	}
}

func TestNameLookupIgnoresSymlinksOutOfTheVault(t *testing.T) {
	v := newTestVault(t)
	outside := filepath.Join(t.TempDir(), "Secret.md")
	if err := os.WriteFile(outside, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(v.root, "Links"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(v.root, "Links", "Secret.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Read("Secret"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read(Secret) = %v, want ErrNotFound", err)
	}
}

// A bare title is a note and never a base, by name as at the root.
func TestNameLookupKeepsTheBaseRule(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "1. Atlas/Fly Reference.base", "views:\n  - type: table\n")

	if _, err := v.Read("Fly Reference"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read(Fly Reference) = %v, want ErrNotFound", err)
	}
	rel, byName, _, err := v.readWindow("Fly Reference.base", 0, 0)
	if err != nil || rel != "1. Atlas/Fly Reference.base" || !byName {
		t.Errorf("readWindow(Fly Reference.base) = %q, %v, %v", rel, byName, err)
	}
}

func TestRefMatchesNumberPrefixes(t *testing.T) {
	for _, c := range []struct {
		want, cand string
		ok         bool
	}{
		{"Atlas/Fishing/Pike.md", "1. Atlas/Fishing/Pike.md", true}, // a dropped number
		{"Pike.md", "1. Atlas/Fishing/Pike.md", true},               // a bare title
		{"1. Atlas/Pike.md", "1. Atlas/Pike.md", true},              // the number given, and right
		{"2. Atlas/Pike.md", "1. Atlas/Pike.md", false},             // the number given, and wrong
		{"02. Areas/Pike.md", "2. Areas/Pike.md", false},            // numbers compare as written
		{"10. Archive/x.md", "10. Archive/x.md", true},              // more than one digit
		{"Intro.md", "Atlas/1. Intro.md", true},                     // a numbered file too
		{"2 Notes.md", "Notes/1.2 Notes.md", false},                 // no space: part of the title
		{"Plan.md", "2026.11 Plan.md", false},                       // a date, not a number prefix
		{"Intro.md", "Atlas/1.Intro.md", false},                     // the space is required
		{"atlas/pike.md", "1. Atlas/Pike.md", true},                 // case
		{"las/Pike.md", "1. Atlas/Pike.md", false},                  // whole components only
		{"Fishing/Atlas/Pike.md", "Atlas/Pike.md", false},           // longer than the candidate
	} {
		if got := refMatches(splitRef(c.want), splitRef(c.cand)); got != c.ok {
			t.Errorf("refMatches(%q, %q) = %v, want %v", c.want, c.cand, got, c.ok)
		}
	}
}

// The reviewer's case: the caller named the folder's number, and it was not
// this one, so neither a read nor an edit may land there.
func TestAGivenNumberMustMatch(t *testing.T) {
	v := newTestVault(t)
	p := write(t, v, "1. Projects/Plan.md", "- one\n")
	if _, err := v.Read("2. Projects/Plan"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read(2. Projects/Plan) = %v, want ErrNotFound", err)
	}
	if _, err := v.Edit("2. Projects/Plan", "- one", "- two"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Edit(2. Projects/Plan) = %v, want ErrNotFound", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "- one\n" {
		t.Errorf("note = %q, want it untouched", got)
	}
}

func TestSearchNoteFindsTheNoteByName(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "1. Atlas/Fishing/Fishing Shopping List.md", "rods\nreels\n")

	rel, byName, hits, err := v.searchNote("Atlas/Fishing/Fishing Shopping List", "reels", 0)
	if err != nil {
		t.Fatal(err)
	}
	if rel != "1. Atlas/Fishing/Fishing Shopping List.md" || !byName || len(hits) != 1 || hits[0].Line != 2 {
		t.Errorf("searchNote = %q, %v, %+v", rel, byName, hits)
	}
}

// Edit needs the note to exist, so finding it by name changes where the edit
// lands and nothing else.
func TestEditFindsTheNoteByName(t *testing.T) {
	v := newTestVault(t)
	p := write(t, v, "1. Atlas/Fishing/Fishing Shopping List.md", "- rods\n")

	rel, err := v.Edit("Fishing Shopping List", "- rods", "- rods\n- reels")
	if err != nil {
		t.Fatal(err)
	}
	if rel != "1. Atlas/Fishing/Fishing Shopping List.md" {
		t.Errorf("Edit wrote %q", rel)
	}
	if got, _ := os.ReadFile(p); !strings.Contains(string(got), "- reels") {
		t.Errorf("note = %q", got)
	}
}

// What a caller actually does: read by name, then append by the same name. The
// append must not create a root note that shadows the real one from then on.
func TestReadThenAppendByNameDoesNotCreateAShadow(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "1. Atlas/Fishing/Fishing Shopping List.md", "- rods\n")

	if _, err := v.Read("Fishing Shopping List"); err != nil {
		t.Fatal(err)
	}
	_, err := v.Append("Fishing Shopping List", "- reel")
	var ns *NamesakeError
	if !errors.As(err, &ns) || ns.Path != "Fishing Shopping List.md" ||
		!reflect.DeepEqual(ns.Matches, []string{"1. Atlas/Fishing/Fishing Shopping List.md"}) {
		t.Fatalf("Append = %v (%+v), want a NamesakeError naming the real note", err, ns)
	}
	if exists(t, v, "Fishing Shopping List.md") {
		t.Error("the refused append created the root note anyway")
	}
	// The path it named works, and so does an append to a note that exists.
	if _, err := v.Append("1. Atlas/Fishing/Fishing Shopping List", "- reel"); err != nil {
		t.Errorf("Append by path: %v", err)
	}
	// With no namesake an append still creates, as it always has.
	if rel, err := v.Append("Packing list", "- socks"); err != nil || rel != "Packing list.md" {
		t.Errorf("Append(Packing list) = %q, %v", rel, err)
	}
}

// Capture's note is configured, not a guess, so a namesake never stops it.
func TestCaptureIgnoresNamesakes(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Archive/Inbox.md", "old\n")
	if rel, err := v.Capture("Inbox", "buy tippet", time.Now()); err != nil || rel != "Inbox.md" {
		t.Errorf("Capture = %q, %v", rel, err)
	}
}

// An excluded namesake must not refuse an append either: the refusal would
// name, or at least reveal, a note voice cannot see.
func TestAppendNamesakeCheckHonoursExclusions(t *testing.T) {
	v := newTestVault(t, "4. Inbox")
	write(t, v, "4. Inbox/Packing list.md", "hidden\n")
	if rel, err := v.Append("Packing list", "- socks"); err != nil || rel != "Packing list.md" {
		t.Errorf("Append = %q, %v; want a new root note", rel, err)
	}
}

// Move and delete take the path they are given and nothing else: a name that
// matches some other file is a move or a delete of the wrong one.
func TestMoveAndTrashDoNotLookUpByName(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "1. Atlas/Pike.md", "x\n")
	if _, _, err := v.Move("Pike", "Elsewhere/Pike"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Move = %v, want ErrNotFound", err)
	}
	if _, err := v.Trash(context.Background(), "Pike", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("Trash = %v, want ErrNotFound", err)
	}
}

func TestBasesDocFoundByNameKeepsThePathItFound(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "1. Atlas/Systems & Workflow/Obsidian/Bases Tips & Syntax.md", "x\n")
	if err := v.SetBasesDoc("Atlas/Systems & Workflow/Obsidian/Bases Tips & Syntax"); err != nil {
		t.Fatal(err)
	}
	if got := v.BasesDoc(); got != "1. Atlas/Systems & Workflow/Obsidian/Bases Tips & Syntax" {
		t.Errorf("BasesDoc = %q", got)
	}
}

func toolText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("empty result: %+v", res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T", res.Content[0])
	}
	return tc.Text
}

// The reply names the path a lookup found on a line of its own above the
// note, and the note below it is byte-identical: edit anchors are copied from
// it.
func TestReadNoteReplyNamesThePathItFound(t *testing.T) {
	s := newSurface(t, false)
	write(t, s.vault, "1. Atlas/Fishing/Fishing Shopping List.md", "- rods\n")
	ctx := context.Background()

	res, _, err := s.readNote(ctx, nil, readInput{Note: "Atlas/Fishing/Fishing Shopping List"})
	if err != nil {
		t.Fatal(err)
	}
	want := "[No note at that path; found it by name at \"1. Atlas/Fishing/Fishing Shopping List.md\". Use that path from now on.]\n- rods\n"
	if got := toolText(t, res); got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}

	res, _, err = s.readNote(ctx, nil, readInput{Note: "1. Atlas/Fishing/Fishing Shopping List"})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolText(t, res); got != "- rods\n" {
		t.Errorf("literal path reply = %q, want the note alone", got)
	}
}

func TestReadNoteAmbiguityReplyQuotesEachPath(t *testing.T) {
	s := newSurface(t, false)
	write(t, s.vault, "1. Atlas/Pike.md", "a\n")
	write(t, s.vault, "2. Areas/Pike.md", "b\n")

	res, _, err := s.readNote(context.Background(), nil, readInput{Note: "Pike"})
	if err != nil {
		t.Fatal(err)
	}
	got := toolText(t, res)
	if !res.IsError || !strings.Contains(got, `"1. Atlas/Pike.md", "2. Areas/Pike.md"`) {
		t.Errorf("reply = %q (error %v), want both paths quoted", got, res.IsError)
	}
}

// Quoted, so "1. " cannot pass for list numbering.
func TestSearchAndListQuotePaths(t *testing.T) {
	s := newSurface(t, false)
	write(t, s.vault, "1. Atlas/Fishing/Fishing Shopping List.md", "rods\n")
	ctx := context.Background()

	res, _, err := s.searchNotes(ctx, nil, searchInput{Query: "shopping"})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolText(t, res); !strings.Contains(got, "\n- \"1. Atlas/Fishing/Fishing Shopping List.md\" — ") {
		t.Errorf("search reply = %q", got)
	}

	res, _, err = s.listNotes(ctx, nil, listInput{})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolText(t, res); got != "- \"1. Atlas/\"\n" {
		t.Errorf("list reply = %q", got)
	}

	res, _, err = s.searchNotes(ctx, nil, searchInput{Query: "rods", Note: "Fishing Shopping List"})
	if err != nil {
		t.Fatal(err)
	}
	got := toolText(t, res)
	if !strings.HasPrefix(got, "[No note at that path; found it by name at \"1. Atlas/Fishing/Fishing Shopping List.md\"") ||
		!strings.Contains(got, "1 line(s) in \"1. Atlas/Fishing/Fishing Shopping List.md\" matching \"rods\"") {
		t.Errorf("note search reply = %q", got)
	}
}

// A symlink into .claude/ was readable by its own path before the lookup, and
// the lookup made it reachable by a bare title. Neither may reach it now, and
// neither may search or an edit's anchor errors.
func TestSymlinkIntoDotClaudeIsHiddenEverywhere(t *testing.T) {
	v := newTestVault(t)
	write(t, v, ".claude/secret.md", "policy\n")
	if err := os.MkdirAll(filepath.Join(v.root, "Notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(v.root, ".claude", "secret.md"), filepath.Join(v.root, "Notes", "Policy.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Read("Notes/Policy"); !errors.Is(err, ErrDenied) {
		t.Errorf("Read(Notes/Policy) = %v, want ErrDenied", err)
	}
	if _, err := v.Read("Policy"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read(Policy) = %v, want ErrNotFound", err)
	}
	if _, err := v.Edit("Notes/Policy", "policy", "x"); !errors.Is(err, ErrDenied) {
		t.Errorf("Edit(Notes/Policy) = %v, want ErrDenied", err)
	}
	hits, err := v.Search("policy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("Search = %+v, want nothing from .claude", hits)
	}
}

// A lookup that cannot read the whole vault fails closed as not found, and
// the server logs why.
func TestLookupGivesUpOnAnUnreadableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder whatever its mode")
	}
	s := newSurface(t, false)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	write(t, s.vault, "1. Atlas/Pike.md", "x\n")
	locked := filepath.Join(s.vault.root, "Locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	_, err := s.vault.Read("Pike")
	var li *LookupIncompleteError
	if !errors.As(err, &li) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read = %v, want a LookupIncompleteError that is ErrNotFound", err)
	}
	res, _, err := s.readNote(context.Background(), nil, readInput{Note: "Pike"})
	if err != nil || !res.IsError {
		t.Fatalf("readNote = %+v, %v", res, err)
	}
	if !strings.Contains(logs.String(), "lookup by name gave up") || !strings.Contains(logs.String(), "Locked") {
		t.Errorf("log = %q, want the give-up and the folder", logs.String())
	}
}

// ...but a folder the lookup never enters cannot turn it off.
func TestLookupIgnoresAnUnreadableExcludedFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a folder whatever its mode")
	}
	v := newTestVault(t, "4. Inbox")
	write(t, v, "1. Atlas/Pike.md", "x\n")
	locked := filepath.Join(v.root, "4. Inbox")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if body, err := v.Read("Pike"); err != nil || body != "x\n" {
		t.Errorf("Read(Pike) = %q, %v", body, err)
	}
}

func TestLookupGivesUpAtTheCap(t *testing.T) {
	old := maxLookupFiles
	maxLookupFiles = 2
	t.Cleanup(func() { maxLookupFiles = old })

	v := newTestVault(t)
	for _, d := range []string{"a", "b", "c"} {
		write(t, v, d+"/Note "+d+".md", "x\n")
	}
	var li *LookupIncompleteError
	if _, err := v.Read("Note c"); !errors.As(err, &li) {
		t.Errorf("Read past the cap = %v, want LookupIncompleteError", err)
	}
	// And never refuses an append: the check can only add a refusal.
	if _, err := v.Append("Note a", "y"); err != nil {
		t.Errorf("Append past the cap = %v", err)
	}
}

func TestAppendAndCreateRepliesNameTheNamesake(t *testing.T) {
	s := newSurface(t, false)
	write(t, s.vault, "1. Atlas/Pike.md", "x\n")
	ctx := context.Background()

	res, _, err := s.appendNote(ctx, nil, appendInput{Note: "Pike", Text: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolText(t, res); !res.IsError || !strings.Contains(got, `"1. Atlas/Pike.md"`) || !strings.Contains(got, "create_note") {
		t.Errorf("append reply = %q", got)
	}

	res, _, err = s.createNote(ctx, nil, createInput{Note: "Pike", Content: "new\n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolText(t, res); res.IsError || !strings.HasSuffix(got, ` Other notes have that name too: "1. Atlas/Pike.md".`) {
		t.Errorf("create reply = %q", got)
	}
}
