package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Deleting a file, which here means moving it into the vault's .trash folder.
//
// Until 2026-11-02 this server could not remove anything, and that was the
// design: no delete tool, no whole-file overwrite, no edit that empties a note.
// What changed the answer is the weekly grooming routine. It finds duplicates
// and dead notes, the user approves a list, and the approved deletions were the
// one item nothing here could carry out — the routine could only offer to move
// them to an archive folder for a person to delete by hand. See
// obsidian-vault/DECISIONS.md#deleting-moves-to-trash.
//
// So deletion exists, in the narrowest shape that still deletes:
//
//   - It is a rename(2) into <vault>/.trash, never an unlink. The bytes stay on
//     the NAS, under their original path, and the snapshot repo still holds the
//     last committed version (it ignores .trash, so the commit records a
//     deletion). Obsidian's own "move to Obsidian trash" setting uses the same
//     folder, which is why it is this one and not a new one.
//   - It refuses a file other notes link to, and names them. A delete is the
//     one operation here that can only break links, never repair them: there is
//     nowhere for a link to point afterwards. allow_broken_links overrides it,
//     after the caller has asked the user.
//   - It reaches exactly what move_file reaches: notes, bases and the
//     attachment allow list, through writablePath, so AGENTS.md, CLAUDE.md,
//     .claude/, .mcp.json and every dotted folder stay out of reach, and
//     MCP_EXCLUDE folders stay invisible.
//   - It is served on the HTTP surface only. vault-claude's tool policy names
//     the MCP tools it may call, and the research surface's scratch sweeper is
//     the only thing that removes files there.
//   - Folders are never removed, as with a move: an emptied folder stays.

// trashDir is Obsidian's own trash folder, at the vault root.
const trashDir = ".trash"

// maxTrashSuffix bounds the search for a free name when .trash already holds
// one. Past it the delete fails rather than looping: a thousand trashed copies
// of one note is a vault someone should look at.
const maxTrashSuffix = 1000

// ErrTrashUnsafe is a .trash folder, or a folder under it, that is not a plain
// directory — a symlink above all. Following one would move a note to wherever
// it points, outside the vault included, so nothing is moved.
var ErrTrashUnsafe = errors.New("the vault's .trash folder is not a plain folder")

// BacklinkError is a delete refused because other notes link to the file.
type BacklinkError struct {
	Path    string   // the file that was not deleted
	Linkers []string // the notes that link to it, sorted
}

func (e *BacklinkError) Error() string {
	return fmt.Sprintf("%d %s link to %s", len(e.Linkers), plural(len(e.Linkers), "note", "notes"), e.Path)
}

// LinkCheckError is a delete refused because links to the file could not be
// checked. Deleting anyway is the same decision as deleting a linked file, so
// it takes the same override.
type LinkCheckError struct{ Reason string }

func (e *LinkCheckError) Error() string {
	return "links to it could not be checked: " + e.Reason
}

// TrashResult is what a delete did.
type TrashResult struct {
	Path  string // where the file was
	Trash string // where it is now, under .trash

	// Notes that still link to it. Set only when the caller allowed it.
	BrokenLinks []string
	// Set when links were not checked and the caller allowed the delete anyway.
	LinksUnchecked string
	// True when MCP_EXCLUDE is set: notes in those folders were not read, so
	// one of them may link to the deleted file.
	ExcludedUnchecked bool
}

