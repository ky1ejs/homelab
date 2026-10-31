package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A base as Obsidian writes it, modelled on the hatch-occurrence base in the
// vault's own reference note: an or-group over `this`, a formula column, and
// view keys the Bases syntax does not document but the app writes anyway.
const goodBase = `filters:
  and:
    - type == "hatch-occurrence"
    - or:
        - water == this.asLink()
        - patterns.contains(this.asLink())
formulas:
  days_old: '(now() - file.ctime).days.round(0)'
properties:
  formula.days_old:
    displayName: "Age (days)"
views:
  - type: table
    name: Active
    limit: 50
    order:
      - file.name
      - formula.days_old
    sort:
      - property: file.mtime
        direction: DESC
    groupBy:
      property: water
      direction: ASC
    summaries:
      formula.days_old: Average
    rowHeight: tall
    columnSize:
      file.name: 260
  - type: cards
    name: Gallery
    image: note.cover
    cardSize: 250
`

func TestValidateBaseAcceptsWhatObsidianWrites(t *testing.T) {
	warnings, err := ValidateBase(goodBase)
	if err != nil {
		t.Fatalf("ValidateBase(goodBase) = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

// Only what stops a base loading is refused. Each case names the problem the
// reply must carry, because the reply is the only thing the model sees.
func TestValidateBaseRefusesWhatWouldNotLoad(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"nested double quotes":  {"views:\n  - type: table\nfilters: \"if(done, \"Yes\", \"No\")\"\n", "does not parse"},
		"duplicate key":         {"views:\n  - type: table\nviews:\n  - type: list\n", "does not parse"},
		"empty":                 {"", "empty"},
		"scalar root":           {"just text\n", "top level must be a mapping"},
		"no views":              {"filters: 'file.ext == \"md\"'\n", "no 'views'"},
		"views not a list":      {"views:\n  type: table\n", "'views' must be a list"},
		"empty views":           {"views: []\n", "'views' is empty"},
		"view without type":     {"views:\n  - name: Active\n", `view "Active" has no 'type'`},
		"two ops in a group":    {"filters:\n  and: ['a']\n  or: ['b']\nviews:\n  - type: table\n", "exactly one of"},
		"unknown op":            {"filters:\n  any: ['a']\nviews:\n  - type: table\n", `"any"`},
		"group holds a scalar":  {"filters:\n  and: 'a'\nviews:\n  - type: table\n", "must hold a list"},
		"view filter bad":       {"views:\n  - type: table\n    name: V\n    filters:\n      nor: ['a']\n", `view "V" filters`},
		"sort without property": {"views:\n  - type: table\n    sort:\n      - direction: ASC\n", "need a 'property'"},
		"groupBy a list":        {"views:\n  - type: table\n    groupBy: [a, b]\n", "'groupBy' must be a mapping"},
		"formula a list":        {"formulas:\n  x: [1, 2]\nviews:\n  - type: table\n", `formula "x"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateBase(c.body)
			var be *BaseError
			if !errors.As(err, &be) {
				t.Fatalf("err = %v, want *BaseError", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// An undefined formula is the base that loads and renders an empty column with
// no banner at all. Not refused — a two-step edit passes through this state —
// but never silent.
func TestValidateBaseWarnsAboutWhatRendersWrongly(t *testing.T) {
	body := `custom: 1
views:
  - type: table
    name: V
    order:
      - formula.missing
    sort:
      - property: file.name
        direction: down
    summaries:
      price: Total
`
	warnings, err := ValidateBase(body)
	if err != nil {
		t.Fatalf("ValidateBase = %v, want warnings only", err)
	}
	all := strings.Join(warnings, "\n")
	for _, want := range []string{`"custom"`, "formula.missing", `"down"`, `"Total"`} {
		if !strings.Contains(all, want) {
			t.Errorf("warnings %q do not mention %s", all, want)
		}
	}
}

func TestBaseCreateIsValidatedAndNeverStamped(t *testing.T) {
	v := newTestVault(t)
	v.SetStampAgent("claude-voice")

	rel, warnings, err := v.CreateDocument("Fishing/Hatches.base", goodBase)
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if rel != "Fishing/Hatches.base" || len(warnings) != 0 {
		t.Fatalf("rel, warnings = %q, %v", rel, warnings)
	}
	got, _ := os.ReadFile(filepath.Join(v.root, "Fishing/Hatches.base"))
	// Byte-identical: frontmatter at the top of a .base is a second YAML
	// document, and Obsidian refuses the file.
	if string(got) != goodBase {
		t.Fatalf("base on disk differs from what was written:\n%s", got)
	}

	if _, err := v.Create("Broken.base", "views: [\n"); err == nil {
		t.Fatal("Create accepted a base that does not parse")
	}
	if _, err := os.Stat(filepath.Join(v.root, "Broken.base")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused base was written anyway")
	}
}

func TestBaseEditValidatesTheWholeResult(t *testing.T) {
	v := newTestVault(t)
	v.SetStampAgent("claude-voice")
	p := write(t, v, "Hatches.base", goodBase)

	// Breaks the indentation of the line it lands in, though the replacement
	// text alone is innocuous.
	if _, _, err := v.EditDocument("Hatches.base", "    name: Active\n", "  name: Active\n"); err == nil {
		t.Fatal("an edit that breaks the base's YAML was accepted")
	}
	if got, _ := os.ReadFile(p); string(got) != goodBase {
		t.Fatal("a refused edit changed the file")
	}

	_, warnings, err := v.EditDocument("Hatches.base", "      - formula.days_old\n", "      - formula.age\n")
	if err != nil {
		t.Fatalf("EditDocument: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "formula.age") {
		t.Fatalf("warnings = %v, want one about formula.age", warnings)
	}
	got, _ := os.ReadFile(p)
	if strings.Contains(string(got), keyAgent+":") {
		t.Fatalf("edit stamped a base:\n%s", got)
	}
}

// Notes keep everything they had: the stamp, and resolution by bare title.
func TestNotesAreUnchangedByBaseSupport(t *testing.T) {
	v := newTestVault(t)
	v.SetStampAgent("claude-voice")
	write(t, v, "Hatches.base", goodBase)

	rel, _, err := v.CreateDocument("Hatches", "A note beside the base.")
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if rel != "Hatches.md" {
		t.Fatalf("bare title resolved to %q, want Hatches.md — a base is only ever named with .base", rel)
	}
	got, _ := os.ReadFile(filepath.Join(v.root, "Hatches.md"))
	if !strings.Contains(string(got), "agent-created:") {
		t.Fatalf("the note lost its stamp:\n%s", got)
	}
}

func TestBaseIsReadableSearchableAndListed(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Fishing/Fly Reference.base", goodBase)
	write(t, v, "Fishing/Fly Reference.md", "The note.\n")

	body, err := v.Read("Fishing/Fly Reference.base")
	if err != nil || body != goodBase {
		t.Fatalf("Read(base) = %q, %v", body, err)
	}

	hits, err := v.Search("hatch-occurrence", 5)
	if err != nil || len(hits) != 1 || hits[0].Path != "Fishing/Fly Reference.base" {
		t.Fatalf("Search = %v, %v; want the base", hits, err)
	}

	names, err := v.List("Fishing", 50)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "Fly Reference.base,Fly Reference" {
		t.Fatalf("List = %v, want the base with its extension and the note without", names)
	}
}

func TestBaseCannotBeAppendedToOrCapturedInto(t *testing.T) {
	v := newTestVault(t)
	write(t, v, "Hatches.base", goodBase)

	if _, err := v.Append("Hatches.base", "x"); !errors.Is(err, ErrNotNote) {
		t.Fatalf("Append(base) = %v, want ErrNotNote", err)
	}
	if err := v.CheckWritable("Inbox.base"); !errors.Is(err, ErrNotNote) {
		t.Fatalf("CheckWritable(base) = %v, want ErrNotNote", err)
	}
}

// Takes the attachment path through Move: a bare rename, no stamp.
func TestBaseMovesUnstamped(t *testing.T) {
	v := newTestVault(t)
	v.SetStampAgent("claude-agent")
	write(t, v, "Hatches.base", goodBase)

	if _, to, err := v.Move("Hatches.base", "Fishing/Embedded Views/Hatches.base"); err != nil || to != "Fishing/Embedded Views/Hatches.base" {
		t.Fatalf("Move = %q, %v", to, err)
	}
	got, _ := os.ReadFile(filepath.Join(v.root, "Fishing/Embedded Views/Hatches.base"))
	if string(got) != goodBase {
		t.Fatalf("move changed the base:\n%s", got)
	}
	if _, _, err := v.Move("Fishing/Embedded Views/Hatches.base", "Hatches.md"); !errors.Is(err, ErrKindChange) {
		t.Fatalf("Move(base -> note) = %v, want ErrKindChange", err)
	}
}

// Widening the file types must not widen the paths: the dotted-folder rule and
// the exclusions apply to a base exactly as to a note.
func TestBaseHonoursTheDenyListAndExclusions(t *testing.T) {
	v := newTestVault(t, "4. Inbox", "Private/Plan")
	write(t, v, "4. Inbox/Triage.base", goodBase)
	write(t, v, "Private/Plan.base", goodBase)

	for _, ref := range []string{".obsidian/x.base", ".claude/x.base", "Projects/.trash/x.base"} {
		if _, err := v.Create(ref, goodBase); !errors.Is(err, ErrDenied) {
			t.Errorf("Create(%s) = %v, want ErrDenied", ref, err)
		}
	}
	// A bare-title exclusion hides the base of that name too.
	for _, ref := range []string{"4. Inbox/Triage.base", "Private/Plan.base"} {
		if _, err := v.Read(ref); !errors.Is(err, ErrExcluded) {
			t.Errorf("Read(%s) = %v, want ErrExcluded", ref, err)
		}
	}
	if hits, _ := v.Search("hatch-occurrence", 5); len(hits) != 0 {
		t.Errorf("Search surfaced excluded bases: %v", hits)
	}
}

func TestSetBasesDocOnlyKeepsAReachableNote(t *testing.T) {
	v := newTestVault(t, "Private")
	write(t, v, "Reference/Bases.md", "# Bases\n")
	write(t, v, "Private/Bases.md", "# Bases\n")

	if err := v.SetBasesDoc("Reference/Bases"); err != nil || v.BasesDoc() != "Reference/Bases" {
		t.Fatalf("SetBasesDoc(existing) = %v, doc %q", err, v.BasesDoc())
	}
	if err := v.SetBasesDoc("Reference/Missing"); !errors.Is(err, ErrNotFound) || v.BasesDoc() != "" {
		t.Fatalf("SetBasesDoc(missing) = %v, doc %q; want ErrNotFound and no pointer", err, v.BasesDoc())
	}
	if err := v.SetBasesDoc("Private/Bases"); !errors.Is(err, ErrExcluded) || v.BasesDoc() != "" {
		t.Fatalf("SetBasesDoc(excluded) = %v, doc %q; want ErrExcluded and no pointer", err, v.BasesDoc())
	}
	if err := v.SetBasesDoc(""); err != nil || v.BasesDoc() != "" {
		t.Fatalf("SetBasesDoc(blank) = %v, doc %q", err, v.BasesDoc())
	}
}

// voiceSession connects a client to the voice surface over an in-memory
// transport, so these tests read what the model is actually told.
func voiceSession(t *testing.T, s *server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := s.mcpServer().Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callText(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// The point of the pointer: the model is sent to the vault's own reference
// before it writes a base, and sent back to it when a write is refused.
func TestVoiceSurfacePointsAtTheBasesDoc(t *testing.T) {
	s := newSurface(t, false)
	write(t, s.vault, "Reference/Bases Tips.md", "# Bases\n")
	if err := s.vault.SetBasesDoc("Reference/Bases Tips"); err != nil {
		t.Fatal(err)
	}
	cs := voiceSession(t, s)

	if got := cs.InitializeResult().Instructions; !strings.Contains(got, "read the note 'Reference/Bases Tips'") {
		t.Fatalf("instructions do not point at the bases doc:\n%s", got)
	}

	msg, isErr := callText(t, cs, "create_note", map[string]any{"note": "V.base", "content": "filters:\n  nor: [a]\n"})
	if !isErr || !strings.Contains(msg, "Nothing was written") || !strings.Contains(msg, "no 'views'") ||
		!strings.Contains(msg, "Read 'Reference/Bases Tips'") {
		t.Fatalf("refused create reply = %q", msg)
	}

	msg, isErr = callText(t, cs, "create_note", map[string]any{"note": "V.base", "content": "views:\n  - type: table\n    order: [formula.x]\n"})
	if isErr || !strings.Contains(msg, "Created V.base") || !strings.Contains(msg, "formula.x") {
		t.Fatalf("create-with-warning reply = %q", msg)
	}

	msg, isErr = callText(t, cs, "append_note", map[string]any{"note": "V.base", "text": "x"})
	if !isErr || !strings.Contains(msg, "Use edit_note") {
		t.Fatalf("append-to-base reply = %q", msg)
	}
}

// With no usable reference the surface still serves bases; it just does not
// send the model to a note it could not read.
func TestVoiceSurfaceWithoutABasesDoc(t *testing.T) {
	s := newSurface(t, false)
	cs := voiceSession(t, s)
	if got := cs.InitializeResult().Instructions; strings.Contains(got, "read the note") || !strings.Contains(got, ".base") {
		t.Fatalf("instructions = %q; want bases mentioned and no pointer", got)
	}
}

// Review finding: yaml.Unmarshal reads the first document and drops the rest,
// so a stamp appended as a second document passed. Obsidian refuses the file.
func TestValidateBaseRefusesASecondDocument(t *testing.T) {
	for name, body := range map[string]string{
		"stamp appended":     goodBase + "---\nagent: claude-voice\n",
		"broken second doc":  "views:\n  - type: table\n---\nfoo: [\n",
		"frontmatter-shaped": "---\nagent: claude-voice\n---\nviews:\n  - type: table\n",
		"trailing empty doc": "views:\n  - type: table\n---\n",
	} {
		t.Run(name, func(t *testing.T) {
			var be *BaseError
			if _, err := ValidateBase(body); !errors.As(err, &be) {
				t.Fatalf("ValidateBase accepted a multi-document stream: %v", err)
			}
		})
	}
	// A single document with an explicit start marker is one document.
	if _, err := ValidateBase("---\n" + goodBase); err != nil {
		t.Fatalf("explicit document start refused: %v", err)
	}

	v := newTestVault(t)
	p := write(t, v, "Hatches.base", goodBase)
	if _, _, err := v.EditDocument("Hatches.base", "    cardSize: 250\n", "    cardSize: 250\n---\nagent: claude-voice\n"); err == nil {
		t.Fatal("an edit adding a second document was accepted")
	}
	if got, _ := os.ReadFile(p); string(got) != goodBase {
		t.Fatal("a refused edit changed the file")
	}
}

// Review finding: a key present with no value is absent to Obsidian, and was
// refused here — which would make the base uneditable through this server.
func TestValidateBaseTreatsNullKeysAsAbsent(t *testing.T) {
	body := `filters:
formulas:
properties:
summaries:
views:
  - type: table
    filters:
    order:
    sort:
    groupBy:
    summaries:
`
	if _, err := ValidateBase(body); err != nil {
		t.Fatalf("null-valued keys refused: %v", err)
	}
	if _, err := ValidateBase("views:\n"); err == nil || !strings.Contains(err.Error(), "no 'views'") {
		t.Fatalf("null views = %v, want the no-views refusal", err)
	}
}

// Review finding: aliases and merge keys are valid YAML that loads, and were
// refused because the validator looked at the alias node itself.
func TestValidateBaseFollowsAliasesAndMergeKeys(t *testing.T) {
	cases := map[string]string{
		"aliased views":   "x: &v\n  - type: table\nviews: *v\n",
		"aliased filters": "f: &f\n  and: ['a']\nfilters: *f\nviews:\n  - type: table\n",
		"merged view":     "d: &d\n  type: table\nviews:\n  - <<: *d\n    name: V\n",
		"merge list":      "a: &a {type: table}\nb: &b {name: V}\nviews:\n  - <<: [*b, *a]\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateBase(body); err != nil {
				t.Fatalf("ValidateBase refused valid YAML: %v", err)
			}
		})
	}
}

// Review finding: parsing is roughly a hundred times the input in memory and
// runs inside the write lock, so the size is capped before parsing.
func TestValidateBaseCapsSizeBeforeParsing(t *testing.T) {
	big := "views:\n  - type: table\nx: [" + strings.Repeat("a,", maxBaseBytes/2) + "a]\n"
	_, err := ValidateBase(big)
	if err == nil || !strings.Contains(err.Error(), "KB") {
		t.Fatalf("oversized base = %v, want a size refusal", err)
	}
}
