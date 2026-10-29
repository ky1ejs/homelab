package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"mime"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Building outgoing messages, and turning Gmail's parsed payloads into text.

var errHeaderBreak = errors.New("header values must not contain line breaks")

// bareAddress accepts exactly one plain address: no display name, no group, no
// comment. Names are refused rather than parsed because "Alice <a@x>, b@y" is
// two recipients to one parser and one to another, and the only thing worse
// than refusing a recipient is mailing one nobody saw.
func bareAddress(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", errHeaderBreak
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s || !strings.Contains(s[strings.LastIndex(s, "@")+1:], ".") {
		return "", fmt.Errorf("%q is not a plain email address", s)
	}
	return a.Address, nil
}

func bareAddresses(field string, in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, s := range in {
		a, err := bareAddress(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		out = append(out, a)
	}
	return out, nil
}

type outgoing struct {
	From       mail.Address
	To, Cc     []string
	Bcc        []string
	Subject    string
	Body       string
	InReplyTo  string
	References string
}

// build returns an RFC 5322 message. The body is always base64 UTF-8 plain
// text, which sidesteps line-length and 8-bit transport questions entirely.
//
// Bcc goes in the raw message on purpose: the Gmail API delivers to it and
// strips the header from the copies recipients see.
func (m outgoing) build() (string, error) {
	for _, v := range []string{m.Subject, m.InReplyTo, m.References, m.From.Name} {
		if strings.ContainsAny(v, "\r\n\x00") {
			return "", errHeaderBreak
		}
	}
	var b strings.Builder
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", m.From.String()) // quotes or RFC 2047-encodes the name as needed
	h("To", strings.Join(m.To, ", "))
	if len(m.Cc) > 0 {
		h("Cc", strings.Join(m.Cc, ", "))
	}
	if len(m.Bcc) > 0 {
		h("Bcc", strings.Join(m.Bcc, ", "))
	}
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	if m.InReplyTo != "" {
		h("In-Reply-To", m.InReplyTo)
	}
	if m.References != "" {
		h("References", m.References)
	}
	h("MIME-Version", "1.0")
	h("Content-Type", `text/plain; charset="UTF-8"`)
	h("Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")

	body := strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n")
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String(), nil
}

// --- reading ----------------------------------------------------------------

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailPart struct {
	MimeType string        `json:"mimeType"`
	Filename string        `json:"filename"`
	Headers  []gmailHeader `json:"headers"`
	Body     struct {
		Data         string `json:"data"`
		Size         int    `json:"size"`
		AttachmentID string `json:"attachmentId"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

func header(hs []gmailHeader, name string) string {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

var charsetRe = regexp.MustCompile(`(?i)charset="?([^";\s]+)`)

// decodePart returns a part's text. Go's standard library decodes UTF-8 only,
// and pulling in x/text for every legacy charset is not worth a dependency on
// the internet-facing binary: Latin-1 is mapped by hand because it is common
// and trivial, and anything else is made valid UTF-8 rather than guessed at.
func decodePart(p gmailPart) string {
	if p.Body.Data == "" {
		return ""
	}
	raw, err := base64.URLEncoding.DecodeString(p.Body.Data)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(p.Body.Data)
		if err != nil {
			return ""
		}
	}
	cs := ""
	if m := charsetRe.FindStringSubmatch(header(p.Headers, "Content-Type")); m != nil {
		cs = strings.ToLower(m[1])
	}
	if (cs == "iso-8859-1" || cs == "latin1") && !utf8.Valid(raw) {
		runes := make([]rune, len(raw))
		for i, c := range raw {
			runes[i] = rune(c)
		}
		return string(runes)
	}
	return strings.ToValidUTF8(string(raw), "�")
}

var (
	dropBlocks = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)>`)
	lineBreaks = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|h[1-6])>`)
	anyTag     = regexp.MustCompile(`<[^>]*>`)
	trailingWS = regexp.MustCompile(`[ \t]+\n`)
	manyBlank  = regexp.MustCompile(`\n{3,}`)
)

// htmlToText is a flattener, not a renderer: enough to read an HTML-only mail
// back, with scripts and styles dropped rather than shown.
func htmlToText(s string) string {
	s = dropBlocks.ReplaceAllString(s, "")
	s = lineBreaks.ReplaceAllString(s, "\n")
	s = anyTag.ReplaceAllString(s, "")
	s = strings.ReplaceAll(html.UnescapeString(s), " ", " ")
	s = trailingWS.ReplaceAllString(s, "\n")
	s = manyBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

type attachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int    `json:"size"`
}

// extractBody prefers text/plain and falls back to flattened HTML. Attachments
// are listed by name only: there is no tool that reads one.
func extractBody(p gmailPart) (string, []attachment) {
	var plain, htm []string
	atts := []attachment{}
	var walk func(gmailPart)
	walk = func(p gmailPart) {
		if p.Filename != "" {
			atts = append(atts, attachment{Filename: p.Filename, MimeType: p.MimeType, Size: p.Body.Size})
			return
		}
		switch p.MimeType {
		case "text/plain":
			plain = append(plain, decodePart(p))
		case "text/html":
			htm = append(htm, decodePart(p))
		}
		for _, c := range p.Parts {
			walk(c)
		}
	}
	walk(p)
	if len(plain) > 0 {
		return strings.TrimSpace(strings.Join(plain, "\n")), atts
	}
	return htmlToText(strings.Join(htm, "\n")), atts
}
