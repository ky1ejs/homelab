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
//
// It also refuses non-ASCII (net/mail accepts it, but it goes into the header
// raw, which needs SMTPUTF8 end to end and reads as a lookalike when it is
// not), quoted local parts, and domain literals such as a@[10.0.0.1], which
// route by IP and bypass whatever the domain's MX would have done.
func bareAddress(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", errHeaderBreak
	}
	bad := fmt.Errorf("%q is not a plain email address", s)
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e || s[i] == '"' || s[i] == '[' || s[i] == ']' {
			return "", bad
		}
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s || !strings.Contains(s[strings.LastIndex(s, "@")+1:], ".") {
		return "", bad
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
	OmitFrom   bool // leave From to Gmail, which fills in the default with its name
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
	h := func(k, v string) { b.WriteString(fold(k + ": " + v)) }
	if !m.OmitFrom {
		h("From", m.From.String()) // quotes or RFC 2047-encodes the name as needed
	}
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

	// CRLF, lone CR and lone LF all become CRLF, so a stray CR cannot end up
	// as a bare carriage return in the decoded text.
	body := strings.NewReplacer("\r\n", "\r\n", "\r", "\r\n", "\n", "\r\n").Replace(m.Body)
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String(), nil
}

// fold wraps one header line at spaces so no line passes 78 characters, as
// RFC 5322 section 2.1.1 asks. Gmail accepts long lines, but a relay further
// on may not, and a long References or recipient list is where they come from.
// A word with no space in it longer than the limit is left whole: folding is
// only allowed at whitespace.
func fold(line string) string {
	const limit = 78
	var b strings.Builder
	for len(line) > limit {
		i := strings.LastIndexByte(line[:limit+1], ' ')
		if i <= 0 {
			i = strings.IndexByte(line[limit:], ' ')
			if i < 0 {
				break
			}
			i += limit
		}
		b.WriteString(line[:i] + "\r\n")
		line = line[i:] // keeps the space, which marks the continuation
	}
	b.WriteString(line + "\r\n")
	return b.String()
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
	if latin1Charsets[cs] && !utf8.Valid(raw) {
		runes := make([]rune, len(raw))
		for i, c := range raw {
			runes[i] = rune(c)
			// Mail labelled ISO-8859-1 is almost always Windows-1252 in
			// practice (WHATWG decodes the label that way too): 0x80-0x9F are
			// curly quotes and dashes there, and invisible controls in Latin-1.
			if c >= 0x80 && c <= 0x9f {
				runes[i] = cp1252[c-0x80]
			}
		}
		return string(runes)
	}
	return strings.ToValidUTF8(string(raw), "�")
}

var latin1Charsets = map[string]bool{"iso-8859-1": true, "latin1": true, "windows-1252": true, "cp1252": true, "us-ascii": true}

// Windows-1252 for 0x80-0x9F. The five bytes it leaves undefined map to
// themselves, as WHATWG's decoder does.
var cp1252 = [32]rune{
	'\u20ac', '\u0081', '\u201a', '\u0192', '\u201e', '\u2026', '\u2020', '\u2021',
	'\u02c6', '\u2030', '\u0160', '\u2039', '\u0152', '\u008d', '\u017d', '\u008f',
	'\u0090', '\u2018', '\u2019', '\u201c', '\u201d', '\u2022', '\u2013', '\u2014',
	'\u02dc', '\u2122', '\u0161', '\u203a', '\u0153', '\u009d', '\u017e', '\u0178',
}

var (
	comments   = regexp.MustCompile(`(?s)<!--.*?-->`)
	cellEnds   = regexp.MustCompile(`(?i)</t[dh]\s*>`)
	lineBreaks = regexp.MustCompile(`(?i)<br\b[^>]*>|<hr\b[^>]*>|</(p|div|li|tr|table|h[1-6]|blockquote|ul|ol|dl|dt|dd|section|article|header|footer|address|pre)\s*>`)
	anyTag     = regexp.MustCompile(`<[^>]*>`)
	spaceRuns  = regexp.MustCompile(`[ \t\f\v\r\x{00a0}]+`)
	manyBlank  = regexp.MustCompile(`\n{3,}`)
)

// Elements whose content a reader never sees. Each is cut from its opening
// tag to its own closing tag, or to the end of the document when the closing
// tag is missing: a single alternation would let <head> end at the first
// </style> or </title> inside it, and an unclosed <script> would leak.
var hiddenElements = []string{"script", "style", "head", "title", "template", "noscript"}

// cellSep marks a table cell's end until the text is laid out. It is in the
// Unicode private use area, and stripped from the input first, so mail
// cannot forge one.
const cellSep = "\ue000"

