package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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

func TestRefKeyStripsOnlyNumberPrefixes(t *testing.T) {
	for in, want := range map[string]string{
		"1. Atlas/Fishing/Pike.md": "atlas/fishing/pike.md",
		"02. Areas/Pike.md":        "areas/pike.md",
		"Notes/1.2 Hooks.md":       "notes/1.2 hooks.md",    // no space: a title
		"2026.11 Plan.md":          "2026.11 plan.md",       // a date, not a number prefix
		"Atlas/1. Intro.md":        "atlas/intro.md",        // a numbered file too
		"Atlas/1.Intro.md":         "atlas/1.intro.md",      // the space is required
		"10. Archive/x.md":         "archive/x.md",          // more than one digit
		"Atlas/Fishing/Pike.md":    "atlas/fishing/pike.md", // nothing to strip
	} {
		if got := refKey(in); got != want {
			t.Errorf("refKey(%q) = %q, want %q", in, got, want)
		}
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
// lands and nothing else. Append creates, so it keeps the literal path.
func TestEditFindsByNameButAppendKeepsThePath(t *testing.T) {
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

	rel, err = v.Append("Fishing Shopping List", "- line")
	if err != nil {
		t.Fatal(err)
	}
	if rel != "Fishing Shopping List.md" {
		t.Errorf("Append wrote %q, want a new note at the literal path", rel)
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
