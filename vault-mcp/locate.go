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
// reference. A number prefix on a folder or file ("1. ", "02. ") is ignored on
// both sides, since that is the part a caller drops. Exactly one match is the
// note; several are an AmbiguousRefError naming each, so the next call can be
// the right one; none is ErrNotFound as before.
//
// Used by read_note, search_notes' 'note' and edit_note: each needs the file to
// exist already, so finding it elsewhere cannot change what gets created.
// Deliberately not used by append_note and create_note, which create a missing
// note at the literal path, nor by move_file and delete_file, where a name that
// happens to match a second file is a move or a delete of the wrong one.

// numberPrefix is the "1. " of "1. Atlas": digits, a dot, then whitespace. The
// whitespace is required, so "1.2 Notes" and "2026.11 Plan" keep their number.
var numberPrefix = regexp.MustCompile(`^[0-9]+\.\s+`)

// maxRefMatches caps how many candidates an ambiguity error names: enough to
// choose between, short enough to say aloud.
const maxRefMatches = 5

// AmbiguousRefError is a name more than one visible document has.
type AmbiguousRefError struct {
	Matches []string // vault paths, sorted, at most maxRefMatches
	Total   int      // how many matched in all
}

func (e *AmbiguousRefError) Error() string {
	return fmt.Sprintf("%d files match that name", e.Total)
}

// errNoLookup is a lookup that could not finish. It never reaches a caller:
// a partial walk can make a name look unique, so locate falls back to the
// literal path's ErrNotFound rather than open whatever it found first.
var errNoLookup = errors.New("lookup incomplete")

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
		return abs, false, nil
	case len(matches) == 0:
		return abs, false, nil
	case len(matches) > 1:
		e := &AmbiguousRefError{Total: len(matches), Matches: matches}
		if len(e.Matches) > maxRefMatches {
			e.Matches = e.Matches[:maxRefMatches]
		}
		return "", false, e
	}
	// Through resolve again, so the match gets every check the literal path
	// did: containment, symlinks, exclusions.
	found, err := v.resolve(matches[0])
	if err != nil {
		return "", false, err
	}
	return found, true, nil
}

// refKey is the form two references are compared in: lower case, slash
// separated, each component's number prefix removed.
func refKey(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, p := range parts {
		parts[i] = strings.ToLower(numberPrefix.ReplaceAllString(strings.TrimSpace(p), ""))
	}
	return strings.Join(parts, "/")
}

// findByName lists the documents a search could return whose path ends with
// rel, compared by refKey. The walk skips exactly what Search skips, so a name
// cannot reach a note the vault-wide search would not have shown: dotted
// folders, MCP_EXCLUDE, and symlinks out of the vault or into an exclusion.
// Hidden files are left out of the count too, or an ambiguity error would
// reveal that they exist.
func (v *Vault) findByName(rel string) ([]string, error) {
	want := refKey(rel)
	var matches []string
	scanned := 0
	err := filepath.WalkDir(v.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unlike Search, a skipped subtree is not harmless here: it could
			// hold the second file that makes the name ambiguous.
			return errNoLookup
		}
		if d.IsDir() {
			if p != v.root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			if v.Excluded(p) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || !isDocument(d.Name()) || v.Excluded(p) {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(p)
			if err != nil || !within(v.root, real) || v.Excluded(real) {
				return nil
			}
		}
		if scanned++; scanned > maxSearchFiles {
			return errNoLookup
		}
		r := v.Rel(p)
		if k := refKey(r); k == want || strings.HasSuffix(k, "/"+want) {
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
