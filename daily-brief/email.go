package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"mime"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"time"
)

// The email: multipart/alternative, a plain-text part and an HTML part, both
// built from the validated issue.
//
// Every field reaching here has passed Validate, so none holds a CR, an LF or
// any other control character, and a header cannot be split. The subject is
// still Q-encoded, because the title may hold non-ASCII. The HTML part goes
// through html/template, which escapes every field and would neutralise a
// non-https link even if one got past checkURL.

// Podcast is what the email says about today's episode: a link, or why there
// is none. Exactly one of the two is set, or neither when no show is
// configured.
type Podcast struct {
	URL  string
	Note string
}

type message struct {
	From, To string
	Subject  string
	Date     time.Time
	Text     string
	HTML     string // empty for a plain-text message
}

func briefSubject(is *Issue, day time.Time) string {
	return fmt.Sprintf("Daily brief for %s: %s", day.Format("Mon 2 Jan"), is.Title)
}

func buildBrief(from, to string, is *Issue, day time.Time, p Podcast, now time.Time) ([]byte, error) {
	var html bytes.Buffer
	if err := briefHTML.Execute(&html, briefView{Issue: is, Day: day.Format("Monday 2 January"), Podcast: p}); err != nil {
		return nil, err
	}
	return message{
		From: from, To: to, Date: now,
		Subject: briefSubject(is, day),
		Text:    briefText(is, day, p),
		HTML:    html.String(),
	}.bytes()
}

func briefText(is *Issue, day time.Time, p Podcast) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n\n%s\n", is.Title, day.Format("Monday 2 January"), is.Summary)
	switch {
	case p.URL != "":
		fmt.Fprintf(&b, "\nListen on Spotify: %s\n", p.URL)
	case p.Note != "":
		fmt.Fprintf(&b, "\n%s\n", p.Note)
	}
	for _, s := range is.Sections {
		fmt.Fprintf(&b, "\n%s\n\n", strings.ToUpper(s.Title))
		for _, it := range s.Items {
			fmt.Fprintf(&b, "%s\n%s\n%s: %s\n\n", it.Headline, it.Text, it.Source, it.URL)
		}
	}
	return b.String()
}

type briefView struct {
	Issue   *Issue
	Day     string
	Podcast Podcast
}

// Inline styles only: Gmail drops <style> blocks in some clients. Kept plain
// so it reads the same in dark mode.
var briefHTML = template.Must(template.New("brief").Parse(`<!doctype html>
<html><body style="margin:0;padding:0">
<div style="max-width:640px;margin:0 auto;padding:16px;font-family:-apple-system,Helvetica,Arial,sans-serif;font-size:16px;line-height:1.5">
<h1 style="font-size:22px;margin:0 0 4px">{{.Issue.Title}}</h1>
<p style="margin:0 0 12px;opacity:.7">{{.Day}}</p>
<p style="margin:0 0 16px">{{.Issue.Summary}}</p>
{{- with .Podcast.URL}}
<p style="margin:0 0 16px"><a href="{{.}}" style="font-weight:600">Listen on Spotify</a></p>
{{- else}}{{with .Podcast.Note}}
<p style="margin:0 0 16px;opacity:.7">{{.}}</p>
{{- end}}{{end}}
{{- range .Issue.Sections}}
<h2 style="font-size:17px;margin:24px 0 8px;text-transform:uppercase;letter-spacing:.04em">{{.Title}}</h2>
{{- range .Items}}
<p style="margin:0 0 14px"><strong>{{.Headline}}</strong><br>{{.Text}}<br><a href="{{.URL}}">{{.Source}}</a></p>
{{- end}}
{{- end}}
</div>
</body></html>
`))

// buildNotice is the plain-text email for a day that has no brief: the
// writer failed, or wrote something the publisher refused. reason may come
// from failed.txt, which write.sh writes but which sits in a directory the
// agent's tools reach, so it is cleaned to printable text and capped here.
func buildNotice(from, to, date, reason string, now time.Time) ([]byte, error) {
	return message{
		From: from, To: to, Date: now,
		Subject: "Daily brief for " + date + " did not go out",
		Text: "The daily brief for " + date + " was not sent.\n\nReason: " + cleanReason(reason) +
			"\n\nThe run's log is in `docker compose logs brief-agent` on the NAS.\n",
	}.bytes()
}

func cleanReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < ' ' || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '�' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" {
		return "none given"
	}
	return truncate(s, 500)
}

func (m message) bytes() ([]byte, error) {
	var b bytes.Buffer
	h := textproto.MIMEHeader{}
	h.Set("From", m.From)
	h.Set("To", m.To)
	h.Set("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h.Set("Date", m.Date.Format(time.RFC1123Z))
	h.Set("MIME-Version", "1.0")
	for _, k := range []string{"From", "To", "Subject", "Date", "MIME-Version"} {
		fmt.Fprintf(&b, "%s: %s\r\n", k, h.Get(k))
	}
	if m.HTML == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		if err := writeQP(&b, m.Text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	boundary := randomBoundary()
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	for _, part := range []struct{ typ, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary, part.typ)
		if err := writeQP(&b, part.body); err != nil {
			return nil, err
		}
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), nil
}

// writeQP writes s quoted-printable with CRLF line ends. No field can hold
// the multipart boundary: it is 128 random bits, drawn after the text exists.
func writeQP(b *bytes.Buffer, s string) error {
	w := quotedprintable.NewWriter(b)
	if _, err := w.Write([]byte(strings.ReplaceAll(s, "\n", "\r\n"))); err != nil {
		return err
	}
	return w.Close()
}

func randomBoundary() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return "brief-" + hex.EncodeToString(buf)
}
