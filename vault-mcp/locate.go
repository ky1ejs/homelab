package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// locate.go: finding the note a caller means when the path it gave holds none.
//
// resolve reads a reference as a path from the vault root, so "Fishing
// Shopping List" meant the root's Fishing Shopping List.md and nothing else,
// while read_note's own description promised "by title or path". Search
// returned "1. Atlas/Fishing/Fishing Shopping List.md", and a voice session
// asked for it three ways — the full path without its "1. ", the same without
// ".md", the bare title — and got "There is no note at that path" each time.
// The "1. " was lost because a search result line read "- 1. Atlas/...", which
// is a numbered list item inside a bullet: the number looked like formatting.
//
// So a read that finds nothing at the literal path looks the note up by name,
// the way an Obsidian link does: any document whose path ends with the
// reference. A folder or file name the caller gave WITHOUT a number prefix
// ("Atlas") also matches one that has one ("1. Atlas"), since that is the part
// a caller drops; a name given WITH a number must match it, so "2. Areas/Pike"
// never opens "1. Atlas/Pike". Exactly one match is the note; several are an
// AmbiguousRefError naming each, so the next call can be the right one; none
// is ErrNotFound as before.
//
// Used by read_note, search_notes' 'note' and edit_note: each needs the file to
// exist already, so finding it elsewhere cannot change what gets created.
// Deliberately not used by append_note and create_note, which create a missing
// note at the literal path, nor by move_file and delete_file, where a name that
// happens to match a second file is a move or a delete of the wrong one.
// append_note does consult it, the other way round: see NamesakeError.

// numberPrefix is the "1. " of "1. Atlas": digits, a dot, then whitespace. The
// whitespace is required, so "1.2 Notes" and "2026.11 Plan" keep their number.
var numberPrefix = regexp.MustCompile(`^[0-9]+\.\s+`)

// maxRefMatches caps how many candidates an ambiguity error names: enough to
// choose between, short enough to say aloud.
const maxRefMatches = 5

// maxLookupFiles caps the lookup's walk, at the search's cap. A variable only
// so a test can reach it without writing twenty thousand files.
var maxLookupFiles = maxSearchFiles

// AmbiguousRefError is a name more than one visible document has.
type AmbiguousRefError struct {
	Matches []string // vault paths, sorted, at most maxRefMatches
	Total   int      // how many matched in all
}

func (e *AmbiguousRefError) Error() string {
	return fmt.Sprintf("%d files match that name", e.Total)
}

func newAmbiguous(matches []string) *AmbiguousRefError {
	e := &AmbiguousRefError{Total: len(matches), Matches: matches}
	if len(e.Matches) > maxRefMatches {
		e.Matches = e.Matches[:maxRefMatches]
	}
	return e
}

// NamesakeError is an append that would create a note at its literal path
// while a note of that name exists elsewhere. Before the lookup it was the
// caller's mistake to make; after it, "read X" succeeds by name and "append to
// X" is the natural next call, which would create a root X that from then on
// shadows the real one, since the literal path always wins. Refused instead,
// naming where X is: to add to it, use that path; to start a new note, use
// create_note.
type NamesakeError struct {
	Path    string   // the literal path the append would have created
	Matches []string // where notes of that name are, at most maxRefMatches
	Total   int
}

func (e *NamesakeError) Error() string {
	return fmt.Sprintf("no note at %s, but %d of that name exist elsewhere", e.Path, e.Total)
}

// LookupIncompleteError is a lookup by name that could not finish: a folder it
// could not read, or the file cap. It reports as ErrNotFound, failing closed,
// because a partial walk can make a name look unique and open the wrong note;
// the type exists so the server can log why the lookup gave up.
type LookupIncompleteError struct{ Cause error }

func (e *LookupIncompleteError) Error() string {
	return "lookup by name incomplete: " + e.Cause.Error()
}

func (e *LookupIncompleteError) Is(target error) bool { return target == ErrNotFound }

