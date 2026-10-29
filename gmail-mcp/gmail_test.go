package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestResolveSendAs(t *testing.T) {
	cases := []struct {
		from, want string
		err        error
	}{
		{"", "me@example.com", nil},
		{"alias@example.org", "alias@example.org", nil},
		{"ALIAS@Example.org", "alias@example.org", nil},
		{"me@example.com", "me@example.com", nil},
		{"pending@example.net", "", errSendAsUnverified},
		{"stranger@example.com", "", errSendAsUnknown},
	}
	for _, c := range cases {
		got, err := resolveSendAs(testSendAs, c.from)
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("from %q: err = %v, want %v", c.from, err, c.err)
			}
			continue
		}
		if err != nil || got.SendAsEmail != c.want {
			t.Errorf("from %q: got %q, %v; want %q", c.from, got.SendAsEmail, err, c.want)
		}
	}
}

func TestSearchAndRead(t *testing.T) {
	f := newFakeGmail(t)
	g := f.client()
	ctx := context.Background()

	r, err := g.searchThreads(ctx, "from:sam", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Threads) != 1 || r.NextPageToken != "next" {
		t.Fatalf("search = %+v", r)
	}
	got := r.Threads[0]
	if got.Subject != "Thanksgiving" || got.From != "Alex <alex@example.com>" || got.MessageCount != 2 || !got.Unread {
		t.Errorf("summary = %+v", got)
	}

	th, err := g.readThread(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if th.Messages[0].Body != "Are you coming? \U0001F983" {
		t.Errorf("plain body = %q", th.Messages[0].Body)
	}
	if th.Messages[1].Body != "See attached menu\nThanks & bye" {
		t.Errorf("html body = %q", th.Messages[1].Body)
	}
	if len(th.Messages[1].Attachments) != 1 || th.Messages[1].Attachments[0].Filename != "menu.pdf" {
		t.Errorf("attachments = %+v", th.Messages[1].Attachments)
	}
}

func TestIDsCannotSteerThePath(t *testing.T) {
	f := newFakeGmail(t)
	for _, id := range []string{"../settings/sendAs", "t1?format=raw", "t1/../x", ""} {
		if _, err := f.client().readThread(context.Background(), id); !errors.Is(err, errBadInput) {
			t.Errorf("readThread(%q) err = %v, want errBadInput", id, err)
		}
	}
	if f.called("/settings/sendAs") {
		t.Error("a crafted id reached another endpoint")
	}
}

func TestSendFromVerifiedAlias(t *testing.T) {
	f := newFakeGmail(t)
	g := f.client()
	ctx := context.Background()
	p, err := g.prepare(ctx, compose{To: []string{"friend@example.com"}, Subject: "Hi", Body: "Hello", From: "alias@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.send(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.From != "alias@example.org" || !p.alias {
		t.Errorf("result = %+v alias=%v", r, p.alias)
	}
	raw, _ := f.sent(t, "/messages/send")
	h, body := decodeBody(t, raw)
	if !strings.Contains(h, `From: "Me Alias" <alias@example.org>`) {
		t.Errorf("headers = %q", h)
	}
	if body != "Hello" {
		t.Errorf("body = %q", body)
	}
}

// The property the whole server exists for: an address Gmail would silently
// replace is refused, and no send or draft request is ever made.
func TestRefusedFromNeverReachesGmail(t *testing.T) {
	for _, from := range []string{"pending@example.net", "stranger@example.com"} {
		f := newFakeGmail(t)
		_, err := f.client().prepare(context.Background(), compose{To: []string{"a@example.com"}, Subject: "x", Body: "x", From: from})
		if !errors.Is(err, errSendAsUnknown) && !errors.Is(err, errSendAsUnverified) {
			t.Errorf("from %q: err = %v", from, err)
		}
		if f.called("/messages/send") || f.called("/drafts") {
			t.Errorf("from %q: a send or draft request was made", from)
		}
	}
}

func TestReplyIsThreaded(t *testing.T) {
	f := newFakeGmail(t)
	g := f.client()
	ctx := context.Background()
	p, err := g.prepare(ctx, compose{To: []string{"alex@example.com"}, Body: "Count us in", ReplyToMessageID: "m2"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := g.createDraft(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if d.ThreadID != "t1" || d.From != "me@example.com" {
		t.Errorf("draft = %+v", d)
	}
	raw, threadID := f.sent(t, "/drafts")
	if threadID != "t1" {
		t.Errorf("threadId = %q", threadID)
	}
	h, _ := decodeBody(t, raw)
	for _, want := range []string{
		"Subject: Re: Thanksgiving",
		"In-Reply-To: <orig-2@example.com>",
		"References: <orig-1@example.com> <orig-2@example.com>",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %q in %q", want, h)
		}
	}
}

func TestBadRecipientsRefused(t *testing.T) {
	f := newFakeGmail(t)
	for _, to := range []string{
		"a@example.com\r\nBcc: evil@example.com",
		"Alice <a@example.com>",
		"a@example.com, b@example.com",
		"not-an-address",
	} {
		_, err := f.client().prepare(context.Background(), compose{To: []string{to}, Subject: "x", Body: "x"})
		if !errors.Is(err, errBadInput) {
			t.Errorf("to %q: err = %v, want errBadInput", to, err)
		}
	}
	if f.called("/settings/sendAs") {
		t.Error("recipients should be checked before any API call")
	}
}

func TestSubjectRequiredUnlessReplying(t *testing.T) {
	f := newFakeGmail(t)
	_, err := f.client().prepare(context.Background(), compose{To: []string{"a@example.com"}, Body: "x"})
	if !errors.Is(err, errBadInput) {
		t.Errorf("err = %v", err)
	}
}
