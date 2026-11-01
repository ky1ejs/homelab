package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Keeping links pointing at a file when Move moves it.
//
// Obsidian rewrites every link to a note when you move or rename it in the
// app. A rename(2) on the NAS is invisible to that machinery: Obsidian never
// sees a move, only a file vanishing and another appearing, so until this
// existed a move through move_file left every [[Old Title]] in the vault
// pointing at nothing. See obsidian-vault/DECISIONS.md#links-follow-a-move
// for why this is done here rather than left to the caller.
//
// The approach is the one that keeps diffs minimal and therefore reviewable in
// the snapshot repo: resolve every link in every note against the vault as it
// was BEFORE the move, and again against the vault as it will be AFTER. A link
// that resolves to the same file both times is left byte-for-byte alone — which
// is every [[Title]] in a shortest-path vault when a note only changes folder.
// Only a link that would stop resolving, or resolve to a different file, is
// rewritten, and only its path: the alias, the #heading or #^block, the ! of an
// embed and a markdown link's title all survive untouched.
//
// Resolution approximates Obsidian's own, in this order:
//
//  1. the path from the vault root ("Projects/Pike", "/Projects/Pike")
//  2. the path relative to the linking note ("../Pike", "./Pike")
//  3. any file whose path ends with the link ("Pike", "Fishing/Pike"),
//     preferring one in the linking note's own folder, then the shortest path
//
// each tried as written and then with ".md" appended, case-insensitively.
// Rule 3's tie-break is the part least certain to match Obsidian exactly, which
// is why a rewrite never relies on it: a new link is a bare name only when that
// name is unique in the whole vault, and otherwise the full vault path.

// maxLinkIndexFiles caps the walk that builds the index. Larger than
// maxSearchFiles because it counts attachments as well as notes. Hitting it
// skips the link update rather than rewriting against a partial index, which
// could misjudge a name as unique and write a link to the wrong note. The same
// cap, and the same answer, applies to the walk that reads the notes.
const maxLinkIndexFiles = 100000

var (
	errIndexTooLarge = errors.New("the vault has more files than the link check reads")
	errIndexPartial  = errors.New("part of the vault could not be read")
)

// linkIndex is every file name in the vault, for resolving links.
//
// It includes files in MCP_EXCLUDE folders, marked hidden. Their PATHS are used
// only to decide which file a link means: leaving them out would make a name
// look unique when it is not, and a rewrite would then point at the wrong note.
// A hidden path is never written into a note — a link whose file is hidden is
// reported as unfixable instead, so the rewrite cannot reveal the folder's
// layout to a surface that may not see it. Their contents are never read here.
type linkIndex struct {
	byPath map[string]string   // lower(vault path) -> vault path
	byName map[string][]string // lower(base name)  -> vault paths
	hidden map[string]bool     // lower(vault path) of files in MCP_EXCLUDE folders
}

func newLinkIndex() *linkIndex {
	return &linkIndex{byPath: map[string]string{}, byName: map[string][]string{}, hidden: map[string]bool{}}
}

func (ix *linkIndex) add(rel string) {
	k := strings.ToLower(rel)
	if _, ok := ix.byPath[k]; ok {
		return
	}
	ix.byPath[k] = rel
	n := strings.ToLower(path.Base(rel))
	ix.byName[n] = append(ix.byName[n], rel)
}

