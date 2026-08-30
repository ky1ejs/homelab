package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 29, 14, 2, 11, 0, time.UTC)

const testStamp = "2026-08-29T14:02:11Z"

// stampingVault is the vault the deployed server runs: an agent name set, and a
// frozen clock so a test can assert the exact recorded timestamp.
func stampingVault(t *testing.T) *Vault {
	t.Helper()
	v := newTestVault(t)
	v.SetStampAgent("claude-voice")
	v.now = func() time.Time { return testNow }
	return v
}

func read(t *testing.T, v *Vault, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(v.root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestStampAddsBlockToNoteWithoutFrontmatter(t *testing.T) {
	got := string(stamp([]byte("# Title\n\nBody text.\n"), "claude-voice", true, testNow))
	want := "---\n" +
		"agent-created: " + testStamp + "\n" +
		"agent-modified: " + testStamp + "\n" +
		"agent: claude-voice\n" +
		"---\n\n" +
		"# Title\n\nBody text.\n"
	if got != want {
		t.Errorf("stamp() =\n%q\nwant\n%q", got, want)
	}
}

// The rest of the frontmatter must survive byte-for-byte. The reason this
// server edits lines instead of parsing and re-emitting the YAML is that
// Obsidian's own property editor round-trips this block: a marshal pass would
// reorder keys, requote strings and drop comments in every note it touches.
func TestStampPreservesExistingFrontmatterExactly(t *testing.T) {
	body := "---\n" +
		"type: fishing-research\n" +
		"date: 2026-08-17\n" +
		"water: \"Multiple — Northeast hubs\"\n" +
		"tags:\n" +
		"  - fishing\n" +
		"  - decision\n" +
		"---\n\n" +
		"# Best Hub\n"

	got := string(stamp([]byte(body), "claude-voice", false, testNow))

	for _, keep := range []string{
		"type: fishing-research\n",
		"date: 2026-08-17\n",
		"water: \"Multiple — Northeast hubs\"\n",
		"tags:\n  - fishing\n  - decision\n",
		"\n# Best Hub\n",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("stamp() dropped or rewrote %q:\n%s", keep, got)
		}
	}
	if !strings.Contains(got, "agent-modified: "+testStamp+"\nagent: claude-voice\n---\n") {
		t.Errorf("stamp() did not append the stamp at the end of the block:\n%s", got)
	}
	// created=false: this note existed before an agent touched it, and saying
	// otherwise is the one claim the stamp must never invent.
	if strings.Contains(got, keyCreated) {
		t.Errorf("stamp() added %s on an edit:\n%s", keyCreated, got)
	}
}

func TestStampIsIdempotent(t *testing.T) {
	once := stamp([]byte("Body.\n"), "claude-voice", true, testNow)

	later := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	twice := string(stamp(once, "claude-agent", false, later))

	if n := strings.Count(twice, keyModified+":"); n != 1 {
		t.Errorf("%s appears %d times, want 1:\n%s", keyModified, n, twice)
	}
	if n := strings.Count(twice, "\nagent:"); n != 1 {
		t.Errorf("agent appears %d times, want 1:\n%s", n, twice)
	}
	if !strings.Contains(twice, "agent-modified: 2026-08-30T09:00:00Z") {
		t.Errorf("second write did not update %s:\n%s", keyModified, twice)
	}
	if !strings.Contains(twice, "agent: claude-agent\n") {
		t.Errorf("second write did not update the writer identity:\n%s", twice)
	}
	// agent-created is the first write's claim. A later edit updating it would
	// turn "an agent made this note" into "an agent touched it recently", which
	// agent-modified already says.
	if !strings.Contains(twice, "agent-created: "+testStamp+"\n") {
		t.Errorf("second write rewrote %s:\n%s", keyCreated, twice)
	}
}

// Frontmatter and a leading horizontal rule are the same bytes. Treating a rule
// as frontmatter would insert properties into the note's prose.
func TestStampDoesNotMistakeHorizontalRuleForFrontmatter(t *testing.T) {
	cases := map[string]string{
		"rule around prose":  "---\nA line of quoted prose.\n---\n\nBody.\n",
		"unterminated rule":  "---\n# Title\n\nBody.\n",
		"rule on empty note": "---\n",
		"blank first line":   "\n---\ntype: note\n---\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got := string(stamp([]byte(body), "claude-voice", false, testNow))
			if !strings.HasPrefix(got, "---\nagent-modified: "+testStamp+"\nagent: claude-voice\n---\n") {
				t.Errorf("stamp() did not prepend a fresh block:\n%s", got)
			}
			if !strings.HasSuffix(got, body) {
				t.Errorf("stamp() altered the note body:\n%s", got)
			}
		})
	}
}

func TestStampRecognisesFrontmatterEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // a substring that proves the block was edited, not replaced
	}{
		{
			name: "empty block",
			body: "---\n---\n\nBody.\n",
			want: "---\nagent-modified: " + testStamp + "\nagent: claude-voice\n---\n\nBody.\n",
		},
		{
			name: "comment first",
			body: "---\n# a comment\ntype: note\n---\n",
			want: "# a comment\ntype: note\nagent-modified: ",
		},
		{
			name: "CRLF endings",
			body: "---\r\ntype: note\r\n---\r\n\r\nBody.\r\n",
			want: "type: note\r\nagent-modified: " + testStamp + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(stamp([]byte(tc.body), "claude-voice", false, testNow))
			if !strings.Contains(got, tc.want) {
				t.Errorf("stamp() =\n%q\nwant it to contain\n%q", got, tc.want)
			}
			if strings.Count(got, "\n---") != 1 {
				t.Errorf("stamp() did not reuse the existing block:\n%q", got)
			}
		})
	}
}

// Regression: a property name with a space, an accent or quotes in it is still
// a property. Recognising only [A-Za-z0-9_.-] meant a note whose FIRST property
// was `Date created:` failed the frontmatter test and had a second block
// prepended above its own — which in Obsidian demotes every real property the
// user had to body text, and never heals, because the next write recognises the
// stamp block instead.
func TestStampRecognisesPropertyNamesObsidianAllows(t *testing.T) {
	cases := map[string]string{
		"space in key":    "---\nDate created: 2026-01-02\ntags:\n  - reading\n---\n\nMy actual note.\n",
		"non-ascii key":   "---\nTítulo: Hola\n---\n\nBody.\n",
		"quoted key":      "---\n\"quoted key\": 1\n---\n\nBody.\n",
		"wikilink value":  "---\nRelated to: \"[[Books]]\"\n---\n\nBody.\n",
		"key with digits": "---\nISO 8601 date: 2026-01-02\n---\n\nBody.\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got := string(stamp([]byte(body), "claude-voice", false, testNow))
			if strings.Count(got, "\n---") != 1 {
				t.Errorf("stamp() prepended a second block instead of reusing the existing one:\n%s", got)
			}
			if !strings.Contains(got, "agent-modified: "+testStamp+"\nagent: claude-voice\n---\n") {
				t.Errorf("stamp() did not add the stamp inside the block:\n%s", got)
			}
		})
	}

	// The horizontal-rule guard must still hold: a block whose first line is
	// prose, or a list, is not frontmatter.
	for name, body := range map[string]string{
		"prose": "---\nA line of quoted prose.\n---\n\nBody.\n",
		"list":  "---\n- just a list\n---\n\nBody.\n",
	} {
		t.Run(name, func(t *testing.T) {
			got := string(stamp([]byte(body), "claude-voice", false, testNow))
			if !strings.HasSuffix(got, body) {
				t.Errorf("stamp() edited a horizontal rule instead of prepending a block:\n%s", got)
			}
		})
	}
}

// Both implementations must produce identical bytes; awk's `print` always
// terminates the final record, so Go has to as well.
func TestStampAlwaysTerminatesTheNote(t *testing.T) {
	for name, body := range map[string]string{
		"no trailing newline":      "No trailing newline",
		"frontmatter unterminated": "---\ntype: note\n---\nBody with no newline",
	} {
		t.Run(name, func(t *testing.T) {
			got := string(stamp([]byte(body), "claude-voice", false, testNow))
			if !strings.HasSuffix(got, "\n") {
				t.Errorf("stamp() = %q, want it to end with a newline", got)
			}
		})
	}
	if got := string(stamp([]byte(""), "claude-voice", false, testNow)); !strings.HasSuffix(got, "---\n") {
		t.Errorf("stamp() on an empty note = %q", got)
	}
}

