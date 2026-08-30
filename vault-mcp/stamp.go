package main

import (
	"regexp"
	"strings"
	"time"
)

// The vault-wide agent stamp: three YAML frontmatter properties written by
// every surface that edits a note on an agent's behalf.
//
//	agent-created:  2026-08-20T09:11:03Z   set once, never rewritten
//	agent-modified: 2026-08-29T14:02:11Z   last agent write of any kind
//	agent:          claude-voice           who made that write
//
// The names are deliberately NOT Claude-specific and the identity lives in the
// value, so a second agent adopts the same three properties rather than
// inventing a fourth schema. The registry of names is in the root README's
// shared contract; this server writes MCP_STAMP_AGENT.
//
// Why frontmatter when every write is already a git commit: the snapshot repo
// is not visible from Obsidian on the phone, and a note that leaves the vault
// through Sync carries none of its history. The stamp travels with the note.
// See obsidian-vault/DECISIONS.md#agent-stamps-in-frontmatter.
const (
	keyAgent    = "agent"
	keyModified = "agent-modified"
	keyCreated  = "agent-created"
)

// Agent names go into YAML unquoted, so the value must not be able to end the
// scalar and start something else. Validated once at startup rather than
// escaped at every write: a name is operator configuration, not user input, and
// a startup failure is the loud version of this mistake.
var agentNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validAgentName(name string) bool {
	return agentNamePattern.MatchString(name)
}

// Two different questions, deliberately answered by two different patterns.
//
// The narrow one identifies OUR keys, for rewriting. It can afford to be strict:
// only three exact ASCII names ever need to match it.
//
// The wide one answers "is this line a property at all", which is what decides
// whether a block is frontmatter. It has to accept anything Obsidian would,
// because getting it wrong on a note the user wrote demotes that note's real
// properties to body text. Using the narrow pattern for both was a bug: a note
// whose first property was `Date created:` or `Título:` failed the test and had
// a second block prepended above its own.
//
// Both require no leading whitespace, and a space or end-of-line after the
// colon. The space matters: "agent:value" is the plain scalar "agent:value" in
// YAML, not a mapping. The leading-whitespace requirement is what keeps a nested
// key — inside some other property's block — from being mistaken for a top-level
// one.
var (
	topLevelKeyPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+):([ \t].*)?$`)

	// A leading "-" is excluded because a block that opens with a list item is
	// a sequence, not a mapping, and Obsidian shows no properties for it.
	// Comments are handled separately by the caller.
	anyKeyPattern = regexp.MustCompile(`^[^ \t#\-][^:]*:([ \t].*)?$`)
)

// topLevelKey names the property on this line if it is one of the plain ASCII
// keys this file rewrites, and "" otherwise.
func topLevelKey(line string) string {
	m := topLevelKeyPattern.FindStringSubmatch(chomp(line))
	if m == nil {
		return ""
	}
	return m[1]
}

// isPropertyLine reports whether this line reads as a top-level YAML property,
// under the widest reading Obsidian would accept: spaces in the name, non-ASCII,
// quotes.
func isPropertyLine(line string) bool {
	return anyKeyPattern.MatchString(chomp(line))
}

// chomp drops a trailing CR so a note with CRLF line endings is still
// recognised. Rewritten lines keep whichever ending they had; lines this adds
// use LF. A mixed-ending file is cosmetically odd and semantically fine — the
// alternative is failing to see existing frontmatter and prepending a second
// block, which silently destroys the note's properties in Obsidian.
func chomp(s string) string {
	return strings.TrimSuffix(s, "\r")
}

func stampTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// frontmatterEnd returns the index of the closing "---" line, or 0 when the
// note has no frontmatter.
//
// Frontmatter and a leading horizontal rule are the same bytes, so the delimiter
// alone is not enough: "---\nSome quoted line\n---" is a rule around a line of
// prose, and inserting properties into it would corrupt the note. The block is
// only treated as frontmatter when its first non-empty line is a top-level key
// or a comment — which is also the condition under which Obsidian parses it.
//
// The false negative is safe: a block of genuinely invalid YAML gets a fresh
// stamp block prepended above it, leaving its content untouched. The false
// positive is not, which is why this errs toward not recognising one.
func frontmatterEnd(lines []string) int {
	if len(lines) < 2 || chomp(lines[0]) != "---" {
		return 0
	}
	end := 0
	for i := 1; i < len(lines); i++ {
		if chomp(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == 0 {
		return 0
	}
	for i := 1; i < end; i++ {
		t := chomp(lines[i])
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "#") && !isPropertyLine(t) {
			return 0
		}
		break
	}
	return end
}

// maskStamp blanks the stamp lines to spaces, so a keyword search does not
// match text this server wrote itself.
//
// Without it the stamp becomes search noise the moment it is adopted: every
// agent-written note contains the word "agent", and this vault has real notes
// about agents. Masking only the three stamp lines rather than the whole block
// is deliberate — searching frontmatter is useful here, where notes carry
// properties like `water:` and `type:` worth finding by.
//
// Length is preserved exactly, so an offset found in the masked copy still
// indexes into the original.
func maskStamp(s string) string {
	lines := strings.Split(s, "\n")
	end := frontmatterEnd(lines)
	if end == 0 {
		return s
	}
	masked := false
	for i := 1; i < end; i++ {
		switch topLevelKey(lines[i]) {
		case keyAgent, keyModified, keyCreated:
			lines[i] = strings.Repeat(" ", len(lines[i]))
			masked = true
		}
	}
	if !masked {
		return s
	}
	return strings.Join(lines, "\n")
}

// bodyStart is the first line after the frontmatter block, for callers that
// want the note rather than its properties.
func bodyStart(lines []string) int {
	if end := frontmatterEnd(lines); end > 0 && end+1 < len(lines) {
		return end + 1
	}
	return 0
}

// stamp returns body with the agent stamp applied. An empty agent name disables
// stamping and returns body unchanged.
//
// Only the three stamp lines are touched; every other byte of the note —
// including the rest of the frontmatter — is preserved exactly. This is a
// deliberate rejection of parsing the YAML and re-emitting it: Obsidian's own
// property editor round-trips this block, and a marshal/unmarshal pass would
// reorder keys, requote strings and drop comments in every note an agent
// touches. See obsidian-vault/DECISIONS.md#agent-stamps-in-frontmatter.
//
// Idempotent: stamping an already-stamped note rewrites the same lines in place
// and never appends a second copy of a key.
func stamp(body []byte, agent string, created bool, now time.Time) []byte {
	if agent == "" {
		return body
	}
	ts := stampTime(now)

	lines := strings.Split(string(body), "\n")
	end := frontmatterEnd(lines)
	if end == 0 {
		var b strings.Builder
		b.WriteString("---\n")
		if created {
			b.WriteString(keyCreated + ": " + ts + "\n")
		}
		b.WriteString(keyModified + ": " + ts + "\n")
		b.WriteString(keyAgent + ": " + agent + "\n")
		b.WriteString("---\n")
		// One blank line between the block and the note, unless the note already
		// starts with one. Tested on the chomped first line rather than the raw
		// bytes so a CRLF note is treated the same as an LF one — the awk
		// implementation in obsidian-vault/scripts/hook-stamp.sh does the same,
		// and the two must not disagree about a note's bytes.
		if len(lines) > 0 && chomp(lines[0]) != "" {
			b.WriteString("\n")
		}
		b.Write(body)
		return terminate(b.String())
	}

	var seenAgent, seenModified, seenCreated bool
	for i := 1; i < end; i++ {
		cr := ""
		if strings.HasSuffix(lines[i], "\r") {
			cr = "\r"
		}
		switch topLevelKey(lines[i]) {
		case keyAgent:
			lines[i] = keyAgent + ": " + agent + cr
			seenAgent = true
		case keyModified:
			lines[i] = keyModified + ": " + ts + cr
			seenModified = true
		case keyCreated:
			// Never rewritten. The first agent write is the claim it makes; a
			// later edit updating it would turn "an agent made this note" into
			// "an agent touched it recently", which agent-modified already says.
			seenCreated = true
		}
	}

	var add []string
	if created && !seenCreated {
		add = append(add, keyCreated+": "+ts)
	}
	if !seenModified {
		add = append(add, keyModified+": "+ts)
	}
	if !seenAgent {
		add = append(add, keyAgent+": "+agent)
	}
	if len(add) > 0 {
		out := make([]string, 0, len(lines)+len(add))
		out = append(out, lines[:end]...)
		out = append(out, add...)
		out = append(out, lines[end:]...)
		lines = out
	}
	return terminate(strings.Join(lines, "\n"))
}

// terminate ends the note with a newline.
//
// awk's `print` terminates every record it writes, so without this the two
// implementations produce different bytes for a note that arrived without a
// final newline — the silent divergence both headers warn against. Normalising
// on the awk side is not practical (awk cannot tell), so the Go side matches it.
func terminate(s string) []byte {
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s)
}
