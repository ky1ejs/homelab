package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

const (
	gmailAPI     = "https://gmail.googleapis.com/gmail/v1/users/me"
	maxBodyChars = 20000
	// Per thread, across all its messages. Long threads quote each other, so
	// a per-message cap alone still lets one read return megabytes.
	maxThreadChars = 100000
	// Message-IDs kept in References: the root plus the most recent ones,
	// which is what RFC 5322 section 3.6.4 suggests trimming to.
	maxReferences = 10
)

var (
	errSendAsUnknown    = errors.New("not a Send mail as address on this account")
	errSendAsUnverified = errors.New("a Send mail as address, but Gmail has not verified it")
	errBadInput         = errors.New("invalid input")
	// A send whose outcome is unknown. Retrying it can send the mail twice.
	errMaybeSent = errors.New("the send may or may not have gone through")
	// Gmail put a different From on the sent copy than the one requested.
	errFromRewritten = errors.New("Gmail sent the message from a different address than requested")
)

type sendAs struct {
	SendAsEmail        string `json:"sendAsEmail"`
	DisplayName        string `json:"displayName"`
	IsPrimary          bool   `json:"isPrimary"`
	IsDefault          bool   `json:"isDefault"`
	VerificationStatus string `json:"verificationStatus"`
}

func (s sendAs) usable() bool { return s.IsPrimary || s.VerificationStatus == "accepted" }

// resolveSendAs is the whole point of this server.
//
// Gmail's own behaviour, given a From it does not recognise, is to quietly
// rewrite it to the primary address and send anyway. That is the worst
// outcome available: the mail goes, from the identity you were trying not to
// use, and nothing tells you. So the address is checked against the live
// sendAs list on every call -- not a cached copy, which would let an alias
// you have since removed keep working -- and anything that is neither the
// primary nor verified is refused before Gmail is asked to do anything.
//
// The default goes through the same check as a named address: an unverified
// default would otherwise be the one path round the rule. If the default is
// not usable, the primary is used instead, which is always usable.
func resolveSendAs(list []sendAs, from string) (sendAs, error) {
	if from == "" {
		for _, s := range list {
			if s.IsDefault && s.usable() {
				return s, nil
			}
		}
		for _, s := range list {
			if s.IsPrimary {
				return s, nil
			}
		}
		return sendAs{}, fmt.Errorf("%w: this account reports no usable default address", errSendAsUnknown)
	}
	for _, s := range list {
		if strings.EqualFold(s.SendAsEmail, strings.TrimSpace(from)) {
			if !s.usable() {
				return sendAs{}, fmt.Errorf("%s: %w (status %q)", from, errSendAsUnverified, s.VerificationStatus)
			}
			return s, nil
		}
	}
	return sendAs{}, fmt.Errorf("%s: %w", from, errSendAsUnknown)
}

type gmailMessage struct {
	ID       string    `json:"id"`
	ThreadID string    `json:"threadId"`
	LabelIDs []string  `json:"labelIds"`
	Snippet  string    `json:"snippet"`
	Payload  gmailPart `json:"payload"`
}

type gmailThread struct {
	ID       string         `json:"id"`
	Messages []gmailMessage `json:"messages"`
}

type gmailClient struct {
	base   string // overridden in tests
	http   *http.Client
	tokens interface {
		Get(context.Context) (string, error)
	}
}

// invalidator is implemented by token sources that cache: a 401 means the
// cached access token was revoked early, so it must not be served again.
type invalidator interface{ Invalidate() }

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("gmail api: %d %s", e.Status, e.Message) }

// transportError is a failed request with the URL taken out. A *url.Error
// prints the whole request URL, and for a search that carries the query, so
// passing one up would put the query in the log and in the tool's reply.
type transportError struct {
	Method, Path string // path only, never the query
	Err          error
}

func (e *transportError) Error() string { return e.Method + " " + e.Path + ": " + e.Err.Error() }
func (e *transportError) Unwrap() error { return e.Err }

