package main

import (
	"mime"
	"net/mail"
	"strings"
	"testing"
)

func TestBareAddress(t *testing.T) {
	for _, ok := range []string{"a@example.com", "first.last+tag@sub.example.co.uk"} {
		if _, err := bareAddress(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"Alice <a@example.com>", "a@localhost", "no-at-sign", "a@example.com\nBcc: x@y.com", "(c) a@example.com"} {
		if _, err := bareAddress(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBuild(t *testing.T) {
	raw, err := outgoing{
		From:       mail.Address{Name: "Zoë", Address: "me@example.com"},
		To:         []string{"a@example.com", "b@example.com"},
		Cc:         []string{"c@example.com"},
		Bcc:        []string{"d@example.com"},
		Subject:    "Plans ✓",
		Body:       "Line one\nLine two",
		InReplyTo:  "<orig@example.com>",
		References: "<root@example.com> <orig@example.com>",
	}.build()
	if err != nil {
		t.Fatal(err)
	}
	h, body := decodeBody(t, raw)
	for _, want := range []string{
		"From: =?utf-8?q?Zo=C3=AB?= <me@example.com>",
		"To: a@example.com, b@example.com",
		"Cc: c@example.com",
		"Bcc: d@example.com",
		"In-Reply-To: <orig@example.com>",
		`Content-Type: text/plain; charset="UTF-8"`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %q in\n%s", want, h)
		}
	}
	subj := ""
	for _, line := range strings.Split(h, "\r\n") {
		if strings.HasPrefix(line, "Subject: ") {
			subj, _ = new(mime.WordDecoder).DecodeHeader(strings.TrimPrefix(line, "Subject: "))
		}
	}
	if subj != "Plans ✓" {
		t.Errorf("subject decodes to %q", subj)
	}
	if body != "Line one\r\nLine two" {
		t.Errorf("body = %q", body)
	}
}

func TestBuildRefusesHeaderBreaks(t *testing.T) {
	for _, m := range []outgoing{
		{From: mail.Address{Address: "a@example.com"}, To: []string{"b@example.com"}, Subject: "Hi\r\nBcc: e@example.com"},
		{From: mail.Address{Name: "x\nBcc: e@example.com", Address: "a@example.com"}, To: []string{"b@example.com"}, Subject: "Hi"},
	} {
		if _, err := m.build(); err == nil {
			t.Errorf("accepted %+v", m)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	got := htmlToText("<style>p{}</style><p>a&#39;b</p><p>c&#x2713;</p><script>alert(1)</script>")
	if got != "a'b\nc✓" {
		t.Errorf("got %q", got)
	}
}

func TestLatin1Body(t *testing.T) {
	p := gmailPart{MimeType: "text/plain", Headers: []gmailHeader{{"Content-Type", "text/plain; charset=ISO-8859-1"}}}
	p.Body.Data = b64("caf\xe9")
	if got := decodePart(p); got != "café" {
		t.Errorf("got %q", got)
	}
}
