package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func resultText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// An unexpected error reaches the model as a fixed message, not as whatever
// the upstream error said.
func TestFailHidesUnexpectedErrors(t *testing.T) {
	tl := &tools{log: quietLogger()}
	r, _, err := tl.fail(context.Background(), "search_threads", errors.New("upstream said: from:secret-sender"))
	if err != nil || r == nil || !r.IsError || strings.Contains(resultText(r), "secret") {
		t.Errorf("r = %+v, err = %v", r, err)
	}
}

func TestFailTellsTheModelNotToResend(t *testing.T) {
	tl := &tools{log: quietLogger()}
	r, _, _ := tl.fail(context.Background(), "send_message", fmt.Errorf("%w: timeout", errMaybeSent))
	if r == nil || !strings.Contains(resultText(r), "Do not send again") {
		t.Errorf("r = %+v", r)
	}
}

// A send Gmail rewrote is reported as sent, with a warning, never as a
// refusal the model might answer by sending again.
func TestSendToolWarnsOnRewrittenFrom(t *testing.T) {
	f := newFakeGmail(t)
	f.sentFrom = "Me <me@example.com>"
	tl := &tools{gmail: f.client(), log: quietLogger()}
	r, _, err := tl.send(context.Background(), nil, composeInput{To: []string{"a@example.com"}, Subject: "x", Body: "x", From: "alias@example.org"})
	if err != nil || r == nil || r.IsError {
		t.Fatalf("r = %+v, err = %v", r, err)
	}
	if txt := resultText(r); !strings.Contains(txt, "warning") || !strings.Contains(txt, "Do not send again") {
		t.Errorf("result = %s", txt)
	}
}

// The draft tool refuses an unverified alias the same way send does.
func TestDraftToolRefusesUnverifiedAlias(t *testing.T) {
	f := newFakeGmail(t)
	tl := &tools{gmail: f.client(), log: quietLogger()}
	r, _, err := tl.draft(context.Background(), nil, composeInput{To: []string{"a@example.com"}, Subject: "x", Body: "x", From: "pending@example.net"})
	if err != nil || r == nil || !r.IsError || f.called("/drafts") {
		t.Errorf("r = %+v, err = %v, drafted = %v", r, err, f.called("/drafts"))
	}
}
