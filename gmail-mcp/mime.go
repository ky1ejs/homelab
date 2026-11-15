package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
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
// (x/net/html, by contrast, is a dependency: a wrong charset garbles a few
// letters, but a wrong HTML tokenizer lets hidden text through to the model.
// See htmlToText.)
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

// hiddenElements hold content a reader never sees. The tokenizer reads most
// of them as raw text, so markup inside one cannot end it early, and one left
// unclosed hides the rest of the document, as it does in a browser. <head> is
// not here: </head> is optional, and the only text a head can hold is inside
// these elements anyway.
var hiddenElements = map[string]bool{
	"script": true, "style": true, "title": true, "template": true,
	"noscript": true, "iframe": true, "noembed": true, "noframes": true,
}

// Elements that end a line, and of those, the ones that end a paragraph.
var (
	lineBlocks = map[string]bool{
		"div": true, "li": true, "dt": true, "dd": true, "tr": true,
		"section": true, "article": true, "header": true, "footer": true,
		"address": true, "nav": true, "aside": true, "main": true,
		"figure": true, "figcaption": true, "form": true, "fieldset": true,
		"caption": true, "center": true, "p": true, "h1": true, "h2": true,
		"h3": true, "h4": true, "h5": true, "h6": true,
	}
	paragraphBlocks = map[string]bool{
		"table": true, "blockquote": true, "pre": true, "xmp": true,
		"ul": true, "ol": true, "dl": true,
	}
)

// cellSep marks a table cell's end until the text is laid out. It is in the
// Unicode private use area and is removed from every piece of decoded text,
// entities included, so mail cannot forge one.
const cellSep = '\ue000'

// htmlToText is a flattener, not a renderer: enough to read an HTML-only mail
// back, with hidden content dropped rather than shown.
//
// It walks golang.org/x/net/html's tokenizer once, left to right, so it is
// linear in the input and reads comments, attributes and raw text the way a
// browser does. A hand-rolled regex version let a "<!--" inside an attribute
// swallow a <script> opener, and leaked unclosed comments; text hidden in a
// comment, a <title> or a <template> is a common place to put instructions
// aimed at a model rather than the reader.
//
// Layout: whitespace collapses except inside <pre>; block elements end lines;
// cells on one table row are joined with " | ". Inside an innermost row (one
// holding no nested table) line breaks become spaces, which keeps a receipt's
// description, quantity and price on one line. Rows that hold a table are
// layout, not data, and keep their breaks.
//
// Content-Transfer-Encoding is not handled here: the Gmail API has already
// undone quoted-printable and base64 by the time decodePart sees the data.
func htmlToText(src string) string {
	f := flattener{hidden: map[string]int{}}
	z := html.NewTokenizer(strings.NewReader(src))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.TextToken:
			if f.isHidden() {
				continue
			}
			f.text(string(z.Text()))
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			f.start(string(name), tt == html.SelfClosingTagToken)
		case html.EndTagToken:
			name, _ := z.TagName()
			f.end(string(name))
		}
		// Comments and doctypes carry nothing a reader sees.
	}
	return f.layout()
}

type flattener struct {
	out    []byte
	hidden map[string]int // open hidden elements, by name
	pre    int            // open <pre> and <xmp> elements
	rows   []*openRow     // open <tr> elements, innermost last
}

type openRow struct {
	breaks   []int // offsets in out of line breaks written inside this row
	hasTable bool
}

func (f *flattener) isHidden() bool {
	for _, n := range f.hidden {
		if n > 0 {
			return true
		}
	}
	return false
}

func (f *flattener) start(name string, selfClosing bool) {
	if hiddenElements[name] {
		// <script/> still opens a script: the tokenizer reads its content as
		// raw text up to </script>, and so does a browser.
		f.hidden[name]++
		return
	}
	if f.isHidden() {
		return
	}
	switch {
	case name == "br":
		f.newline()
	case name == "hr":
		f.lineBreak()
	case name == "tr" && !selfClosing:
		f.lineBreak()
		f.rows = append(f.rows, &openRow{})
	case name == "table":
		// Only the innermost row is marked; endRow passes the mark outwards,
		// which keeps deeply nested tables linear.
		if n := len(f.rows); n > 0 {
			f.rows[n-1].hasTable = true
		}
		f.paragraphBreak()
	case (name == "pre" || name == "xmp") && !selfClosing:
		f.paragraphBreak()
		f.pre++
	case lineBlocks[name] || paragraphBlocks[name]:
		f.lineBreak()
	}
}

