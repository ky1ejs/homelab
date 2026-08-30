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

	// Vault-relative paths this server treats as absent: not searched, not
	// listed, not readable, not writable. Normalised — lowercased, slash-form,
	// unrooted — for matching. excludesRaw holds the operator's spelling at the
	// same indexes, for log lines and MissingExcludes.
	//
	// This is NOT part of the deny list writable() mirrors, and it deliberately
	// does not apply to vault-claude. See README.md#what-voice-cannot-see.
	excludes    []string
	excludesRaw []string

	// Serialises read-modify-write sequences (append, edit) against each other.
	// Voice traffic is one request at a time in practice, but two concurrent
	// appends without this would silently drop one of them.
	mu sync.Mutex

	// Identity written into the agent stamp on every note this server changes.
	// Empty disables stamping entirely. See stamp.go and the shared contract in
	// the root README.md.
	stampAgent string

	// Injectable so tests can assert the exact timestamp a write records.
	now func() time.Time
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
	ErrExcluded   = errors.New("path is outside what this server may see")
	ErrOutside    = errors.New("path escapes the vault")
	ErrNotUnique  = errors.New("anchor text is not unique")
	ErrNoAnchor   = errors.New("anchor text not found")
	ErrWouldEmpty = errors.New("edit would empty the note")
	ErrConflict   = errors.New("note changed underneath us")
	ErrSameNote   = errors.New("source and destination are the same note")
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

func NewVault(root string, excludes []string) (*Vault, error) {
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
	v := &Vault{root: real, now: time.Now}
	for _, raw := range excludes {
		norm, err := normalizeExclude(raw)
		if err != nil {
			return nil, err
		}
		if norm == "" {
			continue
		}
		v.excludes = append(v.excludes, norm)
		v.excludesRaw = append(v.excludesRaw, strings.Trim(strings.TrimSpace(raw), "/"))
	}
	return v, nil
}

// normalizeExclude turns one operator-supplied entry into the form matchExclude
// compares against, and rejects the spellings that would silently match nothing.
//
// Lowercased because the vault lives on a case-sensitive filesystem on the NAS
// and a case-insensitive one on the Mac. A case-sensitive rule would be
// bypassable by asking for "4. inbox/..." on whichever of the two folds case,
// and the whole point of an exclusion is that there is no way around it.
func normalizeExclude(raw string) (string, error) {
	e := strings.TrimSpace(raw)
	if e == "" {
		return "", nil
	}
	if filepath.IsAbs(e) || strings.HasPrefix(e, "/") {
		return "", fmt.Errorf("MCP_EXCLUDE %q: must be relative to the vault root", raw)
	}
	e = strings.Trim(filepath.ToSlash(filepath.Clean(filepath.FromSlash(e))), "/")
	if e == "." || e == ".." || strings.HasPrefix(e, "../") {
		return "", fmt.Errorf("MCP_EXCLUDE %q: must name a folder or note inside the vault", raw)
	}
	return strings.ToLower(e), nil
}

// matchExclude reports which exclusion covers a vault-relative path, if any.
//
// Three spellings all work, because all three are natural to write: a folder
// ("4. Inbox" covers everything beneath it), a note with its extension
// ("Private/Diary.md"), and a bare note title ("Private/Diary"). The "/" in the
// prefix test is what keeps "4. Inbox" from also swallowing "4. Inbox Archive".
func (v *Vault) matchExclude(rel string) (string, bool) {
	if len(v.excludes) == 0 {
		return "", false
	}
	r := normalizeRel(rel)
	if r == "" {
		return "", false
	}
	for _, e := range v.excludes {
		if covers(e, r) {
			return e, true
		}
	}
	return "", false
}

func normalizeRel(rel string) string {
	r := strings.ToLower(strings.Trim(filepath.ToSlash(rel), "/"))
	if r == "." {
		return ""
	}
	return r
}

// covers is the single definition of "this exclusion applies to this path".
// matchExclude stops at the first hit because it only needs a yes/no;
// MissingExcludes must test every entry, so the predicate lives out here rather
// than being written twice with a chance of the two drifting.
func covers(exclude, rel string) bool {
	return rel == exclude || rel == exclude+".md" || strings.HasPrefix(rel, exclude+"/")
}

// Excluded takes an absolute path. A path it cannot place relative to the root
// is treated as excluded: this is the fail-closed direction, and resolve()
// has already rejected anything genuinely outside the vault by the time it is
// asked.
func (v *Vault) Excluded(abs string) bool {
	if len(v.excludes) == 0 {
		return false
	}
	rel, err := filepath.Rel(v.root, abs)
	if err != nil {
		return true
	}
	_, hit := v.matchExclude(rel)
	return hit
}

