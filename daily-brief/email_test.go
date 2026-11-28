package main

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

// parts parses a built message the way a mail client would and returns its
// decoded subject and its parts by content type.
func parts(t *testing.T, raw []byte) (string, map[string]string) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	if !strings.HasPrefix(mt, "multipart/") {
		b, _ := io.ReadAll(qpReader(m.Body))
		out[mt] = string(b)
		return subject, out
	}
	r := multipart.NewReader(m.Body, params["boundary"])
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pt, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		b, _ := io.ReadAll(p) // multipart decodes quoted-printable itself
		out[pt] = string(b)
	}
	return subject, out
}

func TestBuildBrief(t *testing.T) {
	is := sampleIssue("2026-11-28")
	is.Title = "Rates hold, café owners worry"
	is.Sections[0].Items[0].Text = `<script>alert(1)</script> & "quotes"`
	day := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	raw, err := buildBrief("you@example.com", "you@example.com", is, day, Podcast{URL: "https://open.spotify.com/episode/abc"}, day)
	if err != nil {
		t.Fatal(err)
	}
	subject, ps := parts(t, raw)
	if subject != "Daily brief for Sat 28 Nov: Rates hold, café owners worry" {
		t.Fatalf("subject = %q", subject)
	}
	html, text := ps["text/html"], ps["text/plain"]
	mustContain(t, html, `&lt;script&gt;alert(1)&lt;/script&gt; &amp; &#34;quotes&#34;`)
	mustContain(t, html, `<a href="https://open.spotify.com/episode/abc"`)
	mustContain(t, html, `<a href="https://www.ft.com/content/abc">FT</a>`)
	mustContain(t, text, "Listen on Spotify: https://open.spotify.com/episode/abc")
	mustContain(t, text, "BUSINESS AND MARKETS")
	mustContain(t, text, "BBC News: https://www.bbc.co.uk/news/business-1")
	if strings.Contains(html, "<script>") {
		t.Fatal("unescaped markup in the HTML part")
	}
}

func TestBuildBriefNote(t *testing.T) {
	is := sampleIssue("2026-11-28")
	day := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	raw, err := buildBrief("a@b.co", "a@b.co", is, day, Podcast{Note: "No podcast today: the voice render failed."}, day)
	if err != nil {
		t.Fatal(err)
	}
	_, ps := parts(t, raw)
	mustContain(t, ps["text/html"], "No podcast today: the voice render failed.")
	if strings.Contains(ps["text/html"], "Listen on Spotify") {
		t.Fatal("link shown with no episode")
	}
}

func TestBuildNoticeCleansReason(t *testing.T) {
	now := time.Date(2026, 11, 28, 6, 0, 0, 0, time.UTC)
	raw, err := buildNotice("a@b.co", "a@b.co", "2026-11-28", "timed out\r\nBcc: evil@x.com\x1b[2J"+strings.Repeat("z", 900), now)
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get("Bcc") != "" {
		t.Fatal("reason injected a header")
	}
	subject, ps := parts(t, raw)
	if subject != "Daily brief for 2026-11-28 did not go out" {
		t.Fatalf("subject = %q", subject)
	}
	body := ps["text/plain"]
	mustContain(t, body, "Reason: timed out Bcc: evil@x.com[2J")
	if strings.Contains(body, "\x1b") || strings.Count(body, "z") > 500 {
		t.Fatal("reason not cleaned or not capped")
	}
}