func (g *gmailClient) call(ctx context.Context, method, path string, body, out any) error {
	err := g.do(ctx, method, path, body, out)
	var api *apiError
	if errors.As(err, &api) && api.Status == http.StatusUnauthorized {
		if inv, ok := g.tokens.(invalidator); ok {
			inv.Invalidate()
			return g.do(ctx, method, path, body, out)
		}
	}
	return err
}

func (g *gmailClient) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := g.tokens.Get(ctx)
	if err != nil {
		return err
	}
	var buf []byte
	if body != nil {
		if buf, err = json.Marshal(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		p, _, _ := strings.Cut(path, "?")
		return &transportError{Method: method, Path: p, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return &apiError{Status: resp.StatusCode, Message: e.Error.Message}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Gmail ids are hex; anything else is refused rather than escaped, so a
// caller cannot steer a request to a different API path.
var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func checkID(field, id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("%w: %s is not a Gmail id", errBadInput, field)
	}
	return nil
}

type threadSummary struct {
	ID           string `json:"id"`
	Subject      string `json:"subject"`
	From         string `json:"from"`
	Date         string `json:"date"`
	MessageCount int    `json:"message_count"`
	Snippet      string `json:"snippet"`
	Unread       bool   `json:"unread"`
}

type searchResult struct {
	Threads       []threadSummary `json:"threads"`
	NextPageToken string          `json:"next_page_token,omitempty"`
}

func (g *gmailClient) searchThreads(ctx context.Context, query string, max int, pageToken string) (*searchResult, error) {
	q := url.Values{"q": {query}, "maxResults": {fmt.Sprint(max)}}
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	var list struct {
		Threads []struct {
			ID string `json:"id"`
		} `json:"threads"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := g.call(ctx, http.MethodGet, "/threads?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}

	meta := url.Values{"format": {"metadata"}, "metadataHeaders": {"Subject", "From", "Date"}}.Encode()
	out := make([]threadSummary, len(list.Threads))
	errs := make([]error, len(list.Threads))
	sem := make(chan struct{}, 5) // Gmail's per-user rate limit is generous, but not infinite
	var wg sync.WaitGroup
	for i, t := range list.Threads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := checkID("thread id", t.ID); err != nil {
				errs[i] = err
				return
			}
			var th gmailThread
			if err := g.call(ctx, http.MethodGet, "/threads/"+t.ID+"?"+meta, nil, &th); err != nil {
				// Deleted between the list and this read: leave it out rather
				// than fail the whole search.
				var api *apiError
				if errors.As(err, &api) && api.Status == http.StatusNotFound {
					return
				}
				errs[i] = err
				return
			}
			s := threadSummary{ID: t.ID, MessageCount: len(th.Messages)}
			if n := len(th.Messages); n > 0 {
				first, last := th.Messages[0], th.Messages[n-1]
				s.Subject = header(first.Payload.Headers, "Subject")
				s.From = header(last.Payload.Headers, "From")
				s.Date = header(last.Payload.Headers, "Date")
				s.Snippet = last.Snippet
			}
			for _, m := range th.Messages {
				for _, l := range m.LabelIDs {
					if l == "UNREAD" {
						s.Unread = true
					}
				}
			}
			out[i] = s
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	found := out[:0]
	for _, s := range out {
		if s.ID != "" {
			found = append(found, s)
		}
	}
	return &searchResult{Threads: found, NextPageToken: list.NextPageToken}, nil
}

type messageView struct {
	ID          string       `json:"id"`
	From        string       `json:"from"`
	To          string       `json:"to"`
	Cc          string       `json:"cc,omitempty"`
	Date        string       `json:"date"`
	Subject     string       `json:"subject"`
	Labels      []string     `json:"labels"`
	Body        string       `json:"body"`
	BodyOmitted bool         `json:"body_omitted,omitempty"` // over the thread budget: snippet only
	Snippet     string       `json:"snippet,omitempty"`
	Attachments []attachment `json:"attachments"`
}

type threadView struct {
	ID       string        `json:"id"`
	Messages []messageView `json:"messages"`
	// True when older messages were cut to a snippet to keep the reply under
	// maxThreadChars. The newest messages are always the ones kept in full.
	Truncated bool `json:"truncated,omitempty"`
}

func (g *gmailClient) readThread(ctx context.Context, id string) (*threadView, error) {
	if err := checkID("thread_id", id); err != nil {
		return nil, err
	}
	var th gmailThread
	if err := g.call(ctx, http.MethodGet, "/threads/"+id+"?format=full", nil, &th); err != nil {
		return nil, err
	}
	v := &threadView{ID: th.ID, Messages: make([]messageView, len(th.Messages))}
	budget := maxThreadChars
	// Newest first, so the budget goes to the messages most likely to matter.
	for i := len(th.Messages) - 1; i >= 0; i-- {
		m := th.Messages[i]
		body, atts := extractBody(m.Payload)
		if r := []rune(body); len(r) > maxBodyChars {
			body = string(r[:maxBodyChars]) + "\n[truncated]"
		}
		hs := m.Payload.Headers
		mv := messageView{
			ID: m.ID, From: header(hs, "From"), To: header(hs, "To"), Cc: header(hs, "Cc"),
			Date: header(hs, "Date"), Subject: header(hs, "Subject"),
			Labels: m.LabelIDs, Attachments: atts,
		}
		if n := len([]rune(body)); n <= budget {
			mv.Body, budget = body, budget-n
		} else {
			mv.BodyOmitted, mv.Snippet, v.Truncated = true, m.Snippet, true
			budget = 0
		}
		v.Messages[i] = mv
	}
	return v, nil
}

func (g *gmailClient) listSendAs(ctx context.Context) ([]sendAs, error) {
	var r struct {
		SendAs []sendAs `json:"sendAs"`
	}
	if err := g.call(ctx, http.MethodGet, "/settings/sendAs", nil, &r); err != nil {
		return nil, err
	}
	return r.SendAs, nil
}

type compose struct {
	To, Cc, Bcc      []string
	Subject, Body    string
	From             string
	ReplyToMessageID string
}

type prepared struct {
	raw      string
	threadID string
	from     string
	alias    bool // sent from something other than the default address
}

// prepare validates everything and builds the raw message, so a draft and a
// send take exactly the same path up to the final API call.
func (g *gmailClient) prepare(ctx context.Context, c compose) (*prepared, error) {
	if len(c.To) == 0 {
		return nil, fmt.Errorf("%w: to needs at least one address", errBadInput)
	}
	to, err := bareAddresses("to", c.To)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadInput, err)
	}
	cc, err := bareAddresses("cc", c.Cc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadInput, err)
	}
	bcc, err := bareAddresses("bcc", c.Bcc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadInput, err)
	}

	list, err := g.listSendAs(ctx)
	if err != nil {
		return nil, err
	}
	sender, err := resolveSendAs(list, c.From)
	if err != nil {
		return nil, err
	}

	msg := outgoing{From: mail.Address{Name: sender.DisplayName, Address: sender.SendAsEmail}, To: to, Cc: cc, Bcc: bcc, Subject: c.Subject, Body: c.Body}
	// The primary address often has no displayName in sendAs, which Gmail
	// documents as "use the account's name". A From header written here would
	// go out as a bare address instead, so for the default with no name the
	// header is left to Gmail, which fills in the default with that name.
	if sender.IsDefault && sender.DisplayName == "" {
		msg.OmitFrom = true
	}
	p := &prepared{from: sender.SendAsEmail, alias: !sender.IsDefault}

	if c.ReplyToMessageID != "" {
		if err := checkID("reply_to_message_id", c.ReplyToMessageID); err != nil {
			return nil, err
		}
		q := url.Values{"format": {"metadata"}, "metadataHeaders": {"Subject", "Message-ID", "References"}}.Encode()
		var orig gmailMessage
		if err := g.call(ctx, http.MethodGet, "/messages/"+c.ReplyToMessageID+"?"+q, nil, &orig); err != nil {
			return nil, err
		}
		p.threadID = orig.ThreadID
		// Both headers come from a stranger's mail, so only well-formed
		// msg-ids are carried over, and References is trimmed to a bounded
		// length rather than copied whole.
		if ids := msgIDs(header(orig.Payload.Headers, "Message-ID")); len(ids) > 0 {
			msg.InReplyTo = ids[len(ids)-1]
		}
		msg.References = strings.Join(trimReferences(append(msgIDs(header(orig.Payload.Headers, "References")), msg.InReplyTo)), " ")
		if msg.Subject == "" {
			s := header(orig.Payload.Headers, "Subject")
			if !strings.HasPrefix(strings.ToLower(s), "re:") {
				s = "Re: " + s
			}
			msg.Subject = s
		}
	}
	if msg.Subject == "" {
		return nil, fmt.Errorf("%w: subject is required unless replying", errBadInput)
	}

	raw, err := msg.build()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBadInput, err)
	}
	p.raw = base64.URLEncoding.EncodeToString([]byte(raw))
	return p, nil
}

type rawMessage struct {
	Raw      string `json:"raw"`
	ThreadID string `json:"threadId,omitempty"`
}

type draftResult struct {
	DraftID   string `json:"draft_id"`
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id"`
	From      string `json:"from"`
}

func (g *gmailClient) createDraft(ctx context.Context, p *prepared) (*draftResult, error) {
	var r struct {
		ID      string `json:"id"`
		Message struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"message"`
	}
	body := map[string]rawMessage{"message": {Raw: p.raw, ThreadID: p.threadID}}
	if err := g.call(ctx, http.MethodPost, "/drafts", body, &r); err != nil {
		return nil, err
	}
	return &draftResult{DraftID: r.ID, MessageID: r.Message.ID, ThreadID: r.Message.ThreadID, From: p.from}, nil
}

type sendResult struct {
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id"`
	From      string `json:"from"`
	// The sent copy could not be read back, so From is the address requested.
	FromUnconfirmed bool `json:"from_unconfirmed,omitempty"`
}

// send reports the From that Gmail actually put on the sent copy, not the one
// requested. Between the sendAs check and the send an alias can lose its
// verification, and Gmail's answer to that is to send from the default
// without saying so; reading the sent message back is the only way to know.
func (g *gmailClient) send(ctx context.Context, p *prepared) (*sendResult, error) {
	var r struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	if err := g.call(ctx, http.MethodPost, "/messages/send", rawMessage{Raw: p.raw, ThreadID: p.threadID}, &r); err != nil {
		// Only a 4xx is a definite refusal. A timeout, a reset or a 5xx can
		// arrive after Gmail accepted the message.
		var api *apiError
		if errors.As(err, &api) && api.Status/100 == 4 {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", errMaybeSent, err)
	}
	res := &sendResult{MessageID: r.ID, ThreadID: r.ThreadID, From: p.from}
	if checkID("message id", r.ID) != nil {
		res.FromUnconfirmed = true
		return res, nil
	}
	var sent gmailMessage
	q := url.Values{"format": {"metadata"}, "metadataHeaders": {"From"}}.Encode()
	if err := g.call(ctx, http.MethodGet, "/messages/"+r.ID+"?"+q, nil, &sent); err != nil {
		res.FromUnconfirmed = true
		return res, nil
	}
	got, err := mail.ParseAddress(header(sent.Payload.Headers, "From"))
	if err != nil {
		res.FromUnconfirmed = true
		return res, nil
	}
	res.From = strings.ToLower(got.Address)
	if !strings.EqualFold(got.Address, p.from) {
		return res, fmt.Errorf("%w: asked for %s, sent as %s", errFromRewritten, p.from, got.Address)
	}
	return res, nil
}

var msgIDRe = regexp.MustCompile(`<[^<>\s@]+@[^<>\s@]+>`)

// msgIDs extracts the well-formed message ids from a header value and drops
// everything else.
func msgIDs(v string) []string {
	ids := msgIDRe.FindAllString(v, -1)
	out := ids[:0]
	for _, id := range ids {
		if len(id) <= 250 {
			out = append(out, id)
		}
	}
	return out
}

// trimReferences keeps the thread's root and the most recent ids.
func trimReferences(ids []string) []string {
	var clean []string
	for _, id := range ids {
		if id != "" {
			clean = append(clean, id)
		}
	}
	if len(clean) <= maxReferences {
		return clean
	}
	return append([]string{clean[0]}, clean[len(clean)-(maxReferences-1):]...)
}