// SetStampAgent names the identity this server writes into the agent stamp.
// An empty name turns stamping off, which only MCP_STAMP=0 produces: loadConfig
// substitutes the default for a blank name and refuses to start on a malformed
// one, so a dropped .env line cannot quietly mean "no stamp".
func (v *Vault) SetStampAgent(name string) { v.stampAgent = name }

// applyStamp is the single place a write acquires its stamp. Every caller does
// it on the bytes it is about to hand to atomicWrite, never as a second write:
// a stamp applied afterwards would be a separate rename that `ob sync` could
// observe on its own, and would sit outside the verifyUnchanged guard.
func (v *Vault) applyStamp(body []byte, created bool) []byte {
	return stamp(body, v.stampAgent, created, v.now())
}

// HasExcludes reports whether any exclusion is configured, so the server can
// tell the model that part of the vault is deliberately invisible rather than
// letting it conclude the note was never written.
func (v *Vault) HasExcludes() bool { return len(v.excludes) > 0 }

// Excludes returns the operator's spelling of each exclusion, for logging.
func (v *Vault) Excludes() []string { return v.excludesRaw }

// MissingExcludes returns the exclusions that currently match nothing in the
// vault. A typo, or a folder renamed in Obsidian months later, protects nothing
// while every other check still passes — the silent-failure shape this repo
// keeps a separate invariant list for. Startup warns; it is not fatal, because
// naming a folder before creating it is legitimate.
func (v *Vault) MissingExcludes() []string {
	if len(v.excludes) == 0 {
		return nil
	}
	hit := make(map[string]bool, len(v.excludes))
	_ = filepath.WalkDir(v.root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil || path == v.root {
			return nil
		}
		// Every entry, not just the first match: "4. Inbox" and "4. Inbox/Sub"
		// are both satisfied by the same file, and stopping at the first would
		// report the redundant one as protecting nothing.
		rel := normalizeRel(v.Rel(path))
		for _, e := range v.excludes {
			if covers(e, rel) {
				hit[e] = true
			}
		}
		if len(hit) == len(v.excludes) {
			return fs.SkipAll
		}
		return nil
	})
	var missing []string
	for i, e := range v.excludes {
		if !hit[e] {
			missing = append(missing, v.excludesRaw[i])
		}
	}
	return missing
}

