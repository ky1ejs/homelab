package main

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolNames lists what a surface actually offers, over a real MCP session
// rather than by reading the registration code back to itself.
func toolNames(t *testing.T, s *server) []string {
	t.Helper()
	ctx := context.Background()

	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := s.mcpServer().Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func newSurface(t *testing.T, stdio bool) *server {
	t.Helper()
	v, err := NewVault(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewVault: %v", err)
	}
	return &server{
		cfg:   &config{stdio: stdio},
		vault: v,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// The agent surface serves exactly the operations Claude Code has no tool for:
// a move, a delete of the two file types that may be deleted, and a folder
// removal. Nothing that WRITES a note.
//
// vault-claude already has Read, Write, Edit, Glob and Grep over the whole
// vault. Serving it edit_note as well would give the model a second way to
// write a note — one that fires no PostToolUse hook, so those writes would
// reach the vault unstamped while looking to the model like any other edit.
// Adding a tool here is therefore a decision about the stamp contract, not a
// convenience. The three below are not writes to a note: a move stamps the note
// itself, and neither removal produces content to attribute.
// See obsidian-vault/DECISIONS.md#giving-the-agent-a-move.
func TestStdioSurfaceServesTheFileOperationsAndNothingElse(t *testing.T) {
	got := toolNames(t, newSurface(t, true))
	want := []string{"delete_empty_folder", "move_file", "trash_file"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stdio tools = %v, want %v exactly", got, want)
	}
}

// vault-research gets one more tool, and only because nothing else can put a
// file on disk: WebFetch returns text, Write cannot produce binary, Read cannot
// open a PNG. The stamp argument above does not apply to it — fetch_attachment
// writes to the scratch volume, which is not the vault and is not snapshotted.
//
// Asserted as an exact list for the same reason the one above is: this surface
// is the one with an outbound connection, and a tool arriving here unnoticed is
// how that stops being a considered decision.
// See obsidian-vault/DECISIONS.md#fetching-attachments.
func TestStdioWithFetchServesExactlyTwoTools(t *testing.T) {
	s := newSurface(t, true)
	s.cfg.fetch = true
	s.fetch = newFetcher(time.Second, 1024, nil)

	got := toolNames(t, s)
	if len(got) != 2 || got[0] != "fetch_attachment" || got[1] != "move_file" {
		t.Fatalf("stdio+fetch tools = %v, want [fetch_attachment move_file] exactly", got)
	}
}

// The research surface gets no removal, and this is the assertion that says so
// rather than leaving it implicit in the exact list above.
//
// vault-research runs this same binary with VAULT_DIR=/scratch, so a delete
// tool there would remove things from the scratch volume — where
// scratch-sweep.sh is documented as the only thing that removes anything, and
// where an agent reading pages it did not write has no filing job to do. The
// gate is s.fetch, in Go, so a change to either .mcp.json cannot open it.
func TestFetchSurfaceNeverServesARemoval(t *testing.T) {
	s := newSurface(t, true)
	s.cfg.fetch = true
	s.fetch = newFetcher(time.Second, 1024, nil)

	for _, name := range toolNames(t, s) {
		if name == "trash_file" || name == "delete_empty_folder" {
			t.Fatalf("the research surface is serving %s; scratch-sweep.sh is the only thing that removes anything there", name)
		}
	}
}

// vault-claude's import tool, and the counterpart to fetch_attachment above: it
// appears only when a scratch root is configured, which only
// vault-claude-mcp.json does.
func TestStdioWithImportServesExactlyFourTools(t *testing.T) {
	s := newSurface(t, true)
	s.importVault = newTestVault(t)

	got := toolNames(t, s)
	want := []string{"delete_empty_folder", "import_attachment", "move_file", "trash_file"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stdio+import tools = %v, want %v exactly", got, want)
	}
}

// The two halves of the research handoff must never be on one surface. Together
// they are "reach the web, then write into the vault" in a single session,
// which is the combination the whole three-surface split exists to prevent.
// This test found that nothing in the CODE enforced the split — only the
// compose file and two .mcp.json files did, which is the sort of separation
// that lasts until someone consolidates a config. loadConfig now refuses the
// combination outright, so the enforcement is where it cannot be edited away by
// a deployment change.
func TestFetchAndImportAreNeverOnTheSameSurface(t *testing.T) {
	t.Setenv("MCP_FETCH", "1")
	t.Setenv("IMPORT_DIR", "/scratch")

	if _, err := loadConfig(true); err == nil {
		t.Fatal("a surface with both fetch_attachment and import_attachment loaded; it must be refused")
	}

	// Each alone still works, or the check is just breaking the feature.
	t.Setenv("IMPORT_DIR", "")
	if _, err := loadConfig(true); err != nil {
		t.Errorf("fetch alone should load: %v", err)
	}
	t.Setenv("MCP_FETCH", "0")
	t.Setenv("IMPORT_DIR", "/scratch")
	if _, err := loadConfig(true); err != nil {
		t.Errorf("import alone should load: %v", err)
	}
}

// The research agent's tool must never appear on the surface serving claude.ai.
// That client has web access of its own, and adding an outbound fetch to it
// would be a way out of a conversation that already has several — on the one
// surface here that cannot be given a tool policy.
func TestVoiceSurfaceNeverServesFetch(t *testing.T) {
	s := newSurface(t, false)
	// Force the field on, so this asserts the surface split rather than merely
	// re-testing that loadConfig refuses the combination.
	s.cfg.fetch = true
	s.fetch = newFetcher(time.Second, 1024, nil)

	for _, name := range toolNames(t, s) {
		if name == "fetch_attachment" {
			t.Fatal("the voice surface is serving fetch_attachment")
		}
	}
}

func TestVoiceSurfaceKeepsItsToolsAndGainsMove(t *testing.T) {
	got := toolNames(t, newSurface(t, false))
	// An exact list, like the stdio ones. This is the internet-reachable
	// surface and the only one whose client cannot be given a tool policy, so
	// "what does it serve" is a question that must be answered by a failing
	// test rather than by reading the registrations back.
	want := []string{
		"append_note", "capture_note", "create_note", "delete_empty_folder",
		"edit_note", "list_notes", "move_file", "read_note", "search_notes",
		"trash_file",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("voice tools = %v, want %v exactly", got, want)
	}
}

// stdio has no listener, so there is nothing to authenticate — but the check
// that enforces that on the HTTP surface is a refusal to start, and getting the
// exemption wrong in the other direction is how a public endpoint ends up
// unauthenticated. Both halves are asserted: stdio starts with no
// authorization server, and it does not quietly set allowNoAuth doing so.
func TestStdioNeedsNoAuthorizationServer(t *testing.T) {
	t.Setenv("OAUTH_ISSUER", "")
	t.Setenv("OAUTH_RESOURCE", "")
	t.Setenv("MCP_ALLOW_NO_AUTH", "")

	c, err := loadConfig(true)
	if err != nil {
		t.Fatalf("loadConfig(stdio) = %v, want a usable config", err)
	}
	if c.allowNoAuth {
		t.Error("stdio set allowNoAuth; it must stay an HTTP-only opt-out")
	}
}

// An OAuth configuration inherited from an .env shared with the connector must
// not follow the agent's local pipe: the verifier would then be built for a
// surface that never sees a request, and MCP_ALLOW_NO_AUTH would start looking
// like something you set to make stdio work.
func TestStdioIgnoresInheritedOAuthConfig(t *testing.T) {
	t.Setenv("OAUTH_ISSUER", "https://auth.example.com")
	t.Setenv("OAUTH_RESOURCE", "https://vault.example.ts.net/mcp")
	t.Setenv("OAUTH_ALLOWED_SUBJECTS", "user_01")

	c, err := loadConfig(true)
	if err != nil {
		t.Fatalf("loadConfig(stdio): %v", err)
	}
	if c.oauthIssuer != "" || c.oauthResource != "" {
		t.Errorf("stdio kept OAuth config: issuer=%q resource=%q", c.oauthIssuer, c.oauthResource)
	}
}

// MCP_EXCLUDE hides folders from the CONNECTOR, whose client has web access.
// The agent reads those folders with Read and Grep either way, so an exclusion
// here would hide nothing and only make move_file refuse on the Inbox — the
// folder the agent exists to triage. See vault-mcp/README.md#what-voice-cannot-see.
func TestStdioIgnoresExclusions(t *testing.T) {
	t.Setenv("MCP_EXCLUDE", "4. Inbox")

	c, err := loadConfig(true)
	if err != nil {
		t.Fatalf("loadConfig(stdio): %v", err)
	}
	if len(c.excludes) != 0 {
		t.Errorf("stdio applied exclusions %v; the agent surface has none", c.excludes)
	}

	if c, err = loadConfig(false); err == nil && len(c.excludes) != 1 {
		t.Errorf("the connector surface lost its exclusions: %v", c.excludes)
	}
}

// Who the vault records as the writer follows the surface, not the binary.
func TestStdioWritesUnderTheAgentIdentity(t *testing.T) {
	t.Setenv("MCP_STAMP_AGENT", "")
	t.Setenv("VAULT_AGENT_NAME", "")
	t.Setenv("AGENT_GIT_NAME", "")
	t.Setenv("AGENT_GIT_EMAIL", "")

	c, err := loadConfig(true)
	if err != nil {
		t.Fatalf("loadConfig(stdio): %v", err)
	}
	if c.stampAgent != "claude-agent" {
		t.Errorf("stamp agent = %q, want claude-agent", c.stampAgent)
	}
	if c.authorName != "Claude Code" {
		t.Errorf("snapshot author = %q, want Claude Code", c.authorName)
	}

	t.Setenv("VAULT_AGENT_NAME", "claude-vault")
	if c, err = loadConfig(true); err != nil {
		t.Fatalf("loadConfig(stdio): %v", err)
	} else if c.stampAgent != "claude-vault" {
		t.Errorf("stamp agent = %q, want the vault's VAULT_AGENT_NAME", c.stampAgent)
	}
}
