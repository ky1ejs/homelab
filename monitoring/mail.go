package main

// Sending the report. Plain SMTP submission with the stdlib client, because a
// weekly email to one address does not justify a mail library in a module that
// is otherwise dependency-free. The MIME it needs is one multipart/alternative
// with two parts, which is a few lines here.
//
// TLS IS NOT OPTIONAL. Port 465 is implicit TLS; anything else must offer
// STARTTLS or the send is refused before credentials are written. net/smtp's
// PlainAuth refuses an unencrypted connection to anything but localhost too,
// but that is a second line, not the first: the check here also covers an
// unauthenticated relay, where no AUTH happens at all and the report would
// otherwise cross the network in clear.

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

type mailConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
	To       []string
}

func (c mailConfig) validate() error {
	if c.Host == "" {
		return errors.New("SMTP_HOST is not set")
	}
	if c.From == "" || len(c.To) == 0 {
		return errors.New("REPORT_FROM and REPORT_TO must both be set")
	}
	for _, a := range append([]string{c.From}, c.To...) {
		if _, err := mail.ParseAddress(a); err != nil {
			return fmt.Errorf("not an email address: %q", a)
		}
	}
	return nil
}

// email is one message's content. HTML is optional: without it the message
// is a single text/plain part, which is what the failure report sends.
type email struct {
	Subject string
	Text    string
	HTML    string
}

// buildMessage assembles an RFC 5322 message. Header values are checked for
// CR and LF: the subject is generated here, not supplied, but a header built
// from a string that can carry a newline is a header that can be injected into.
func buildMessage(c mailConfig, m email, now time.Time) ([]byte, error) {
	for _, v := range append([]string{c.From, m.Subject}, c.To...) {
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("newline in mail header value %q", v)
		}
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	domain := "homelab.invalid"
	if at := strings.LastIndex(c.From, "@"); at >= 0 {
		domain = strings.Trim(c.From[at+1:], "> ")
	}

	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", c.From)
	h("To", strings.Join(c.To, ", "))
	// RFC 2047: a disk or system name from the hub may not be ASCII. An
	// ASCII subject comes back unchanged.
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	// render() writes ASCII with lines far under 7bit's 998-octet limit, and
	// the HTML part writes the hub's names as entities. The text part carries
	// those names as they are, so if one is not ASCII the part goes as UTF-8
	// quoted-printable: a stray 8-bit byte under a 7bit header is a message
	// some gateways reject.
	part := func(contentType, body string) {
		if isASCII(body) {
			h("Content-Type", contentType+"; charset=us-ascii")
			h("Content-Transfer-Encoding", "7bit")
			b.WriteString("\r\n")
			b.WriteString(crlf(body))
			return
		}
		h("Content-Type", contentType+"; charset=utf-8")
		h("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		var q strings.Builder
		w := quotedprintable.NewWriter(&q)
		w.Write([]byte(body)) // writes to a strings.Builder cannot fail
		w.Close()
		b.WriteString(q.String())
	}
	if m.HTML == "" {
		part("text/plain", m.Text)
		return []byte(b.String()), nil
	}
	// The boundary is random so no body can contain it. Text first: clients
	// show the LAST alternative they can render, so HTML wins where it can.
	boundary := "homelab-" + hex.EncodeToString(id)
	h("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	for _, p := range []struct{ typ, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		b.WriteString("--" + boundary + "\r\n")
		part(p.typ, p.body)
		if !strings.HasSuffix(p.body, "\n") {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String()), nil
}

func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

func sendMail(c mailConfig, msg []byte) error {
	addr := net.JoinHostPort(c.Host, c.Port)
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	tlsConf := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	if c.Port == "465" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConf)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	// One deadline for the whole exchange: a server that accepts the
	// connection and then stalls would otherwise hold the send forever.
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer client.Close()

	if c.Port != "465" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not offer STARTTLS - refusing to send in clear", addr)
		}
		if err := client.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if c.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	from, _ := mail.ParseAddress(c.From)
	if err := client.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, to := range c.To {
		a, _ := mail.ParseAddress(to)
		if err := client.Rcpt(a.Address); err != nil {
			return fmt.Errorf("smtp RCPT TO %s: %w", a.Address, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp end of data: %w", err)
	}
	// The server accepted the message at end of DATA. A failed QUIT after that
	// is not a failed send, and reporting it as one would skip the ping and
	// set off the dead-man's switch for a report that arrived.
	if err := client.Quit(); err != nil {
		log.Printf("smtp QUIT after a delivered message: %v", err)
	}
	return nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}