// CheckWritable validates a configured note reference at startup. A CAPTURE_NOTE
// pointing into an excluded folder — or at CLAUDE.md, or at a .txt — otherwise
// fails on the first thing you try to capture, which will be from the car.
func (v *Vault) CheckWritable(ref string) error {
	abs, err := v.resolve(ref)
	if err != nil {
		return err
	}
	return v.writable(abs)
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
	var resolved string
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if !within(v.root, real) {
				return "", ErrOutside
			}
			// probe is the deepest ancestor that exists; the rest of abs has
			// not been created yet and cannot itself be a link.
			resolved = filepath.Join(real, strings.TrimPrefix(abs, probe))
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
	// Both spellings, because a symlink that stays inside the vault passes every
	// check above while landing somewhere the operator excluded: "Public/link"
	// pointing at "4. Inbox" would otherwise read straight through the exclusion.
	if v.Excluded(abs) || v.Excluded(resolved) {
		return "", ErrExcluded
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
//
// The exclusion list is the one thing that is deliberately NOT mirrored from
// that file, and lives in matchExclude rather than here so the distinction is
// hard to lose. See README.md#what-voice-cannot-see.
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

	// Best effort: the rename landed, and an unsyncable directory is not worth
	// failing a completed write over.
	syncDir(dir)
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
			if v.Excluded(path) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		if v.Excluded(path) {
			return nil
		}
		// WalkDir reports a symlink, never its target, and does not descend one.
		// So the only way a link smuggles excluded content into results is as a
		// note-shaped leaf, which is worth the extra stat that costs nothing on
		// the vaults that contain no links at all.
		if d.Type()&fs.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(path)
			if err != nil || !within(v.root, real) || v.Excluded(real) {
				return nil
			}
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
		// Masked so the stamp this server wrote is not itself searchable: every
		// note it touches would otherwise match "agent". Offsets survive the
		// masking, which preserves length.
		if idx := strings.Index(maskStamp(strings.ToLower(string(body))), q); idx >= 0 {
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

// firstLine is the snippet shown for a title match: the note's first line of
// actual prose, skipping its frontmatter and its heading.
//
// The frontmatter skip is what keeps a spoken search result from opening with
// "agent-created, twenty twenty-six dash oh eight..." — every note an agent
// touches has a block now, so without this the stamp would be the first thing
// voice reads out about most of the vault.
func firstLine(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(body), "\n")
	for _, line := range lines[bodyStart(lines):] {
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
	if err := atomicWrite(abs, v.applyStamp([]byte(content), true)); err != nil {
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
	if err := atomicWrite(abs, v.applyStamp([]byte(b.String()), !existed)); err != nil {
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
	if err := atomicWrite(abs, v.applyStamp([]byte(updated), false)); err != nil {
		return "", err
	}
	return v.Rel(abs), nil
}

// Move renames a note, or files it into another folder — the same operation,
// since a note's folder is part of its path.
//
// This is the one write that exists for the AGENT rather than for voice.
// Claude Code has no move tool: Read plus Write to the new path leaves the
// original behind, and nothing in the vault surfaces may delete it, so triage
// that ends in "file this under Projects/" was unexpressible from the phone.
// See obsidian-vault/DECISIONS.md#giving-the-agent-a-move.
//
// It is a rename(2), not a copy-and-delete. A copy would double the note on
// disk for as long as the delete took to land, and `ob sync` reads this
// directory continuously — it would propagate the duplicate to every device and
// then a deletion, which is the shape of a sync conflict rather than a move.
//
// ORDER: the stamp is written into the source, and only then is the source
// renamed. Stamping the destination after the rename would be a second write
// `ob sync` could observe on its own, outside the verifyUnchanged guard — the
// invariant vault-mcp/README.md records under "the stamp is applied to the
// bytes atomicWrite receives". The cost of this order is that a failure between
// the two steps leaves the note stamped where it started, which is a no-op a
// retry fixes. The other order can lose the note.
//
// Only notes: resolve() appends ".md" and writable() requires it, so an
// attachment cannot be moved through here any more than it can be written.
func (v *Vault) Move(from, to string) (string, string, error) {
	fromAbs, err := v.resolve(from)
	if err != nil {
		return "", "", err
	}
	toAbs, err := v.resolve(to)
	if err != nil {
		return "", "", err
	}
	// BOTH ends, deliberately. Denying only the destination would let a move
	// carry AGENTS.md or CLAUDE.md out of the way, which changes the standing
	// instructions every future session inherits just as surely as editing one
	// does — the deny list is about those files, not about one direction of
	// travel. See obsidian-vault/vault-claude-settings.json.
	if err := v.writable(fromAbs); err != nil {
		return "", "", err
	}
	if err := v.writable(toAbs); err != nil {
		return "", "", err
	}
	if fromAbs == toAbs {
		return "", "", ErrSameNote
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	body, err := os.ReadFile(fromAbs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	// Checked before the stamp is written, so a move onto an occupied name
	// leaves the source completely untouched rather than stamped-but-not-moved.
	if _, err := os.Stat(toAbs); err == nil {
		return "", "", ErrExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", "", err
	}

	if err := verifyUnchanged(fromAbs, true, body); err != nil {
		return "", "", err
	}
	if err := atomicWrite(fromAbs, v.applyStamp(body, false)); err != nil {
		return "", "", err
	}

	// Filing into a folder that does not exist yet is a normal thing to ask
	// for — "put this under Projects/Homelab" when there is no Homelab folder.
	if err := os.MkdirAll(filepath.Dir(toAbs), 0o755); err != nil {
		return "", "", err
	}
	// Re-checked immediately before the rename, for the same reason every other
	// write re-checks: `ob sync` may have landed a note of this name since the
	// Stat above. rename(2) would overwrite it silently, and this server does
	// not overwrite notes.
	if err := verifyUnchanged(toAbs, false, nil); err != nil {
		return "", "", ErrExists
	}
	if err := os.Rename(fromAbs, toAbs); err != nil {
		return "", "", err
	}
	// Both directories: the rename changed an entry in each, and only fsyncing
	// the destination would leave a crash able to resurrect the note at its old
	// path as well as its new one.
	syncDir(filepath.Dir(fromAbs))
	syncDir(filepath.Dir(toAbs))

	// The now-empty source folder is left in place. Removing it would be a
	// directory delete inferred from a note move, and Obsidian keeps empty
	// folders on purpose — they are where you are about to put something.
	return v.Rel(fromAbs), v.Rel(toAbs), nil
}

// syncDir makes a rename durable. Best effort, like the one in atomicWrite: the
// rename has already landed in the page cache and failing the call here would
// turn a completed move into an error the caller could only retry pointlessly.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
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
	if v.Excluded(dir) {
		return nil, ErrExcluded
	}
	// os.ReadDir follows symlinks, so unlike resolve() this needs its own check:
	// without it a link in the vault lists an excluded folder — or a directory
	// outside the vault entirely.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		if !within(v.root, real) {
			return nil, ErrOutside
		}
		if v.Excluded(real) {
			return nil, ErrExcluded
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
		if v.Excluded(filepath.Join(dir, e.Name())) {
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
