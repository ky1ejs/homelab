package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Vault is the only thing in this program that touches /vault.
//
// Two rules it exists to enforce, both of which fail silently if you inline
// file operations elsewhere instead:
//
//  1. Every write is temp-then-rename on the same filesystem. `ob sync
//     --continuous` is reading this directory concurrently and a partial write
//     propagates to every device. See obsidian-vault/ARCHITECTURE.md#known-unresolved-risk.
//  2. Every path is resolved and checked against the deny list before it is
//     opened. This server is reachable from the public internet; the vault
//     agent's tool policy in vault-claude-settings.json does not apply to it,
//     so the equivalent denials are reimplemented here.
type Vault struct {
	root string

	// Serialises read-modify-write sequences (append, edit) against each other.
	// Voice traffic is one request at a time in practice, but two concurrent
	// appends without this would silently drop one of them.
	mu sync.Mutex
}

const (
	// Caps exist because results are spoken aloud. A tool that returns 4 KB of
	// markdown produces a voice response nobody wants to sit through, and burns
	// context for no benefit. See README.md#designing-tools-for-voice.
	maxReadBytes   = 8 * 1024
	maxSearchFiles = 20000
	snippetLen     = 160
)

var (
	ErrNotFound   = errors.New("note not found")
	ErrExists     = errors.New("note already exists")
	ErrDenied     = errors.New("path is not writable through this server")
	ErrOutside    = errors.New("path escapes the vault")
	ErrNotUnique  = errors.New("anchor text is not unique")
	ErrNoAnchor   = errors.New("anchor text not found")
	ErrWouldEmpty = errors.New("edit would empty the note")
	ErrConflict   = errors.New("note changed underneath us")
)

// verifyUnchanged is the concurrent-writer check.
//
// The mutex in Vault serialises this server against itself, but the vault has a
// second writer that knows nothing about it: `ob sync --continuous` landing an
// edit made on the Mac or the phone. Atomic writes stop a reader seeing a torn
// file; they do nothing about read-modify-write losing whoever wrote last.
//
// So every read-modify-write re-reads the file immediately before renaming and
// compares it byte-for-byte against what it read. A collision fails loudly and
// retryably instead of silently discarding an edit made in Obsidian seconds
// earlier. Exact rather than mtime-based: filesystem timestamp granularity can
// be a full second, which is comfortably long enough to hide a lost edit.
//
// A window remains between this check and the rename. It cannot be closed
// without locking the vault against Obsidian itself, which is
// obsidian-vault/ARCHITECTURE.md#known-unresolved-risk escalation step 2 and a
// much larger change than this.
func verifyUnchanged(path string, existed bool, seen []byte) error {
	current, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if !existed {
				return nil // still absent, as it was when we looked
			}
			return ErrConflict // deleted underneath us
		}
		return err
	}
	if !existed {
		return ErrConflict // created underneath us
	}
	if !bytes.Equal(current, seen) {
		return ErrConflict
	}
	return nil
}

// Extensions that mean "the caller meant an actual file, not a note title".
// Not a security control — writable() already requires the resolved path to end
// in .md — purely so a mistaken reference fails loudly instead of creating a
// note with a confusing name.
var nonMarkdownExts = map[string]bool{
	".txt": true, ".json": true, ".yaml": true, ".yml": true, ".toml": true,
	".ini": true, ".conf": true, ".xml": true, ".csv": true, ".html": true,
	".css": true, ".js": true, ".ts": true, ".py": true, ".sh": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".svg": true, ".pdf": true, ".mp3": true, ".mp4": true, ".mov": true,
	".wav": true, ".zip": true, ".exe": true,
}

func NewVault(root string) (*Vault, error) {
	// Resolve once at startup: if the vault root is itself a symlink, every
	// later containment check would compare against the wrong prefix.
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("vault root %s: %w", abs, err)
	}
	return &Vault{root: real}, nil
}

// resolve turns a user-supplied note reference into an absolute path inside the
// vault, or fails. Callers must never build paths any other way.
//
// A bare name is treated as a note title, so "Reading list" and "Reading
// list.md" are the same note — voice input never includes the extension.
func (v *Vault) resolve(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(ref) {
		return "", ErrOutside
	}
	if !strings.HasSuffix(strings.ToLower(ref), ".md") {
		// Only bare titles get ".md" appended. A reference that already names a
		// real non-markdown file is refused rather than quietly turned into
		// "notes.txt.md" — a note the user will never find again.
		//
		// An explicit list rather than "anything that looks like an extension",
		// because note titles contain dots: "Chapter 1.2" must stay a title.
		if nonMarkdownExts[strings.ToLower(filepath.Ext(ref))] {
			return "", ErrDenied
		}
		ref += ".md"
	}

	clean := filepath.Clean(filepath.FromSlash(ref))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrOutside
	}

	abs := filepath.Join(v.root, clean)

	// Join+Clean already collapses traversal, but a symlink inside the vault can
	// still point out of it. Check the deepest ancestor that actually exists —
	// the leaf may legitimately not exist yet on a create.
	probe := abs
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if !within(v.root, real) {
				return "", ErrOutside
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", ErrOutside
		}
		probe = parent
	}

	if !within(v.root, abs) {
		return "", ErrOutside
	}
	return abs, nil
}

