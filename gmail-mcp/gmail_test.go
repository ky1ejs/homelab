package main

import (
	"context"
	"errors"
	"fmt"
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

// The default goes through the same verification as a named alias, and an
// unverified one falls back to the primary rather than being used.
func TestResolveSendAsDefault(t *testing.T) {
	list := []sendAs{
		{SendAsEmail: "me@example.com", IsPrimary: true},
		{SendAsEmail: "alias@example.org", IsDefault: true, VerificationStatus: "pending"},
	}
	got, err := resolveSendAs(list, "")
	if err != nil || got.SendAsEmail != "me@example.com" {
		t.Errorf("unverified default: got %q, %v; want the primary", got.SendAsEmail, err)
	}
	if _, err := resolveSendAs(nil, ""); !errors.Is(err, errSendAsUnknown) {
		t.Errorf("empty list: err = %v", err)
	}
}

// A default with no display name leaves From to Gmail, which adds the name.
func TestNamelessDefaultLeavesFromToGmail(t *testing.T) {
	f := newFakeGmail(t)
	f.sendAs = []sendAs{{SendAsEmail: "me@example.com", IsPrimary: true, IsDefault: true}}
	p, err := f.client().prepare(context.Background(), compose{To: []string{"a@example.com"}, Subject: "x", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.client().send(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	raw, _ := f.sent(t, "/messages/send")
	if strings.Contains(raw, "From:") {
		t.Errorf("From written for a nameless default:\n%s", raw)
	}
}

// A thread deleted between the list and the read is skipped; a rate limit is
// an error the caller can name.
func TestSearchSkipsGoneThreads(t *testing.T) {
	f := newFakeGmail(t)
	f.searchIDs = []string{"t1", "t2", "t3", "gone", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "t11"}
	r, err := f.client().searchThreads(context.Background(), "x", 25, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Threads) != 11 {
		t.Errorf("got %d threads, want 11", len(r.Threads))
	}
	for _, th := range r.Threads {
		if th.ID == "" || th.ID == "gone" {
			t.Errorf("thread %+v in results", th)
		}
	}

	f.searchIDs = []string{"t1", "busy"}
	_, err = f.client().searchThreads(context.Background(), "x", 25, "")
	var api *apiError
	if !errors.As(err, &api) || api.Status != 429 {
		t.Errorf("err = %v, want a 429", err)
	}
}

// One read stays under the thread budget, newest messages kept in full.
func TestReadThreadBudget(t *testing.T) {
	th, err := newFakeGmail(t).client().readThread(context.Background(), "long")
	if err != nil {
		t.Fatal(err)
	}
	if !th.Truncated {
		t.Error("not marked truncated")
	}
	total := 0
	for i, m := range th.Messages {
		total += len(m.Body)
		full := i >= 3 // 5 x 19,000 fit in 100,000; the older 3 do not
		if full == m.BodyOmitted || (m.BodyOmitted && m.Snippet != fmt.Sprintf("snippet %d", i)) {
			t.Errorf("message %d: omitted=%v snippet=%q", i, m.BodyOmitted, m.Snippet)
		}
	}
	if total > maxThreadChars {
		t.Errorf("returned %d characters", total)
	}
}

// A 401 from Gmail drops the cached token and retries once.
func TestUnauthorizedRetriesWithFreshToken(t *testing.T) {
	f := newFakeGmail(t)
	tokens := &rotatingTokens{}
	g := &gmailClient{base: f.URL, http: f.Client(), tokens: tokens}
	if _, err := g.listSendAs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := tokens.invalidated.Load(); n != 1 {
		t.Errorf("invalidated %d times, want 1", n)
	}
}

// A failed request's error carries no part of the query: it goes to the log,
// and the query is the person's own text.
func TestTransportErrorHasNoQuery(t *testing.T) {
	f := newFakeGmail(t)
	g := f.client()
	f.Close()
	_, err := g.searchThreads(context.Background(), "from:secret-sender subject:biopsy", 10, "")
	if err == nil {
		t.Fatal("no error from a closed server")
	}
	for _, leak := range []string{"secret", "biopsy", "q="} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error contains %q: %v", leak, err)
		}
	}
}

// When Gmail sends from another address than asked, the result says so.
func TestSendReportsRewrittenFrom(t *testing.T) {
	f := newFakeGmail(t)
	f.sentFrom = "Me <me@example.com>"
	g := f.client()
	p, err := g.prepare(context.Background(), compose{To: []string{"a@example.com"}, Subject: "x", Body: "x", From: "alias@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.send(context.Background(), p)
	if !errors.Is(err, errFromRewritten) || r == nil || r.From != "me@example.com" {
		t.Errorf("r = %+v, err = %v", r, err)
	}
}

// Only a 4xx is a definite refusal; anything else may have been sent.
func TestSendOutcomeUnknown(t *testing.T) {
	for status, maybe := range map[int]bool{500: true, 503: true, 400: false, 403: false} {
		f := newFakeGmail(t)
		f.sendStatus = status
		g := f.client()
		p, err := g.prepare(context.Background(), compose{To: []string{"a@example.com"}, Subject: "x", Body: "x"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = g.send(context.Background(), p)
		if errors.Is(err, errMaybeSent) != maybe {
			t.Errorf("status %d: err = %v, maybe-sent = %v", status, err, maybe)
		}
	}
}

// References from a stranger's mail are reduced to well-formed ids and
// trimmed to the root plus the most recent.
func TestReferencesSanitized(t *testing.T) {
	junk := "<root@example.com> garbage (comment) <bad id@x> <" + strings.Repeat("a", 300) + "@example.com>"
	for i := range 20 {
		junk += fmt.Sprintf(" <m%d@example.com>", i)
	}
	ids := trimReferences(append(msgIDs(junk), "<new@example.com>"))
	if len(ids) != maxReferences || ids[0] != "<root@example.com>" || ids[len(ids)-1] != "<new@example.com>" {
		t.Errorf("ids = %v", ids)
	}
	for _, id := range ids {
		if strings.ContainsAny(id, " ()") || len(id) > 250 {
			t.Errorf("kept %q", id)
		}
	}
}

// Nothing is drafted or sent when the reply target is malformed or the alias
// list cannot be read.
func TestNoSendWithoutChecks(t *testing.T) {
	cases := map[string]struct {
		setup func(*fakeGmail)
		in    compose
	}{
		"bad reply id": {func(*fakeGmail) {}, compose{To: []string{"a@example.com"}, Body: "x", ReplyToMessageID: "../drafts"}},
		"sendAs fails": {func(f *fakeGmail) { f.sendAsStatus = 500 }, compose{To: []string{"a@example.com"}, Subject: "x", Body: "x"}},
	}
	for name, c := range cases {
		f := newFakeGmail(t)
		c.setup(f)
		if _, err := f.client().prepare(context.Background(), c.in); err == nil {
			t.Errorf("%s: prepared", name)
		}
		if f.called("/messages/send") || f.called("/drafts") {
			t.Errorf("%s: a send or draft request was made", name)
		}
	}
}