func (ix *linkIndex) remove(rel string) {
	k := strings.ToLower(rel)
	canon, ok := ix.byPath[k]
	if !ok {
		return
	}
	delete(ix.byPath, k)
	delete(ix.hidden, k)
	n := strings.ToLower(path.Base(canon))
	list := ix.byName[n]
	for i, r := range list {
		if r == canon {
			ix.byName[n] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(ix.byName[n]) == 0 {
		delete(ix.byName, n)
	}
}

func (ix *linkIndex) clone() *linkIndex {
	c := newLinkIndex()
	for k, v := range ix.byPath {
		c.byPath[k] = v
	}
	for k, v := range ix.byName {
		c.byName[k] = append([]string(nil), v...)
	}
	for k := range ix.hidden {
		c.hidden[k] = true
	}
	return c
}

func (ix *linkIndex) isHidden(rel string) bool {
	return ix.hidden[strings.ToLower(rel)]
}

// buildLinkIndex walks the vault the way Obsidian indexes it: every file, in
// every folder that does not start with a dot. A folder it cannot read fails
// the whole index, like the cap: a name missing from it could look unique.
func (v *Vault) buildLinkIndex() (*linkIndex, error) {
	ix := newLinkIndex()
	n := 0
	err := filepath.WalkDir(v.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return errIndexPartial
		}
		if d.IsDir() {
			if p != v.root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil // atomicWrite's temp files, .DS_Store, and the like
		}
		if n++; n > maxLinkIndexFiles {
			return errIndexTooLarge
		}
		rel := v.Rel(p)
		ix.add(rel)
		if v.Excluded(p) {
			ix.hidden[strings.ToLower(rel)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ix, nil
}

// How a link names its file. Kept so a rewrite writes the same kind of link the
// user (or Obsidian's "New link format" setting) wrote in the first place.
type linkStyle int

const (
	styleRoot     linkStyle = iota // a path from the vault root
	styleRelative                  // a path from the linking note's folder
	styleName                      // a name, or the tail of a path
)

// linkMatch is the file a link target resolved to, and how.
type linkMatch struct {
	rel   string
	style linkStyle
	// asWritten reports whether the match was on the target as written (true)
	// or with ".md" appended (false), so a rewrite keeps or drops the extension
	// to match.
	asWritten bool
	// ambiguous is set when rule 3 chose between several files. Only the
	// tie-break decided which one, and it may not be the one Obsidian chose.
	ambiguous bool
}

// resolve finds the file a link target means, from a note at source.
func (ix *linkIndex) resolve(target, source string) (linkMatch, bool) {
	t := strings.TrimSpace(target)
	if t == "" {
		return linkMatch{}, false
	}
	for i, cand := range []string{t, t + ".md"} {
		if i == 1 && strings.EqualFold(path.Ext(t), ".md") {
			break
		}
		if m, ok := ix.resolveOne(cand, source); ok {
			m.asWritten = i == 0
			return m, true
		}
	}
	return linkMatch{}, false
}

func (ix *linkIndex) resolveOne(cand, source string) (linkMatch, bool) {
	rooted := strings.HasPrefix(cand, "/")
	explicit := strings.HasPrefix(cand, "./") || strings.HasPrefix(cand, "../")
	c := strings.TrimPrefix(path.Clean("/"+cand), "/")
	if c == "" {
		return linkMatch{}, false
	}
	// "../Pike" cleans to "Pike" against the root, so an explicitly relative
	// link must skip this step or it would match a note it does not name.
	if !explicit {
		if r, ok := ix.byPath[strings.ToLower(c)]; ok {
			return linkMatch{rel: r, style: styleRoot}, true
		}
	}
	if rooted {
		return linkMatch{}, false
	}
	srcDir := path.Dir(source)
	rel := path.Clean(path.Join(srcDir, cand))
	if rel != ".." && !strings.HasPrefix(rel, "../") {
		if r, ok := ix.byPath[strings.ToLower(rel)]; ok {
			return linkMatch{rel: r, style: styleRelative}, true
		}
	}
	if explicit {
		return linkMatch{}, false // explicitly relative, and it did not resolve
	}
	lc := strings.ToLower(c)
	var best string
	n := 0
	for _, r := range ix.byName[strings.ToLower(path.Base(c))] {
		lr := strings.ToLower(r)
		if lr != lc && !strings.HasSuffix(lr, "/"+lc) {
			continue
		}
		n++
		if best == "" || betterMatch(r, best, srcDir) {
			best = r
		}
	}
	if best == "" {
		return linkMatch{}, false
	}
	return linkMatch{rel: best, style: styleName, ambiguous: n > 1}, true
}

// betterMatch is rule 3's tie-break: the linking note's own folder first, then
// the shortest path, then alphabetical so the answer never depends on walk order.
func betterMatch(a, b, srcDir string) bool {
	aHere, bHere := path.Dir(a) == srcDir, path.Dir(b) == srcDir
	if aHere != bHere {
		return aHere
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// uniqueName reports whether a file is the only one in the vault with its name,
// which is the only case a rewrite will write a bare name for it.
func (ix *linkIndex) uniqueName(rel string) bool {
	return len(ix.byName[strings.ToLower(path.Base(rel))]) == 1
}

// A link found in a note: the byte range of its path, and how to write a new one.
type foundLink struct {
	start, end int    // byte offsets of the path inside the note
	target     string // the path, decoded: what resolve() is given
	markdown   bool   // a [text](path) link rather than a [[wiki link]]
	angle      bool   // a markdown destination written as <path>
	encoded    bool   // a markdown destination that was percent-encoded
	yaml       byte   // inside frontmatter: the scalar style it sits in (see yamlStyle); 0 in the body
}

// The YAML scalar a frontmatter link sits in, which decides how a new path has
// to be escaped to leave the properties block valid.
const (
	yamlPlain  byte = 'p' // unquoted, including a flow sequence
	yamlSingle byte = '\''
	yamlDouble byte = '"'
)

var (
	wikiLinkPattern = regexp.MustCompile(`\[\[([^\[\]\n]+?)\]\]`)
	// [text](dest) with one level of brackets in the text and of parentheses in
	// the destination, which covers "[a [b]](c)" and "(Pike (2026).md)".
	mdLinkPattern = regexp.MustCompile(`\[(?:[^\[\]\n]|\[[^\[\]\n]*\])*\]\(([^()\n]*(?:\([^()\n]*\)[^()\n]*)*)\)`)
	schemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// findLinks returns every wiki link and markdown link in a note outside code.
// Frontmatter is included: Obsidian reads links in properties, and updates them
// on a rename, so a "related: [[Pike]]" property is as much a link as a body one.
func findLinks(body []byte) []foundLink {
	masked := maskCode(body)
	fmEnd := frontmatterBytes(body)
	var out []foundLink

	for _, m := range wikiLinkPattern.FindAllSubmatchIndex(masked, -1) {
		inner := string(body[m[2]:m[3]])
		end := len(inner)
		// The alias starts at the first "|"; inside a table it is written "\|",
		// and the backslash belongs to the separator, not the path.
		if i := strings.IndexByte(inner, '|'); i >= 0 {
			end = i
			if i > 0 && inner[i-1] == '\\' {
				end = i - 1
			}
		}
		if i := strings.IndexByte(inner[:end], '#'); i >= 0 {
			end = i // #heading or #^block
		}
		target := inner[:end]
		if strings.TrimSpace(target) == "" {
			continue // [[#heading]] is a link to the note it is in
		}
		out = append(out, foundLink{start: m[2], end: m[2] + end, target: target, yaml: yamlStyle(body, m[2], fmEnd)})
	}

	for _, m := range mdLinkPattern.FindAllSubmatchIndex(masked, -1) {
		dest := string(body[m[2]:m[3]])
		lead := len(dest) - len(strings.TrimLeft(dest, " \t"))
		d := dest[lead:]
		var raw string
		angle := false
		if strings.HasPrefix(d, "<") {
			i := strings.IndexByte(d, '>')
			if i < 0 {
				continue
			}
			raw, angle = d[1:i], true
		} else {
			raw = d
			if i := strings.IndexAny(d, " \t"); i >= 0 {
				raw = d[:i] // the rest is a "title"
			}
		}
		pathPart := raw
		if i := strings.IndexByte(raw, '#'); i >= 0 {
			pathPart = raw[:i]
		}
		if pathPart == "" || schemePattern.MatchString(pathPart) || strings.HasPrefix(pathPart, "//") {
			continue // a #fragment on this note, or not a file in the vault
		}
		// Percent-encoding is decoded inside <...> too: it is a URL either way,
		// and Obsidian writes "<My%20Pike.md>" as readily as "My%20Pike.md".
		target := pathPart
		encoded := false
		if strings.Contains(pathPart, "%") {
			if dec, err := url.PathUnescape(pathPart); err == nil {
				target, encoded = dec, true
			}
		}
		start := m[2] + lead
		if angle {
			start++
		}
		out = append(out, foundLink{start: start, end: start + len(pathPart), target: target, markdown: true, angle: angle, encoded: encoded, yaml: yamlStyle(body, start, fmEnd)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// frontmatterBytes is the byte offset where the note's frontmatter ends, or 0
// when it has none. It uses stamp.go's frontmatterEnd, so a block counts as
// frontmatter here exactly when the stamp would treat it as one.
func frontmatterBytes(body []byte) int {
	lines := strings.Split(string(body), "\n")
	end := frontmatterEnd(lines)
	if end == 0 {
		return 0
	}
	n := 0
	for _, l := range lines[:end] {
		n += len(l) + 1
	}
	return n
}

// yamlStyle reports which kind of YAML scalar the byte at pos sits in, by
// scanning its line up to pos. A quote opens a scalar only where YAML allows
// one to start, so the apostrophe in "title: Kyle's [[Pike]]" is plain text.
// Multi-line quoted scalars are not followed; Obsidian writes each link
// property on one line.
func yamlStyle(body []byte, pos, fmEnd int) byte {
	if pos >= fmEnd {
		return 0
	}
	lineStart := bytes.LastIndexByte(body[:pos], '\n') + 1
	line := body[lineStart:pos]
	state := yamlPlain
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch state {
		case yamlPlain:
			if c == '\'' || c == '"' {
				prev := byte(' ')
				if i > 0 {
					prev = line[i-1]
				}
				if prev == ' ' || prev == '\t' || prev == '[' || prev == '{' || prev == ',' || prev == ':' || prev == '-' {
					state = c
				}
			}
		case yamlSingle:
			if c == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
				} else {
					state = yamlPlain
				}
			}
		case yamlDouble:
			if c == '\\' {
				i++
			} else if c == '"' {
				state = yamlPlain
			}
		}
	}
	return state
}

// yamlEscape makes a new link path safe to write into the scalar style it
// replaces a path in. A plain scalar has no escape, so a path holding a
// character YAML would read as structure cannot go in one at all.
func yamlEscape(p string, style byte) (string, bool) {
	switch style {
	case 0:
		return p, true
	case yamlSingle:
		return strings.ReplaceAll(p, "'", "''"), true
	case yamlDouble:
		return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p), true
	default:
		// Only what can end or restructure a scalar in mid-path matters here;
		// the path never starts the scalar, the link's own brackets do.
		if strings.ContainsAny(p, ":,#{}[]") {
			return "", false
		}
		return p, true
	}
}

// maskCode blanks fenced code blocks and inline code spans, preserving length,
// so a link written as an example inside them is never rewritten. Indented code
// blocks are not masked: telling one from a list item's continuation needs a
// full parse. A link in one is rewritten like any other, which keeps its
// meaning; the cost is an edited example.
func maskCode(body []byte) []byte {
	out := append([]byte(nil), body...)
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}

	// Fences first, line by line.
	var fence string
	fenceStart := 0
	pos := 0
	for pos < len(body) {
		end := bytes.IndexByte(body[pos:], '\n')
		lineEnd := len(body)
		if end >= 0 {
			lineEnd = pos + end
		}
		line := strings.TrimLeft(string(body[pos:lineEnd]), " ")
		indent := lineEnd - pos - len(line)
		if fence == "" {
			if indent <= 3 && (strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")) {
				ch := line[0]
				n := 0
				for n < len(line) && line[n] == ch {
					n++
				}
				// A backtick fence's info string cannot hold a backtick, so
				// "```js``` is inline" is a code span, not a fence that would
				// swallow the rest of the note.
				if ch != '`' || !strings.Contains(line[n:], "`") {
					fence, fenceStart = line[:n], pos
				}
			}
		} else if indent <= 3 && strings.HasPrefix(line, fence) && strings.Trim(line, string(fence[0])+" \t") == "" {
			blank(fenceStart, lineEnd)
			fence = ""
		}
		pos = lineEnd + 1
	}
	if fence != "" {
		blank(fenceStart, len(body)) // an unclosed fence runs to the end, as in Obsidian
	}

	// Then inline code: a run of n backticks closed by the next run of exactly n
	// in the same paragraph. A backslash-escaped backtick opens nothing.
	for i := 0; i < len(out); {
		if out[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < len(out) && out[i+n] == '`' {
			n++
		}
		if escapedAt(out, i) {
			i++ // the escaped backtick is literal; the rest of the run may open
			continue
		}
		closeAt := -1
		for j := i + n; j < len(out); {
			if out[j] == '\n' && blankLineAfter(out, j) {
				break // a code span never crosses a paragraph break
			}
			if out[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(out) && out[j+m] == '`' {
				m++
			}
			if m == n {
				closeAt = j
				break
			}
			j += m
		}
		if closeAt < 0 {
			i += n
			continue
		}
		blank(i, closeAt+n)
		i = closeAt + n
	}
	return out
}

// escapedAt reports whether the byte at i follows an odd number of backslashes.
func escapedAt(b []byte, i int) bool {
	n := 0
	for k := i - 1; k >= 0 && b[k] == '\\'; k-- {
		n++
	}
	return n%2 == 1
}

// blankLineAfter reports whether the line after the newline at i is blank.
func blankLineAfter(b []byte, i int) bool {
	for k := i + 1; k < len(b); k++ {
		switch b[k] {
		case ' ', '\t', '\r':
			continue
		case '\n':
			return true
		default:
			return false
		}
	}
	return true
}

// relinkNote rewrites the links in one note that the move would break.
//
// srcOld and srcNew are the note's own path before and after the move — the
// same unless this is the note being moved, whose relative links are then
// checked against its new folder. Returns the new body, the number of links
// rewritten, and the number that pointed at a file before and that no rewrite
// could make point at the same file after.
func relinkNote(body []byte, srcOld, srcNew, movedOld, movedNew string, before, after *linkIndex) ([]byte, int, int) {
	links := findLinks(body)
	if len(links) == 0 {
		return body, 0, 0
	}
	var buf bytes.Buffer
	last, changed, lost := 0, 0, 0
	for _, l := range links {
		if l.start < last {
			continue // overlaps a link already handled
		}
		was, ok := before.resolve(l.target, srcOld)
		if !ok {
			continue // already broken before the move: not ours to fix
		}
		want := was.rel
		if want == movedOld {
			want = movedNew
		}
		if got, ok := after.resolve(l.target, srcNew); ok && got.rel == want {
			continue // still means the same file
		}
		// Two cases where the link has to change but no rewrite is safe. If the
		// tie-break picked its file, Obsidian may have picked another, and a
		// rewrite would pin the guess. If its file is in an excluded folder, the
		// only rewrite that still points at it is that file's path, which would
		// put the hidden folder's layout into a note this surface can read.
		if was.ambiguous || after.isHidden(want) {
			lost++
			continue
		}
		repl, ok := writeLink(l, want, was.style, was.asWritten, srcNew, after)
		if !ok {
			lost++
			continue
		}
		buf.Write(body[last:l.start])
		buf.WriteString(repl)
		last = l.end
		changed++
	}
	if changed == 0 {
		return body, 0, lost
	}
	buf.Write(body[last:])
	return buf.Bytes(), changed, lost
}

// writeLink produces the new path for a link, in the style it was written in,
// and checks that it resolves to the file it should. If the original style
// cannot express it unambiguously, the full vault path is the fallback, because
// nothing else resolves the same way under every reading of Obsidian's rules.
//
// What it returns is the text that replaces the path's bytes: escaped for a
// markdown destination and for the YAML scalar it sits in, and checked by
// parsing it back.
func writeLink(l foundLink, want string, style linkStyle, asWritten bool, source string, ix *linkIndex) (string, bool) {
	strip := func(p string) string {
		if !asWritten && strings.EqualFold(path.Ext(p), ".md") {
			return strings.TrimSuffix(p, path.Ext(p))
		}
		return p
	}
	// A bare name matches a note at the root by rule 1, but the user wrote a
	// name, not a path, and the rewrite should stay one.
	if !strings.Contains(strings.Trim(strings.TrimSpace(l.target), "/"), "/") && !strings.HasPrefix(strings.TrimSpace(l.target), "/") {
		style = styleName
	}
	var tries []string
	switch style {
	case styleName:
		if ix.uniqueName(want) {
			tries = append(tries, strip(path.Base(want)))
		}
	case styleRelative:
		if r, err := filepath.Rel(filepath.FromSlash(path.Dir(source)), filepath.FromSlash(want)); err == nil {
			r = filepath.ToSlash(r)
			// An explicitly relative link stays explicit: "../Notes/Pike.md"
			// becomes "./Esox.md", not "Esox.md", which would also match by name.
			t := strings.TrimSpace(l.target)
			if (strings.HasPrefix(t, "./") || strings.HasPrefix(t, "../")) && !strings.HasPrefix(r, "../") {
				r = "./" + r
			}
			tries = append(tries, strip(r))
		}
	}
	root := strip(want)
	if strings.HasPrefix(strings.TrimSpace(l.target), "/") {
		root = "/" + root
	}
	tries = append(tries, root)

	for _, t := range tries {
		if got, ok := ix.resolve(t, source); !ok || got.rel != want {
			continue
		}
		var out string
		if !l.markdown {
			// A wiki link cannot carry these, so a file named with one cannot be
			// linked this way at all; leave the link for a human.
			if strings.ContainsAny(t, "[]|#^") {
				return "", false
			}
			out = t
		} else {
			switch {
			case l.angle:
				if strings.ContainsAny(t, "<>\n") {
					return "", false
				}
				// Spaces may stay raw inside <...>; "%" and "#" may not, or
				// the link would read back as a different path.
				out = strings.NewReplacer("%", "%25", "#", "%23").Replace(t)
			case l.encoded || strings.ContainsAny(t, " ()#%<>\t"):
				out = escapePath(t)
			default:
				out = t
			}
			if !mdRoundTrips(out, t, l.angle) {
				return "", false
			}
		}
		esc, ok := yamlEscape(out, l.yaml)
		if !ok {
			return "", false
		}
		return esc, true
	}
	return "", false
}

// mdRoundTrips reports whether a markdown destination parses back, through
// findLinks itself, to exactly the path it was written for.
func mdRoundTrips(dest, want string, angle bool) bool {
	link := "[x](" + dest + ")"
	if angle {
		link = "[x](<" + dest + ">)"
	}
	got := findLinks([]byte(link))
	return len(got) == 1 && got[0].target == want && got[0].start+len(dest) == got[0].end
}

// escapePath percent-encodes each segment, as Obsidian writes a markdown link
// to a file whose name has a space in it.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if s == "." || s == ".." {
			continue
		}
		e := url.PathEscape(s)
		// PathEscape leaves these alone, but an unescaped one ends the link.
		e = strings.NewReplacer("(", "%28", ")", "%29").Replace(e)
		parts[i] = e
	}
	return strings.Join(parts, "/")
}

// relinkPlan is one note's rewrite, computed before the move and written after.
type relinkPlan struct {
	abs, rel  string // where the note is when the plan is written
	src       string // where it was when its links were resolved: rel, except for the moved note
	seen, new []byte
	links     int
}

// SkippedNote is a note holding a link to the moved file that was left as it
// was, and why.
type SkippedNote struct {
	Path   string
	Reason string
}

// MoveResult is what a move did beyond the move itself.
type MoveResult struct {
	From, To string

	// Notes whose links were rewritten, sorted. Includes To when the moved
	// note's own relative links, or links to itself, had to change.
	Relinked []string
	// Links rewritten across all of them.
	Links int

	// Notes with a link the move breaks that were left unchanged.
	Skipped []SkippedNote
	// Notes with a link the move breaks that no safe rewrite could keep
	// pointing at its file: a name containing "|" or "#", a name the vault
	// shares, or a file in an excluded folder. Never names the excluded file.
	Unfixable []string

	// Set when links were not checked at all. The move itself still happened.
	LinksUnchecked string
	// True when MCP_EXCLUDE is set: notes in those folders were not read, so a
	// link in one of them may still point at the old path.
	ExcludedUnchecked bool
}

// moveHook, when set by a test, runs at named points in a move, so a test can
// change the vault exactly where `ob sync` could.
var moveHook func(stage string)

func hook(stage string) {
	if moveHook != nil {
		moveHook(stage)
	}
}

// linkNeedles are the strings at least one of which must appear, ignoring
// case, in any note holding a link the move can change. A link changes only if
// it resolved to the moved file before, so it ends with the old name, or would
// resolve to the moved file after, so it ends with the new one. Either way the
// name is in the note, plainly or percent-encoded.
func linkNeedles(movedOld, movedNew string) [][]byte {
	var out [][]byte
	for _, rel := range []string{movedOld, movedNew} {
		name := path.Base(rel)
		if strings.EqualFold(path.Ext(name), ".md") {
			name = strings.TrimSuffix(name, path.Ext(name))
		}
		for _, n := range []string{name, url.PathEscape(name), escapePath(name)} {
			out = append(out, bytes.ToLower([]byte(n)))
		}
	}
	return out
}

func mayHoldLink(body []byte, needles [][]byte) bool {
	lower := bytes.ToLower(body)
	for _, n := range needles {
		if bytes.Contains(lower, n) {
			return true
		}
	}
	return false
}

// planRelinks reads every note this server may see and works out which would
// need their links rewritten. Nothing is written here.
//
// Notes in MCP_EXCLUDE folders are not read: the exclusion promises this server
// never opens them, and a move is no reason to break that promise. The result
// says so instead. AGENTS.md and CLAUDE.md are read — readable() allows it — so
// that a broken link in one is reported, but they are never written.
//
// A note that cannot mention the moved file by name is not parsed at all, which
// is most of the vault on most moves. An error, the cap, or a cancelled request
// abandons the whole plan, like a failed index: a partial plan would report a
// move as clean when it is not.
func (v *Vault) planRelinks(ctx context.Context, fromAbs, toAbs string, before, after *linkIndex, res *MoveResult) ([]relinkPlan, error) {
	movedOld, movedNew := v.Rel(fromAbs), v.Rel(toAbs)
	needles := linkNeedles(movedOld, movedNew)
	var plans []relinkPlan
	var skipped []SkippedNote
	var unfixable []string
	scanned := 0
	err := filepath.WalkDir(v.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return errIndexPartial
		}
		if d.IsDir() {
			if p != v.root && (strings.HasPrefix(d.Name(), ".") || v.Excluded(p)) {
				return fs.SkipDir
			}
			return nil
		}
		// Symlinks are skipped, not followed. Reading through one would let a
		// link inside the vault expose whatever it points at, and writing one
		// would be worse: atomicWrite renames over the link itself, which turns
		// it into a regular file holding the target's contents.
		if !d.Type().IsRegular() || !isNote(d.Name()) || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if p == fromAbs || v.Excluded(p) {
			return nil // the moved note is planned by Move itself
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
		rel := v.Rel(p)
		if err != nil {
			skipped = append(skipped, SkippedNote{Path: rel, Reason: "could not be read"})
			return nil
		}
		if !mayHoldLink(body, needles) {
			return nil
		}
		updated, n, lost := relinkNote(body, rel, rel, movedOld, movedNew, before, after)
		if lost > 0 {
			unfixable = append(unfixable, rel)
		}
		if n == 0 {
			return nil
		}
		if err := v.writable(p); err != nil {
			skipped = append(skipped, SkippedNote{Path: rel, Reason: "protected file"})
			return nil
		}
		plans = append(plans, relinkPlan{abs: p, rel: rel, src: rel, seen: body, new: updated, links: n})
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Skipped = append(res.Skipped, skipped...)
	res.Unfixable = append(res.Unfixable, unfixable...)
	return plans, nil
}

// applyRelink writes one planned rewrite, after the rename has landed. It
// returns the links rewritten and the links found that could not be.
//
// The concurrent-writer guard applies as to every other edit. If Obsidian
// changed the note since it was planned, the plan is redone once against the
// note as it is now — the links it needs are a property of the note's text and
// the two indexes, both still in hand — and only a second collision gives up.
func (v *Vault) applyRelink(p relinkPlan, movedOld, movedNew string, before, after *linkIndex) (int, int, error) {
	lost := 0
	if err := verifyUnchanged(p.abs, true, p.seen); err != nil {
		if !errors.Is(err, ErrConflict) {
			return 0, 0, err
		}
		current, rerr := os.ReadFile(p.abs)
		if rerr != nil {
			return 0, 0, ErrConflict
		}
		updated, n, l := relinkNote(current, p.src, p.rel, movedOld, movedNew, before, after)
		lost = l
		if n == 0 {
			return 0, lost, nil // the edit in Obsidian already fixed or removed the link
		}
		hook("replanned")
		if err := verifyUnchanged(p.abs, true, current); err != nil {
			return 0, lost, err
		}
		p.new, p.links = updated, n
	}
	if err := atomicWrite(p.abs, v.applyStamp(p.new, false)); err != nil {
		return 0, lost, err
	}
	return p.links, lost, nil
}

// applyRelinks writes every plan and records the outcome of each in res. A
// note that cannot be written is reported, not turned into a failed move: the
// file has moved by then, and failing the call would invite a retry that
// cannot succeed.
func (v *Vault) applyRelinks(plans []relinkPlan, movedOld, movedNew string, before, after *linkIndex, res *MoveResult) {
	for _, p := range plans {
		n, lost, err := v.applyRelink(p, movedOld, movedNew, before, after)
		if lost > 0 {
			res.Unfixable = appendOnce(res.Unfixable, p.rel)
		}
		switch {
		case errors.Is(err, ErrConflict):
			res.Skipped = append(res.Skipped, SkippedNote{Path: p.rel, Reason: "changed in Obsidian during the move"})
		case err != nil:
			res.Skipped = append(res.Skipped, SkippedNote{Path: p.rel, Reason: "could not be written"})
		case n > 0:
			res.Relinked = append(res.Relinked, p.rel)
			res.Links += n
		}
	}
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}