// Trash moves one note, base or attachment into .trash.
//
// The link check runs inside the lock and before anything is written, so a
// refused delete is a complete no-op. Unlike an edit, the file is not compared
// against an earlier read before it moves: rename(2) carries whatever bytes are
// there when the kernel runs it, so an edit `ob sync` lands meanwhile goes into
// .trash with the file rather than being lost.
func (v *Vault) Trash(ctx context.Context, ref string, allowBrokenLinks bool) (*TrashResult, error) {
	abs, err := v.resolveRef(ref, true)
	if err != nil {
		return nil, err
	}
	if err := v.writablePath(abs, true); err != nil {
		return nil, err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	// Lstat, as in Move: a symlink is not a file to be deleted, and renaming
	// one would trash the link while the user believes the file is gone.
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotFound
	}

	rel := v.Rel(abs)
	res := &TrashResult{Path: rel, ExcludedUnchecked: v.HasExcludes()}

	linkers, err := v.backlinks(ctx, abs)
	if ctx.Err() != nil {
		return nil, ctx.Err() // nothing has been written yet
	}
	switch {
	case err != nil && !allowBrokenLinks:
		return nil, &LinkCheckError{Reason: err.Error()}
	case err != nil:
		res.LinksUnchecked = err.Error()
	case len(linkers) > 0 && !allowBrokenLinks:
		return nil, &BacklinkError{Path: rel, Linkers: linkers}
	default:
		res.BrokenLinks = linkers
	}

	dest, err := v.trashPath(rel)
	if err != nil {
		return nil, err
	}
	hook("trash-planned")
	// Re-checked immediately before the rename, as every write here is:
	// rename(2) replaces whatever is at dest without a word.
	if err := verifyAbsent(dest); err != nil {
		return nil, err
	}
	if err := os.Rename(abs, dest); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound // deleted in Obsidian while we looked
		}
		return nil, err
	}
	syncDir(filepath.Dir(abs))
	syncDir(filepath.Dir(dest))

	res.Trash = v.Rel(dest)
	return res, nil
}

// trashPath picks where a file goes in .trash: its own vault path under the
// trash folder, so the way back is the path it came from, with " 1", " 2" and
// so on before the extension when that name is taken by an earlier delete.
//
// Every folder on the way is created one at a time and checked with Lstat. A
// MkdirAll would follow a symlink planted at .trash, or anywhere below it, and
// create folders outside the vault before any check could run.
func (v *Vault) trashPath(rel string) (string, error) {
	dir := filepath.Join(v.root, trashDir)
	parts := []string{""}
	if d := path.Dir(rel); d != "." {
		parts = append(parts, strings.Split(d, "/")...)
	}
	for _, p := range parts {
		if p != "" {
			dir = filepath.Join(dir, p)
		}
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() { // a symlink's Lstat mode is not a directory
			return "", ErrTrashUnsafe
		}
	}

	name := path.Base(rel)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < maxTrashSuffix; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s %d%s", stem, i, ext)
		}
		p := filepath.Join(dir, cand)
		_, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf(".trash already holds %d files named %s", maxTrashSuffix, name)
}

// backlinks lists the notes this server may see that link to target, resolved
// the way a move resolves them (see links.go). The target's own links to
// itself do not count: they go with it.
//
// Like a move, notes in MCP_EXCLUDE folders are never opened. Unlike a move, a
// note that cannot be read fails the whole check rather than being reported,
// because here the answer gates the write: a note nobody read may be the one
// holding the link.
//
// Canvases and bases are not read. A move does not rewrite them either, and
// the tool result says so.
func (v *Vault) backlinks(ctx context.Context, targetAbs string) ([]string, error) {
	ix, err := v.buildLinkIndex()
	if err != nil {
		return nil, err
	}
	target := v.Rel(targetAbs)
	needles := linkNeedles(target, target)
	var linkers []string
	scanned := 0
	err = filepath.WalkDir(v.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return errIndexPartial
		}
		if d.IsDir() {
			if p != v.root && (strings.HasPrefix(d.Name(), ".") || v.Excluded(p)) {
				return fs.SkipDir
			}
			return nil
		}
		// Symlinks are skipped, as in planRelinks: reading through one would
		// let a link inside the vault expose whatever it points at.
		if !d.Type().IsRegular() || !isNote(d.Name()) || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if p == targetAbs || v.Excluded(p) {
			return nil
		}
		if scanned++; scanned > maxLinkIndexFiles {
			return errIndexTooLarge
		}
		if scanned%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return errIndexPartial
		}
		if !mayHoldLink(body, needles) {
			return nil
		}
		rel := v.Rel(p)
		for _, l := range findLinks(body) {
			if m, ok := ix.resolve(l.target, rel); ok && strings.EqualFold(m.rel, target) {
				linkers = append(linkers, rel)
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(linkers)
	return linkers, nil
}
