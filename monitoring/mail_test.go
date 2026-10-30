package main

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

func testMail() mailConfig {
	return mailConfig{Host: "127.0.0.1", Port: "587", From: "NAS <nas@example.com>", To: []string{"me@example.com"}}
}

func TestBuildMessage(t *testing.T) {
	msg, err := buildMessage(testMail(), "[homelab] weekly: all clear", "line one\nline two\n", time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s := string(msg)
	for _, want := range []string{
		"From: NAS <nas@example.com>\r\n",
		"To: me@example.com\r\n",
		"Subject: [homelab] weekly: all clear\r\n",
		"Content-Type: text/plain; charset=us-ascii\r\n",
		"@example.com>\r\n",
		"\r\n\r\nline one\r\nline two\r\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") {
		t.Error("bare LF left in message")
	}
}

func TestBuildMessageRefusesHeaderInjection(t *testing.T) {
	c := testMail()
	if _, err := buildMessage(c, "hi\r\nBcc: x@evil.example", "", time.Now()); err == nil {
		t.Error("newline in subject accepted")
	}
	c.To = []string{"me@example.com\nBcc: x@evil.example"}
	if _, err := buildMessage(c, "hi", "", time.Now()); err == nil {
		t.Error("newline in recipient accepted")
	}
}

func TestValidate(t *testing.T) {
	if err := testMail().validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []mailConfig{
		{Port: "587", From: "a@b.c", To: []string{"d@e.f"}},
		{Host: "h", Port: "587", To: []string{"d@e.f"}},
		{Host: "h", Port: "587", From: "a@b.c"},
		{Host: "h", Port: "587", From: "a@b.c", To: []string{"not an address"}},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
}

// A server that never offers STARTTLS must get nothing past EHLO: no AUTH, no
// MAIL FROM, and above all not the report.
func TestRefusesToSendWithoutTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	seen := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var lines []string
		r := bufio.NewReader(conn)
		conn.Write([]byte("220 fake ESMTP\r\n"))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			lines = append(lines, strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(line, "EHLO"):
				conn.Write([]byte("250-fake\r\n250 AUTH PLAIN\r\n"))
			case strings.HasPrefix(line, "QUIT"):
				conn.Write([]byte("221 bye\r\n"))
				seen <- lines
				return
			default:
				conn.Write([]byte("250 ok\r\n"))
			}
		}
		seen <- lines
	}()

	c := testMail()
	_, c.Port, _ = net.SplitHostPort(ln.Addr().String())
	c.Username, c.Password = "u", "p"
	err = sendMail(c, []byte("Subject: secret\r\n\r\nthe report\r\n"))
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("want a STARTTLS refusal, got %v", err)
	}
	for _, l := range <-seen {
		if !strings.HasPrefix(l, "EHLO") && !strings.HasPrefix(l, "QUIT") {
			t.Errorf("client sent %q to a server without TLS", l)
		}
	}
}