func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// writable mirrors the deny list in obsidian-vault/vault-claude-settings.json.
//
// If it drifts from that file, this server becomes the way around the agent's
// tool policy — the one failure mode that turns a convenience feature into a
// persistent compromise. AGENTS.md and CLAUDE.md are standing instructions to
// every future session; .claude/ holds the policy itself.
func (v *Vault) writable(abs string) error {
	rel, err := filepath.Rel(v.root, abs)
	if err != nil {
		return ErrOutside
	}
	if !strings.HasSuffix(strings.ToLower(rel), ".md") {
		return ErrDenied
	}

	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, p := range parts {
		// Any dotted component: .claude, .obsidian, .trash. Writes to Obsidian's
		// own config from an internet-reachable endpoint are not a feature.
		if strings.HasPrefix(p, ".") {
			return ErrDenied
		}
		if i == len(parts)-1 {
			switch p {
			case "AGENTS.md", "CLAUDE.md":
				return ErrDenied
			}
		}
	}
	return nil
}

// readable is deliberately weaker than writable: the agent may read its own
// instructions, and so may this. Only .claude/ is hidden, because that is where
// the tool policy lives and it is not something a voice session needs.
func (v *Vault) readable(abs string) error {
	rel, err := filepath.Rel(v.root, abs)
	if err != nil {
		return ErrOutside
	}
	for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
		if p == ".claude" {
			return ErrDenied
		}
	}
	return nil
}