// locate is resolve for a note that must already exist. It returns the path
// and whether it was found by name rather than at the literal path, so the
// reply can tell the caller the path to use next time.
func (v *Vault) locate(ref string) (string, bool, error) {
	abs, err := v.resolve(ref)
	if err != nil {
		return "", false, err
	}
	// Lstat, not Stat: a symlink that resolve accepted is the literal path's
	// file, broken or not, and the read reports it as such.
	if _, err := os.Lstat(abs); !errors.Is(err, fs.ErrNotExist) {
		return abs, false, nil
	}

	matches, err := v.findByName(v.Rel(abs))
	switch {
	case err != nil:
		return "", false, &LookupIncompleteError{Cause: err}
	case len(matches) == 0:
		return abs, false, nil
	case len(matches) > 1:
		return "", false, newAmbiguous(matches)
	}
	// Through resolve again, so the match gets every check the literal path
	// did: containment, symlinks, exclusions.
	found, err := v.resolve(matches[0])
	if err != nil {
		return "", false, err
	}
	return found, true, nil
}

// namesakes is the lookup for a note about to be created at abs: the visible
// documents elsewhere that a by-name read of the same reference would open. A
// lookup that cannot finish reports none, since it only ever adds a refusal or
// a warning.
func (v *Vault) namesakes(abs string) []string {
	rel := v.Rel(abs)
	matches, err := v.findByName(rel)
	if err != nil {
		return nil
	}
	out := matches[:0]
	for _, m := range matches {
		if m != rel {
			out = append(out, m)
		}
	}
	return out
}

// refPart is one component of a path, split into its number prefix and the
// rest, both lower-cased for comparison.
type refPart struct{ num, name string }

func splitRef(rel string) []refPart {
	comps := strings.Split(filepath.ToSlash(rel), "/")
	parts := make([]refPart, len(comps))
	for i, c := range comps {
		c = strings.ToLower(strings.TrimSpace(c))
		num := numberPrefix.FindString(c)
		parts[i] = refPart{num: strings.TrimSpace(num), name: c[len(num):]}
	}
	return parts
}

// refMatches reports whether candidate's path ends with want, component by
// component. A component of want with no number matches the candidate's with
// or without one; a component with a number must have the same number.
func refMatches(want, cand []refPart) bool {
	if len(want) > len(cand) {
		return false
	}
	cand = cand[len(cand)-len(want):]
	for i, w := range want {
		c := cand[i]
		if w.name != c.name || (w.num != "" && w.num != c.num) {
			return false
		}
	}
	return true
}

// linkVisible reports whether a symlinked leaf found on a walk may be shown:
// its target must be inside the vault, outside MCP_EXCLUDE, and outside every
// dotted folder, since the walk itself never enters one. Without the last
// test a link such as "Notes/Policy.md" pointing into .claude/ would surface
// the tool policy in search results and in a lookup by name.
func (v *Vault) linkVisible(p string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !within(v.root, real) || v.Excluded(real) {
		return false
	}
	for _, c := range strings.Split(v.Rel(real), "/") {
		if strings.HasPrefix(c, ".") {
			return false
		}
	}
	return true
}

// findByName lists the documents a search could return whose path ends with
// rel, as refMatches compares them. The walk skips exactly what Search skips,
// so a name cannot reach a note the vault-wide search would not have shown:
// dotted folders, MCP_EXCLUDE, and symlinks out of the vault or into either.
// Hidden files are left out of the count too, or an ambiguity error would
// reveal that they exist.
func (v *Vault) findByName(rel string) ([]string, error) {
	want := splitRef(rel)
	var matches []string
	scanned := 0
	err := filepath.WalkDir(v.root, func(p string, d fs.DirEntry, err error) error {
		// The skips come before the error check, so an unreadable folder that
		// the lookup would never have entered cannot turn it off.
		if p != v.root && d != nil && d.IsDir() && (strings.HasPrefix(d.Name(), ".") || v.Excluded(p)) {
			return fs.SkipDir
		}
		if err != nil {
			// Unlike Search, a skipped subtree is not harmless here: it could
			// hold the second file that makes the name ambiguous.
			return fmt.Errorf("could not read %s: %w", v.Rel(p), err)
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || !isDocument(d.Name()) || v.Excluded(p) {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 && !v.linkVisible(p) {
			return nil
		}
		if scanned++; scanned > maxLookupFiles {
			return fmt.Errorf("the vault has more than %d notes", maxLookupFiles)
		}
		r := v.Rel(p)
		if refMatches(want, splitRef(r)) {
			matches = append(matches, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}