func (f *flattener) end(name string) {
	if hiddenElements[name] {
		if f.hidden[name] > 0 {
			f.hidden[name]--
		}
		return
	}
	if f.isHidden() {
		return
	}
	switch {
	case name == "td" || name == "th":
		f.trimSpace()
		f.out = utf8.AppendRune(f.out, cellSep)
	case name == "tr":
		f.endRow()
		f.lineBreak()
	case (name == "pre" || name == "xmp") && f.pre > 0:
		f.pre--
		f.paragraphBreak()
	case paragraphBlocks[name]:
		f.paragraphBreak()
	case lineBlocks[name]:
		f.lineBreak()
	}
}

// endRow closes the innermost open row. A data row's breaks become spaces;
// a layout row's stay. Each break is resolved exactly once, by the row it was
// written in.
func (f *flattener) endRow() {
	if len(f.rows) == 0 {
		return
	}
	r := f.rows[len(f.rows)-1]
	f.rows = f.rows[:len(f.rows)-1]
	if r.hasTable {
		if n := len(f.rows); n > 0 {
			f.rows[n-1].hasTable = true
		}
		return
	}
	for _, at := range r.breaks {
		f.out[at] = ' '
	}
}

func (f *flattener) text(t string) {
	t = strings.Map(func(r rune) rune {
		if r == cellSep || r == 0 {
			return -1
		}
		return r
	}, t)
	if f.pre > 0 {
		for i, line := range strings.Split(strings.ReplaceAll(t, "\r\n", "\n"), "\n") {
			if i > 0 {
				f.newline()
			}
			f.out = append(f.out, strings.ReplaceAll(line, "\r", "")...)
		}
		return
	}
	for _, r := range t {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\u00a0' {
			if n := len(f.out); n > 0 && f.out[n-1] != ' ' && f.out[n-1] != '\n' && !f.afterCell() {
				f.out = append(f.out, ' ')
			}
			continue
		}
		f.out = utf8.AppendRune(f.out, r)
	}
}

// afterCell reports whether out ends with a cell separator, after which
// leading whitespace is dropped just as it is after a line break.
func (f *flattener) afterCell() bool {
	r, _ := utf8.DecodeLastRune(f.out)
	return r == cellSep
}

func (f *flattener) trimSpace() {
	for len(f.out) > 0 && f.out[len(f.out)-1] == ' ' {
		f.out = f.out[:len(f.out)-1]
	}
}

// newline always writes a line break: <br><br> is a blank line.
func (f *flattener) newline() {
	f.trimSpace()
	if len(f.rows) > 0 {
		r := f.rows[len(f.rows)-1]
		r.breaks = append(r.breaks, len(f.out))
	}
	f.out = append(f.out, '\n')
}

// lineBreak ends the current line unless it is already ended.
func (f *flattener) lineBreak() {
	f.trimSpace()
	if n := len(f.out); n > 0 && f.out[n-1] != '\n' {
		f.newline()
	}
}

// paragraphBreak leaves one blank line after the current line.
func (f *flattener) paragraphBreak() {
	f.lineBreak()
	if n := len(f.out); n > 1 && f.out[n-2] != '\n' {
		f.newline()
	}
}

// layout joins each line's cells with " | ", drops empty cells, and keeps at
// most one blank line in a row. Spaces left by a data row's resolved breaks
// are collapsed here, outside <pre>, which never holds a cell separator.
func (f *flattener) layout() string {
	lines := strings.Split(string(f.out), "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		if strings.ContainsRune(line, cellSep) {
			var cells []string
			for _, c := range strings.Split(line, string(cellSep)) {
				if c = strings.Join(strings.Fields(c), " "); c != "" {
					cells = append(cells, c)
				}
			}
			line = strings.Join(cells, " | ")
		} else {
			line = strings.TrimRight(line, " ")
		}
		if strings.TrimSpace(line) == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

type attachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int    `json:"size"`
}

// safeExtractBody is extractBody that cannot take the server down: a panic in
// a tool handler is not recovered by the MCP SDK and ends the process, and
// every byte it reads here was written by whoever sent the mail. A message
// whose body cannot be read says so instead.
func safeExtractBody(p gmailPart) (body string, atts []attachment) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("extracting a message body panicked", "panic", r)
			body, atts = "[this message's body could not be read]", []attachment{}
		}
	}()
	return extractBody(p)
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