// Rel is the vault-relative path, for log lines and git pathspecs.
func (v *Vault) Rel(abs string) string {
	rel, err := filepath.Rel(v.root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// atomicWrite is the whole reason this server exists in Go rather than being a
// shell script. Write to a temp file on the same filesystem, fsync it, rename
// over the target, then fsync the directory so the rename itself is durable.
//
// The rename is what makes it safe: a concurrent reader — Obsidian's sync
// client — sees either the old file or the new one, never a half-written one.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Same directory, so the rename cannot cross a filesystem boundary. A dotted
	// prefix keeps the temp file out of Obsidian's indexer if we die mid-write.
	tmp, err := os.CreateTemp(dir, ".vault-mcp-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes 0600; notes must stay readable by the Mac over SMB.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}

	d, err := os.Open(dir)
	if err != nil {
		return nil // the rename landed; an unsyncable dir is not worth failing on
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

type SearchHit struct {
	Path    string
	Snippet string
}

// Search matches the query against note titles and body text, title matches
// first. Deliberately a plain substring scan: the vault's markdown is a few MB,
// and an index would be another thing to keep correct and in sync.
func (v *Vault) Search(query string, limit int) ([]SearchHit, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, errors.New("empty query")
	}
	if limit <= 0 {
		limit = 5
	}

	var titleHits, bodyHits []SearchHit
	scanned := 0

	err := filepath.WalkDir(v.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree must not fail the whole search
		}
		if d.IsDir() {
			if path != v.root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		if scanned++; scanned > maxSearchFiles {
			return fs.SkipAll
		}

		rel := v.Rel(path)
		title := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))

		if strings.Contains(strings.ToLower(title), q) {
			titleHits = append(titleHits, SearchHit{Path: rel, Snippet: firstLine(path)})
			return nil
		}
		if len(titleHits)+len(bodyHits) >= limit*4 {
			return nil // enough candidates; stop reading file bodies
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if idx := strings.Index(strings.ToLower(string(body)), q); idx >= 0 {
			bodyHits = append(bodyHits, SearchHit{Path: rel, Snippet: snippetAt(string(body), idx)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	hits := append(titleHits, bodyHits...)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func firstLine(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "#") {
			return truncate(line, snippetLen)
		}
	}
	return ""
}

// snippetAt takes an index found in a lowercased copy of body. strings.ToLower
// can change byte length for some non-ASCII runes, so the offset is clamped
// rather than trusted — a vault full of prose will find that edge eventually.
func snippetAt(body string, idx int) string {
	if idx > len(body) {
		idx = len(body)
	}
	start := idx - 40
	if start < 0 {
		start = 0
	}
	end := start + snippetLen
	if end > len(body) {
		end = len(body)
	}
	return strings.TrimSpace(strings.ReplaceAll(body[start:end], "\n", " "))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Read returns the note body, truncated. Truncation is reported in the returned
// string rather than silently, so the model can say so instead of confidently
// summarising half a note.
func (v *Vault) Read(ref string) (string, error) {
	abs, err := v.resolve(ref)
	if err != nil {
		return "", err
	}
	if err := v.readable(abs); err != nil {
		return "", err
	}
	body, err := os.ReadFile(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if len(body) > maxReadBytes {
		return string(body[:maxReadBytes]) +
			fmt.Sprintf("\n\n[truncated — note is %d bytes, showing the first %d]", len(body), maxReadBytes), nil
	}
	return string(body), nil
}

// Create writes a new note and refuses to overwrite an existing one. Overwrite
// is the deletion path in disguise, which is exactly what this server does not do.
func (v *Vault) Create(ref, content string) (string, error) {
	abs, err := v.resolve(ref)
	if err != nil {
		return "", err
	}
	if err := v.writable(abs); err != nil {
		return "", err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if _, err := os.Stat(abs); err == nil {
		return "", ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	// Re-check immediately before the rename: sync may have landed a note of
	// this name in the meantime, and create must never clobber one.
	if err := verifyUnchanged(abs, false, nil); err != nil {
		return "", ErrExists
	}
	if err := atomicWrite(abs, []byte(content)); err != nil {
		return "", err
	}
	return v.Rel(abs), nil
}

// Append adds text to the end of a note, creating it if absent. This is the
// capture path, and the reason append is a first-class operation rather than
// read-then-write: appending cannot destroy existing content, so it is the one
// write worth allowing with the least hesitation.
func (v *Vault) Append(ref, text string) (string, error) {
	abs, err := v.resolve(ref)
	if err != nil {
		return "", err
	}
	if err := v.writable(abs); err != nil {
		return "", err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	existing, err := os.ReadFile(abs)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimRight(text, "\n"))
	b.WriteString("\n")

	if err := verifyUnchanged(abs, existed, existing); err != nil {
		return "", err
	}
	if err := atomicWrite(abs, []byte(b.String())); err != nil {
		return "", err
	}
	return v.Rel(abs), nil
}

// Capture appends a timestamped line to the capture note. One parameter, no
// path to disambiguate over voice, and it works when the note does not exist yet.
func (v *Vault) Capture(captureNote, text string, now time.Time) (string, error) {
	line := fmt.Sprintf("- %s — %s", now.Format("2006-01-02 15:04"), strings.TrimSpace(text))
	return v.Append(captureNote, line)
}

// Edit replaces an anchored substring. It requires the anchor to appear exactly
// once, which is what stops "edit" from being "overwrite in disguise" — an
// edit tool taking whole-file content would be a delete with extra steps.
//
// The uniqueness requirement also catches the model editing a note it misread:
// a wrong anchor fails loudly instead of changing the wrong paragraph.
//
// Anchoring alone is not quite enough, though. Read a short note, pass its
// entire body as the anchor and an empty replacement, and you have emptied it
// without ever calling a delete. The blank-result check below closes that.
func (v *Vault) Edit(ref, oldText, newText string) (string, error) {
	if oldText == "" {
		return "", errors.New("anchor text must not be empty")
	}
	abs, err := v.resolve(ref)
	if err != nil {
		return "", err
	}
	if err := v.writable(abs); err != nil {
		return "", err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	body, err := os.ReadFile(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}

	switch strings.Count(string(body), oldText) {
	case 0:
		return "", ErrNoAnchor
	case 1:
	default:
		return "", ErrNotUnique
	}

	updated := strings.Replace(string(body), oldText, newText, 1)
	if strings.TrimSpace(updated) == "" && strings.TrimSpace(string(body)) != "" {
		return "", ErrWouldEmpty
	}
	if err := verifyUnchanged(abs, true, body); err != nil {
		return "", err
	}
	if err := atomicWrite(abs, []byte(updated)); err != nil {
		return "", err
	}
	return v.Rel(abs), nil
}

// List returns note titles in a folder, for navigation by voice.
func (v *Vault) List(folder string, limit int) ([]string, error) {
	dir := v.root
	if f := strings.TrimSpace(folder); f != "" && f != "." && f != "/" {
		clean := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(f, "/")))
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, ErrOutside
		}
		dir = filepath.Join(v.root, clean)
		if !within(v.root, dir) {
			return nil, ErrOutside
		}
	}
	if limit <= 0 {
		limit = 50
	}

	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() {
			out = append(out, e.Name()+"/")
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			out = append(out, strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