// htmlToText is a flattener, not a renderer: enough to read an HTML-only mail
// back, with scripts and styles dropped rather than shown. Table cells on one
// row are joined with " | ", which keeps a receipt's item, quantity and price
// readable as one line.
//
// Comments go first: text hidden in a comment is a common place to put
// instructions aimed at a model rather than the reader, and so is a <title>
// or <template> nobody sees rendered.
//
// Content-Transfer-Encoding is not handled here: the Gmail API has already
// undone quoted-printable and base64 by the time decodePart sees the data.
func htmlToText(s string) string {
	s = strings.ReplaceAll(s, cellSep, "")
	s = comments.ReplaceAllString(s, "")
	for _, tag := range hiddenElements {
		s = dropElement(s, tag)
	}
	// Line breaks in the source are only whitespace to HTML; the markup
	// says where lines end. (<pre> loses its layout, which mail rarely uses.)
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	s = flattenRows(s)
	s = cellEnds.ReplaceAllString(s, cellSep)
	s = lineBreaks.ReplaceAllString(s, "\n")
	s = anyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	var out []string
	for _, line := range strings.Split(s, "\n") {
		var cells []string
		for _, c := range strings.Split(line, cellSep) {
			if c = strings.TrimSpace(spaceRuns.ReplaceAllString(c, " ")); c != "" {
				cells = append(cells, c)
			}
		}
		out = append(out, strings.Join(cells, " | "))
	}
	s = manyBlank.ReplaceAllString(strings.Join(out, "\n"), "\n\n")
	return strings.TrimSpace(s)
}

var inRowBreaks = regexp.MustCompile(`(?i)<br\b[^>]*>|</(p|div|li|h[1-6])\s*>`)

// flattenRows keeps each innermost table row on one line: paragraphs and
// <br>s inside its cells become spaces instead of line breaks, so a receipt's
// description, quantity and price stay on the same line. Rows that hold a
// nested table are layout, not data, and keep their breaks.
func flattenRows(s string) string {
	lower := strings.ToLower(s)
	var b strings.Builder
	done := 0
	for from := 0; ; {
		end := indexTag(lower, "</tr", from)
		if end < 0 {
			break
		}
		from = end + 1
		start := lastIndexTag(lower[:end], "<tr")
		if start < done || strings.Contains(lower[start:end], "<table") {
			continue
		}
		b.WriteString(s[done:start])
		b.WriteString(inRowBreaks.ReplaceAllString(s[start:end], " "))
		done = end
	}
	b.WriteString(s[done:])
	return b.String()
}

// lastIndexTag is indexTag searching backwards.
func lastIndexTag(lower, prefix string) int {
	for to := len(lower); to > 0; {
		at := strings.LastIndex(lower[:to], prefix)
		if at < 0 {
			return -1
		}
		end := at + len(prefix)
		if end == len(lower) || strings.IndexByte(" \t\r\n\f>/", lower[end]) >= 0 {
			return at
		}
		to = at
	}
	return -1
}

// dropElement removes every <tag ...>...</tag> from s, matching the tag name
// case-insensitively and only as a whole name, so dropping "head" leaves
// <header> alone.
func dropElement(s, tag string) string {
	lower := strings.ToLower(s) // ASCII-only change, so offsets still line up
	var b strings.Builder
	i := 0
	for {
		j := indexTag(lower, "<"+tag, i)
		if j < 0 {
			b.WriteString(s[i:])
			return b.String()
		}
		b.WriteString(s[i:j])
		k := indexTag(lower, "</"+tag, j)
		if k < 0 {
			return b.String()
		}
		if gt := strings.IndexByte(lower[k:], '>'); gt >= 0 {
			i = k + gt + 1
		} else {
			return b.String()
		}
	}
}

// indexTag finds prefix at or after from where the next byte ends the tag
// name: whitespace, '>' or '/'.
func indexTag(lower, prefix string, from int) int {
	for from <= len(lower) {
		n := strings.Index(lower[from:], prefix)
		if n < 0 {
			return -1
		}
		at := from + n
		end := at + len(prefix)
		if end == len(lower) || strings.IndexByte(" \t\r\n\f>/", lower[end]) >= 0 {
			return at
		}
		from = at + 1
	}
	return -1
}

type attachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int    `json:"size"`
}

// extractBody prefers text/plain and falls back to flattened HTML. A
// text/plain part that is empty or only whitespace does not count: some
// senders (PredictSpring's receipt template, for one) put an empty one beside
// the real HTML body, and preferring it returned nothing at all. Attachments
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
		switch strings.ToLower(p.MimeType) {
		case "text/plain":
			if t := decodePart(p); strings.TrimSpace(t) != "" {
				plain = append(plain, t)
			}
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
