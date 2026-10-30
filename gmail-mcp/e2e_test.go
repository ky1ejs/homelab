package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// TestEndToEnd drives the real handler with the go-sdk client: a token from
// the stand-in authorization server, the stand-in Gmail behind it.
func TestEndToEnd(t *testing.T) {
	as := newFakeAS(t)
	g := newFakeGmail(t)
	srv := testServer(t, as, g)
	ctx := context.Background()

	token := as.mint(t, "user_kyle", testResource, time.Now().Add(time.Hour))
	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}},
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	want := []string{"create_draft", "list_send_as", "read_thread", "search_threads", "send_message"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"to": []string{"friend@example.com"}, "subject": "Hi", "body": "Hello", "from": "alias@example.org",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("send refused: %s", text(res))
	}
	raw, _ := g.sent(t, "/messages/send")
	if !strings.Contains(raw, "From: \"Me Alias\" <alias@example.org>") {
		t.Errorf("raw = %q", raw)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"to": []string{"friend@example.com"}, "subject": "Hi", "body": "Hello", "from": "pending@example.net",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(res), "verified") {
		t.Errorf("unverified alias: IsError=%v %q", res.IsError, text(res))
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_send_as"})
	if err != nil || res.IsError {
		t.Fatalf("list_send_as: %v %s", err, text(res))
	}
	var aliases []map[string]any
	_ = json.Unmarshal([]byte(text(res)), &aliases)
	if len(aliases) != 3 {
		t.Errorf("list_send_as = %s", text(res))
	}
}

func text(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