// A key nested inside another property is not ours. Only a top-level "agent:"
// with a space after the colon is the stamp.
func TestStampLeavesNestedAndScalarKeysAlone(t *testing.T) {
	body := "---\n" +
		"meeting:\n" +
		"  agent: someone-else\n" +
		"note: agent:value\n" +
		"---\n"
	got := string(stamp([]byte(body), "claude-voice", false, testNow))

	if !strings.Contains(got, "  agent: someone-else\n") {
		t.Errorf("stamp() rewrote a nested key:\n%s", got)
	}
	if !strings.Contains(got, "note: agent:value\n") {
		t.Errorf("stamp() rewrote a scalar containing a colon:\n%s", got)
	}
	if !strings.Contains(got, "\nagent: claude-voice\n") {
		t.Errorf("stamp() did not add its own top-level key:\n%s", got)
	}
}

func TestStampDisabledWithoutAnAgentName(t *testing.T) {
	body := []byte("# Title\n")
	if got := string(stamp(body, "", true, testNow)); got != string(body) {
		t.Errorf("stamp() with no agent = %q, want the note unchanged", got)
	}
}

// The name goes into YAML unquoted, so anything that could end the scalar and
// start something else has to be refused at startup.
func TestAgentNameValidation(t *testing.T) {
	for _, ok := range []string{"claude-voice", "claude-agent", "fishing.v2", "a", "agent_1"} {
		if !validAgentName(ok) {
			t.Errorf("validAgentName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", " ", "claude voice", "a: b", "x\nagent: y", "-leading", "quote\"d", "#comment"} {
		if validAgentName(bad) {
			t.Errorf("validAgentName(%q) = true, want false", bad)
		}
	}
}

// A blank MCP_STAMP is a dropped line, not a decision, and must not read as
// "off": stamping wrongly is fixable by editing notes, while a month of
// silently unstamped writes cannot be identified afterwards. Only an explicit 0
// disables, and a typo refuses to start rather than defaulting either way.
func TestStampConfigTreatsBlankAsOn(t *testing.T) {
	cases := []struct {
		name    string
		stamp   string
		agent   string
		unset   bool // absent entirely, as in an .env predating the convention
		want    string
		wantErr bool
	}{
		{name: "unset", unset: true, want: "claude-voice"},
		{name: "blank", stamp: "", want: "claude-voice"},
		{name: "explicitly on", stamp: "1", want: "claude-voice"},
		{name: "blank name", stamp: "1", agent: "  ", want: "claude-voice"},
		{name: "custom name", stamp: "1", agent: "fishing.v2", want: "fishing.v2"},
		{name: "explicitly off", stamp: "0", want: ""},
		{name: "typo", stamp: "true", wantErr: true},
		{name: "malformed name", stamp: "1", agent: "evil: injected", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MCP_ALLOW_NO_AUTH", "1")
			t.Setenv("MCP_STAMP", tc.stamp)
			t.Setenv("MCP_STAMP_AGENT", tc.agent)
			if tc.unset {
				// t.Setenv registered the restore; unset on top of it.
				os.Unsetenv("MCP_STAMP")
				os.Unsetenv("MCP_STAMP_AGENT")
			}

			cfg, err := loadConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("loadConfig() = %+v, want an error", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.stampAgent != tc.want {
				t.Errorf("stampAgent = %q, want %q", cfg.stampAgent, tc.want)
			}
		})
	}
}

// The stamp must not become search noise: "agent" is a word this vault uses in
// its own notes, and every agent-written note now contains it.
func TestSearchDoesNotMatchTheStampItWrote(t *testing.T) {
	v := stampingVault(t)
	if _, err := v.Create("Notes/Stamped", "A note about fishing.\n"); err != nil {
		t.Fatal(err)
	}
	write(t, v, "Notes/Real.md", "---\ntype: meeting\n---\n\nExperimenting via the Slack agent.\n")

	hits, err := v.Search("agent", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "Notes/Real.md" {
		t.Errorf("Search(\"agent\") = %+v, want only the note that says it", hits)
	}

	// Frontmatter that is not the stamp stays searchable — this vault files
	// notes by properties like type: and water:.
	hits, err = v.Search("meeting", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("Search(\"meeting\") = %+v, want the note with that property", hits)
	}
}

// A spoken search result should open with the note, not with its properties.
func TestSearchSnippetSkipsFrontmatter(t *testing.T) {
	v := stampingVault(t)
	if _, err := v.Create("Notes/Tippet", "# Tippet\n\nFluorocarbon in low water.\n"); err != nil {
		t.Fatal(err)
	}
	hits, err := v.Search("tippet", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("Search = %+v, want one title hit", hits)
	}
	if hits[0].Snippet != "Fluorocarbon in low water." {
		t.Errorf("snippet = %q, want the first line of prose", hits[0].Snippet)
	}
}

// ---------------------------------------------------------------------------
// Through the write paths
// ---------------------------------------------------------------------------

func TestCreateStampsAsAgentCreated(t *testing.T) {
	v := stampingVault(t)
	if _, err := v.Create("Notes/New", "# New\n"); err != nil {
		t.Fatal(err)
	}
	got := read(t, v, "Notes/New.md")
	for _, want := range []string{
		"agent-created: " + testStamp,
		"agent-modified: " + testStamp,
		"agent: claude-voice",
		"# New\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Create() note missing %q:\n%s", want, got)
		}
	}
}

func TestEditAndAppendStampWithoutClaimingCreation(t *testing.T) {
	v := stampingVault(t)
	write(t, v, "Notes/Existing.md", "---\ntype: note\n---\n\nOriginal line.\n")

	if _, err := v.Append("Notes/Existing", "Appended line."); err != nil {
		t.Fatal(err)
	}
	got := read(t, v, "Notes/Existing.md")
	if strings.Contains(got, keyCreated) {
		t.Errorf("Append() to an existing note claimed %s:\n%s", keyCreated, got)
	}
	if !strings.Contains(got, "type: note\n") || !strings.Contains(got, "Appended line.\n") {
		t.Errorf("Append() lost content:\n%s", got)
	}

	if _, err := v.Edit("Notes/Existing", "Original line.", "Edited line."); err != nil {
		t.Fatal(err)
	}
	got = read(t, v, "Notes/Existing.md")
	if strings.Count(got, keyModified+":") != 1 {
		t.Errorf("Edit() duplicated the stamp:\n%s", got)
	}
	if !strings.Contains(got, "Edited line.\n") {
		t.Errorf("Edit() did not apply:\n%s", got)
	}
}

// Append creating a note is still a creation, and must say so.
func TestAppendToMissingNoteClaimsCreation(t *testing.T) {
	v := stampingVault(t)
	if _, err := v.Append("Inbox", "- a thought"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, v, "Inbox.md"); !strings.Contains(got, "agent-created: "+testStamp) {
		t.Errorf("Append() creating a note did not record %s:\n%s", keyCreated, got)
	}
}

// Capture is the voice path: the same stamp, and the timestamped line intact.
func TestCaptureStamps(t *testing.T) {
	v := stampingVault(t)
	if _, err := v.Capture("Inbox.md", "buy tippet", testNow); err != nil {
		t.Fatal(err)
	}
	got := read(t, v, "Inbox.md")
	if !strings.Contains(got, "agent: claude-voice") {
		t.Errorf("Capture() did not stamp:\n%s", got)
	}
	if !strings.Contains(got, "- 2026-08-29 14:02 — buy tippet\n") {
		t.Errorf("Capture() lost the captured line:\n%s", got)
	}
}

// The stamp is applied to the bytes handed to atomicWrite, never as a second
// write. A stamp that landed afterwards would be a separate rename `ob sync`
// could observe on its own, and would sit outside the verifyUnchanged guard.
func TestStampedWriteIsASingleAtomicWrite(t *testing.T) {
	v := stampingVault(t)
	if _, err := v.Create("Notes/Once", "body\n"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(v.root, "Notes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".vault-mcp-") {
			t.Errorf("temp file %q left behind", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("Notes/ holds %d entries, want 1", len(entries))
	}
}

// Stamping must not become a way to write where writing is denied.
func TestStampDoesNotReachDeniedPaths(t *testing.T) {
	v := stampingVault(t)
	for _, ref := range []string{"CLAUDE.md", "4. Inbox/AGENTS.md", ".claude/settings.json"} {
		if _, err := v.Create(ref, "x"); err == nil {
			t.Errorf("Create(%q) succeeded, want denied", ref)
		}
	}
}
