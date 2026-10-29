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
)

var (
	errSendAsUnknown    = errors.New("not a Send mail as address on this account")
	errSendAsUnverified = errors.New("a Send mail as address, but Gmail has not verified it")
	errBadInput         = errors.New("invalid input")
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
func resolveSendAs(list []sendAs, from string) (sendAs, error) {
	if from == "" {
		for _, s := range list {
			if s.IsDefault {
				return s, nil
			}
		}
		for _, s := range list {
			if s.IsPrimary {
				return s, nil
			}
		}
		return sendAs{}, errors.New("this account reports no default send-as address")
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

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("gmail api: %d %s", e.Status, e.Message) }

func (g *gmailClient) call(ctx context.Context, method, path string, body, out any) error {
	tok, err := g.tokens.Get(ctx)
	if err != nil {
		return err
	}
	var rd *bytes.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
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
	return &searchResult{Threads: out, NextPageToken: list.NextPageToken}, nil
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
	Attachments []attachment `json:"attachments"`
}

type threadView struct {
	ID       string        `json:"id"`
	Messages []messageView `json:"messages"`
}

func (g *gmailClient) readThread(ctx context.Context, id string) (*threadView, error) {
	if err := checkID("thread_id", id); err != nil {
		return nil, err
	}
	var th gmailThread
	if err := g.call(ctx, http.MethodGet, "/threads/"+id+"?format=full", nil, &th); err != nil {
		return nil, err
	}
	v := &threadView{ID: th.ID, Messages: []messageView{}}
	for _, m := range th.Messages {
		body, atts := extractBody(m.Payload)
		if r := []rune(body); len(r) > maxBodyChars {
			body = string(r[:maxBodyChars]) + "\n[truncated]"
		}
		hs := m.Payload.Headers
		v.Messages = append(v.Messages, messageView{
			ID: m.ID, From: header(hs, "From"), To: header(hs, "To"), Cc: header(hs, "Cc"),
			Date: header(hs, "Date"), Subject: header(hs, "Subject"),
			Labels: m.LabelIDs, Body: body, Attachments: atts,
		})
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
		msg.InReplyTo = header(orig.Payload.Headers, "Message-ID")
		msg.References = strings.TrimSpace(header(orig.Payload.Headers, "References") + " " + msg.InReplyTo)
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
}

func (g *gmailClient) send(ctx context.Context, p *prepared) (*sendResult, error) {
	var r struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	if err := g.call(ctx, http.MethodPost, "/messages/send", rawMessage{Raw: p.raw, ThreadID: p.threadID}, &r); err != nil {
		return nil, err
	}
	return &sendResult{MessageID: r.ID, ThreadID: r.ThreadID, From: p.from}, nil
}
