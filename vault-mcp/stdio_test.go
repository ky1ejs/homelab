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

// The agent surface is deliberately one tool wide.
//
// vault-claude already has Read, Write, Edit, Glob and Grep over the whole
// vault. Serving it edit_note as well would give the model a second way to
// write a note — one that fires no PostToolUse hook, so those writes would
// reach the vault unstamped while looking to the model like any other edit.
// Adding a tool here is therefore a decision about the stamp contract, not a
// convenience. See obsidian-vault/DECISIONS.md#giving-the-agent-a-move.
func TestStdioSurfaceServesOnlyMoveNote(t *testing.T) {
	got := toolNames(t, newSurface(t, true))
	if len(got) != 1 || got[0] != "move_file" {
		t.Fatalf("stdio tools = %v, want [move_file] exactly", got)
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
	got := strings.Join(toolNames(t, newSurface(t, false)), " ")
	for _, want := range []string{
		"append_note", "capture_note", "create_note", "edit_note",
		"list_notes", "move_file", "read_note", "search_notes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("voice surface is missing %s (has: %s)", want, got)
		}
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
