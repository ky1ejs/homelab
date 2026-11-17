package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type parsedPart struct {
	contentType, disposition, filename string
	data                               []byte
}

// parseMixed reads a built message back the way a mail client would.
func parseMixed(t *testing.T, raw string) (*mail.Message, []parsedPart) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("Content-Type = %q (%v)", m.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var parts []parsedPart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if enc := p.Header.Get("Content-Transfer-Encoding"); enc != "base64" {
			t.Fatalf("part encoding = %q", enc)
		}
		b, _ := io.ReadAll(p)
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(b), "\r\n", ""))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\r\n") {
			if len(line) > 76 {
				t.Fatalf("base64 line of %d characters", len(line))
			}
		}
		pp := parsedPart{contentType: p.Header.Get("Content-Type"), disposition: p.Header.Get("Content-Disposition"), data: data}
		if _, dp, err := mime.ParseMediaType(pp.disposition); err == nil {
			pp.filename = dp["filename"]
		}
		parts = append(parts, pp)
	}
	return m, parts
}

func TestBuildWithAttachments(t *testing.T) {
	photo := bytes.Repeat([]byte{0xff, 0xd8, 0x00, '\r', '\n', '-', '-'}, 5000)
	raw, err := outgoing{
		From: mail.Address{Address: "me@example.com"}, To: []string{"a@example.com"}, Subject: "Claim", Body: "Photo attached.\n",
		Attachments: []outgoingAttachment{
			{Filename: "waist snap.jpeg", MimeType: "image/jpeg", Data: photo},
			{Filename: "Café menu.pdf", MimeType: "application/pdf", Data: []byte("%PDF-1.4")},
		},
	}.build()
	if err != nil {
		t.Fatal(err)
	}
	_, parts := parseMixed(t, string(raw))
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(parts))
	}
	if !strings.HasPrefix(parts[0].contentType, "text/plain") || string(parts[0].data) != "Photo attached.\r\n" || parts[0].disposition != "" {
		t.Errorf("body part = %+v", parts[0])
	}
	if !bytes.Equal(parts[1].data, photo) || parts[1].filename != "waist snap.jpeg" || !strings.HasPrefix(parts[1].contentType, "image/jpeg") {
		t.Errorf("photo part: filename %q, type %q, %d bytes", parts[1].filename, parts[1].contentType, len(parts[1].data))
	}
	if parts[2].filename != "Café menu.pdf" || !strings.Contains(parts[2].disposition, "filename*=utf-8''") {
		t.Errorf("non-ASCII filename: %q from %q", parts[2].filename, parts[2].disposition)
	}
}

// A plain message is unchanged: no multipart wrapper.
func TestBuildWithoutAttachmentsIsPlain(t *testing.T) {
	raw, err := outgoing{From: mail.Address{Address: "me@example.com"}, To: []string{"a@example.com"}, Subject: "x", Body: "y"}.build()
	if err != nil {
		t.Fatal(err)
	}
	if s := string(raw); strings.Contains(s, "multipart") || !strings.Contains(s, "Content-Type: text/plain; charset=\"UTF-8\"\r\n") {
		t.Errorf("raw = %q", raw)
	}
}

func TestCheckFilename(t *testing.T) {
	good := []string{"photo.jpg", "Café menu.pdf", "orvis-waist-snap.jpeg", "notes", ".hidden.txt", strings.Repeat("a", 251) + ".txt"}
	bad := []string{
		"", ".", "..", " lead.jpg", "trail.jpg ", "a\r\nBcc: x@example.com", "a\x00.jpg", "../etc/passwd", "dir/x.jpg", `dir\x.jpg`,
		"virus.exe", "VIRUS.EXE", "script.js", "disk.iso",
		"photo‮gpj.png", "a⁦b.txt", "line break.txt", "c1\u0085.txt",
		strings.Repeat("a", 252) + ".txt", "bad\xff.txt",
		// Invisible and lookalike characters, from the security review.
		"invoice\u061cfdp.html", "invoice\u200b.pdf", "in\ufeffvoice.pdf", "invoice\u2060.pdf",
		"invoice\u00ad.pdf", "invoice.pdf\u3000\u3000\u3000.html", "a\u00a0b.pdf", "report.exe.", "report.pdf.",
	}
	for _, n := range good {
		if err := checkFilename(n); err != nil {
			t.Errorf("checkFilename(%q) = %v, want ok", n, err)
		}
	}
	for _, n := range bad {
		if err := checkFilename(n); !errors.Is(err, errBadInput) {
			t.Errorf("checkFilename(%q) = %v, want errBadInput", n, err)
		}
	}
}

func TestAttachmentType(t *testing.T) {
	cases := []struct{ given, name, want string }{
		{"", "a.jpg", "image/jpeg"},
		{"", "a.jpeg", "image/jpeg"},
		{"", "a.pdf", "application/pdf"},
		{"", "a.unknownext", "application/octet-stream"},
		{"", "noext", "application/octet-stream"},
		{"IMAGE/JPEG", "a", "image/jpeg"},
		{" text/csv ", "a", "text/csv"},
	}
	for _, c := range cases {
		if got, err := attachmentType(c.given, c.name); err != nil || got != c.want {
			t.Errorf("attachmentType(%q, %q) = %q, %v; want %q", c.given, c.name, got, err, c.want)
		}
	}
	for _, given := range []string{"text/plain; charset=utf-8", "multipart/mixed", "message/rfc822", "image/jpeg\r\nBcc: x", "jpeg", "image/", `image/"x"`} {
		if _, err := attachmentType(given, "a.jpg"); !errors.Is(err, errBadInput) {
			t.Errorf("attachmentType(%q) = %v, want errBadInput", given, err)
		}
	}
}

func TestDecodeInline(t *testing.T) {
	want := []byte{0xfb, 0xff, 0xfe, 'h', 'i'}
	for _, in := range []string{
		base64.StdEncoding.EncodeToString(want),
		base64.RawStdEncoding.EncodeToString(want),
		base64.URLEncoding.EncodeToString(want),
		base64.RawURLEncoding.EncodeToString(want),
		"+//+\naGk=\r\n",
	} {
		if got, err := decodeInline(in); err != nil || !bytes.Equal(got, want) {
			t.Errorf("decodeInline(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := decodeInline("not base64!"); err == nil {
		t.Error("decodeInline accepted garbage")
	}
}

func newTestHTTP(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func signedIn(sub string) context.Context { return withSubject(context.Background(), sub) }

// An attachment goes through the media endpoint, as multipart/related with
// the thread in the metadata, and never as JSON raw.
func TestSendWithInlineAttachment(t *testing.T) {
	f := newFakeGmail(t)
	tl := &tools{gmail: f.client(), log: quietLogger()}
	csv := "date,fish\n2026-11-15,brown\n"
	r, _, err := tl.send(signedIn("user_kyle"), nil, composeInput{
		To: []string{"a@example.com"}, Subject: "Log", Body: "Attached.", From: "alias@example.org",
		Attachments: []attachmentInput{{Filename: "log.csv", MimeType: "text/csv", ContentBase64: base64.StdEncoding.EncodeToString([]byte(csv))}},
	})
	if err != nil || r == nil || r.IsError {
		t.Fatalf("r = %+v (%s), err = %v", r, resultText(r), err)
	}
	if f.called("/messages/send") {
		t.Error("a message with an attachment went to the JSON endpoint")
	}
	u, ok := f.mediaAt("/messages/send")
	if !ok {
		t.Fatal("nothing reached the media endpoint")
	}
	if len(u.types) != 2 || !strings.HasPrefix(u.types[0], "application/json") || u.types[1] != "message/rfc822" || u.length <= 0 {
		t.Errorf("upload parts = %v, length %d", u.types, u.length)
	}
	m, parts := parseMixed(t, u.raw)
	if !strings.Contains(m.Header.Get("From"), "alias@example.org") {
		t.Errorf("From = %q", m.Header.Get("From"))
	}
	if len(parts) != 2 || string(parts[1].data) != csv || parts[1].filename != "log.csv" {
		t.Errorf("parts = %+v", parts)
	}
	if !strings.Contains(resultText(r), "alias@example.org") {
		t.Errorf("result = %s", resultText(r))
	}
}

// The send endpoint takes the thread id at the top of its metadata, unlike
// drafts; the fake refuses the other shape, as a mismatch would silently
// start a new thread at Gmail.
func TestSendReplyWithAttachmentKeepsThread(t *testing.T) {
	f := newFakeGmail(t)
	tl := &tools{gmail: f.client(), log: quietLogger()}
	r, _, _ := tl.send(signedIn("user_kyle"), nil, composeInput{
		To: []string{"a@example.com"}, Body: "Here.", ReplyToMessageID: "m2",
		Attachments: []attachmentInput{{Filename: "a.txt", ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello"))}},
	})
	if r == nil || r.IsError {
		t.Fatalf("r = %+v (%s)", r, resultText(r))
	}
	u, ok := f.mediaAt("/messages/send")
	if !ok || u.threadID != "t1" || strings.Contains(u.meta, `"message"`) || strings.Contains(u.meta, `"raw"`) {
		t.Fatalf("send upload = %+v, %v", u, ok)
	}
	if !strings.Contains(resultText(r), `"thread_id": "t1"`) {
		t.Errorf("result = %s", resultText(r))
	}
}

func TestDraftReplyWithAttachmentKeepsThread(t *testing.T) {
	f := newFakeGmail(t)
	tl := &tools{gmail: f.client(), log: quietLogger()}
	r, _, _ := tl.draft(signedIn("user_kyle"), nil, composeInput{
		To: []string{"a@example.com"}, Body: "Here.", ReplyToMessageID: "m2",
		Attachments: []attachmentInput{{Filename: "a.txt", ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello"))}},
	})
	if r == nil || r.IsError {
		t.Fatalf("r = %+v (%s)", r, resultText(r))
	}
	u, ok := f.mediaAt("/drafts")
	if !ok || u.threadID != "t1" || !strings.Contains(u.meta, `"message"`) {
		t.Fatalf("draft upload = %+v, %v", u, ok)
	}
	m, parts := parseMixed(t, u.raw)
	if m.Header.Get("In-Reply-To") != "<orig-2@example.com>" || len(parts) != 2 || !strings.HasPrefix(parts[1].contentType, "text/plain") {
		t.Errorf("In-Reply-To %q, parts %+v", m.Header.Get("In-Reply-To"), parts)
	}
}

// Every refusal happens before Gmail hears anything, send-as lookup included.
func TestBadAttachmentsNeverReachGmail(t *testing.T) {
	ok := base64.StdEncoding.EncodeToString([]byte("x"))
	many := make([]attachmentInput, maxAttachments+1)
	for i := range many {
		many[i] = attachmentInput{Filename: "a.txt", ContentBase64: ok}
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, maxInlineBytes+1))
	half := base64.StdEncoding.EncodeToString(make([]byte, maxInlineBytes/2+1))
	cases := map[string][]attachmentInput{
		"too many":            many,
		"inline too big":      {{Filename: "a.bin", ContentBase64: big}},
		"inline total":        {{Filename: "a.bin", ContentBase64: half}, {Filename: "b.bin", ContentBase64: half}},
		"no content":          {{Filename: "a.txt"}},
		"empty":               {{Filename: "a.txt", ContentBase64: "===="}},
		"not base64":          {{Filename: "a.txt", ContentBase64: "%%%"}},
		"header in filename":  {{Filename: "a\r\nBcc: x@example.com", ContentBase64: ok}},
		"blocked type":        {{Filename: "a.exe", ContentBase64: ok}},
		"bad mime":            {{Filename: "a.txt", MimeType: "text/plain; boundary=x", ContentBase64: ok}},
		"unknown upload":      {{UploadID: "up_nope", Filename: "a.jpg", SHA256: strings.Repeat("0", 64)}},
		"upload, no sha256":   {{UploadID: "up_nope", Filename: "a.jpg"}},
		"upload, no filename": {{UploadID: "up_nope", SHA256: strings.Repeat("0", 64)}},
		"upload plus inline":  {{UploadID: "up_nope", Filename: "a.jpg", SHA256: strings.Repeat("0", 64), ContentBase64: ok}},
		"html as a photo":     {{Filename: "photo.jpg", ContentBase64: base64.StdEncoding.EncodeToString([]byte("<html><script>x</script>"))}},
		"zip as a pdf":        {{Filename: "invoice.pdf", ContentBase64: base64.StdEncoding.EncodeToString([]byte("PK\x03\x04rest"))}},
		"html typed as jpeg":  {{Filename: "notes", MimeType: "image/jpeg", ContentBase64: base64.StdEncoding.EncodeToString([]byte("<html>"))}},
		// Over the bound and not base64 at all: refused for its size, so the
		// decoder never ran on it.
		"oversized garbage": {{Filename: "a.bin", ContentBase64: strings.Repeat("!", maxInlineBytes*3)}},
	}
	for name, atts := range cases {
		f := newFakeGmail(t)
		tl := &tools{gmail: f.client(), uploads: newUploadStore(), log: quietLogger()}
		r, _, err := tl.send(signedIn("user_kyle"), nil, composeInput{To: []string{"a@example.com"}, Subject: "x", Body: "x", Attachments: atts})
		if err != nil || r == nil || !r.IsError || !strings.Contains(resultText(r), "Nothing was sent") {
			t.Errorf("%s: r = %+v (%s), err = %v", name, r, resultText(r), err)
		}
		f.mu.Lock()
		n := len(f.calls)
		f.mu.Unlock()
		if n != 0 {
			t.Errorf("%s: %d calls reached Gmail", name, n)
		}
		if name == "oversized garbage" && !strings.Contains(resultText(r), "limited to") {
			t.Errorf("oversized garbage was decoded before its size was checked: %s", resultText(r))
		}
	}
}

// A draft or send with attachments may take minutes at the media endpoint,
// longer than the 60s WriteTimeout every other response keeps. Its deadline
// is moved out so the answer still reaches the client.
func TestAttachmentCallOutlastsWriteTimeout(t *testing.T) {
	as := newFakeAS(t)
	g := newFakeGmail(t)
	g.mediaDelay = 700 * time.Millisecond
	srv := testServerWith(t, as, g, func(s *http.Server) { s.WriteTimeout = 300 * time.Millisecond })
	ctx := context.Background()
	token := as.mint(t, "user_kyle", testResource, time.Now().Add(time.Hour))
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "create_draft", Arguments: map[string]any{
		"to": []string{"a@example.com"}, "subject": "x", "body": "x",
		"attachments": []map[string]any{{"filename": "a.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("hi"))}},
	}})
	if err != nil {
		t.Fatalf("slow draft: %v", err)
	}
	if res.IsError {
		t.Fatalf("slow draft: %s", resultText(res))
	}
}

func TestAttachmentTotalCapped(t *testing.T) {
	f := newFakeGmail(t)
	_, err := f.client().prepare(context.Background(), compose{
		To: []string{"a@example.com"}, Subject: "x",
		Attachments: []outgoingAttachment{
			{Filename: "a.bin", MimeType: "application/octet-stream", Data: make([]byte, maxAttachmentBytes/2+1)},
			{Filename: "b.bin", MimeType: "application/octet-stream", Data: make([]byte, maxAttachmentBytes/2+1)},
		},
	})
	if !errors.Is(err, errBadInput) {
		t.Errorf("err = %v, want errBadInput", err)
	}
}

// A media upload that fails without a definite 4xx may still have sent.
func TestMediaSendOutcomeUnknown(t *testing.T) {
	att := []outgoingAttachment{{Filename: "a.txt", MimeType: "text/plain", Data: []byte("x")}}
	for status, want := range map[int]bool{http.StatusBadGateway: true, http.StatusBadRequest: false} {
		f := newFakeGmail(t)
		f.uploadStatus = status
		g := f.client()
		p, err := g.prepare(context.Background(), compose{To: []string{"a@example.com"}, Subject: "x", Attachments: att})
		if err != nil {
			t.Fatal(err)
		}
		_, err = g.send(context.Background(), p)
		if errors.Is(err, errMaybeSent) != want {
			t.Errorf("status %d: err = %v, maybe-sent want %v", status, err, want)
		}
	}
}

// --- uploads -----------------------------------------------------------------

func put(t *testing.T, url string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// The whole path as a client sees it: create_upload over MCP, a PUT with no
// token, then send_message naming the upload.
func TestUploadEndToEnd(t *testing.T) {
	as := newFakeAS(t)
	g := newFakeGmail(t)
	srv := testServer(t, as, g)
	ctx := context.Background()
	token := as.mint(t, "user_kyle", testResource, time.Now().Add(time.Hour))
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "create_upload", Arguments: map[string]any{"filename": "orvis-waist-snap.jpeg"}})
	if err != nil || res.IsError {
		t.Fatalf("create_upload: %v %s", err, resultText(res))
	}
	var up uploadResult
	if err := json.Unmarshal([]byte(resultText(res)), &up); err != nil {
		t.Fatal(err)
	}
	if up.MimeType != "image/jpeg" || !strings.HasPrefix(up.UploadURL, srv.URL+uploadsPath) || up.Method != "PUT" || strings.Contains(up.UploadURL, up.UploadID) {
		t.Fatalf("upload = %+v", up)
	}

	// Naming it before the PUT is refused.
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"to": []string{"claims@example.com"}, "subject": "Claim", "body": "Photo attached.",
		"attachments": []map[string]any{{"upload_id": up.UploadID, "filename": "orvis-waist-snap.jpeg", "sha256": strings.Repeat("0", 64)}},
	}})
	if !res.IsError || !strings.Contains(resultText(res), "PUT") {
		t.Fatalf("send before upload: %s", resultText(res))
	}

	photo := append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), bytes.Repeat([]byte{0x42}, 300000)...)
	resp, body := put(t, up.UploadURL, photo)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %s", resp.StatusCode, body)
	}
	var rc uploadReceipt
	_ = json.Unmarshal(body, &rc)
	if rc.Size != len(photo) || rc.SHA256 != sum(photo) || rc.DetectedType != "image/jpeg" || rc.UploadID != up.UploadID {
		t.Errorf("receipt = %+v", rc)
	}

	// Whoever held the URL could have put anything there, so a send names
	// the hash of the file it meant, and any other is refused.
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"to": []string{"claims@example.com"}, "subject": "Claim", "body": "Photo attached.",
		"attachments": []map[string]any{{"upload_id": up.UploadID, "filename": "orvis-waist-snap.jpeg", "sha256": sum([]byte("some other file"))}},
	}})
	if !res.IsError || !strings.Contains(resultText(res), "sha256 does not match") {
		t.Fatalf("send with the wrong sha256: %s", resultText(res))
	}
	if _, ok := g.mediaAt("/messages/send"); ok {
		t.Fatal("a mismatched upload reached Gmail")
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"to": []string{"claims@example.com"}, "subject": "Claim", "body": "Photo attached.", "from": "alias@example.org",
		"attachments": []map[string]any{{"upload_id": up.UploadID, "filename": "orvis-waist-snap.jpeg", "size": rc.Size, "sha256": rc.SHA256}},
	}})
	if err != nil || res.IsError {
		t.Fatalf("send: %v %s", err, resultText(res))
	}
	u, ok := g.mediaAt("/messages/send")
	if !ok {
		t.Fatal("nothing reached the media endpoint")
	}
	_, parts := parseMixed(t, u.raw)
	if len(parts) != 2 || !bytes.Equal(parts[1].data, photo) || parts[1].filename != "orvis-waist-snap.jpeg" {
		t.Errorf("sent %d parts; attachment %q, %d bytes", len(parts), parts[1].filename, len(parts[1].data))
	}
}

func TestUploadHandlerRules(t *testing.T) {
	s := newUploadStore()
	h := s.handler(quietLogger())
	srv := newTestHTTP(t, h)
	u, err := s.open("user_kyle", "a.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	url := srv + uploadsPath + u.secret

	check := func(name string, method, target string, body []byte, want int) {
		t.Helper()
		req, _ := http.NewRequest(method, target, bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, want)
		}
	}
	check("unknown secret", http.MethodPut, srv+uploadsPath+token(), []byte("x"), http.StatusNotFound)
	check("malformed secret", http.MethodPut, srv+uploadsPath+"../x", []byte("x"), http.StatusNotFound)
	check("upload id is not the secret", http.MethodPut, srv+uploadsPath+strings.TrimPrefix(u.id, "up_"), []byte("x"), http.StatusNotFound)
	check("no GET", http.MethodGet, url, nil, http.StatusMethodNotAllowed)
	check("empty", http.MethodPut, url, nil, http.StatusBadRequest)
	check("too big", http.MethodPut, url, make([]byte, maxAttachmentBytes+1), http.StatusRequestEntityTooLarge)
	check("fits after a refusal", http.MethodPut, url, []byte("ok"), http.StatusOK)
	check("only once", http.MethodPut, url, []byte("again"), http.StatusConflict)
	check("still no GET", http.MethodGet, url, nil, http.StatusMethodNotAllowed)

	ok := sum([]byte("ok"))
	if att, err := s.get("user_kyle", want{id: u.id, sha256: ok, filename: "a.txt"}, false); err != nil || string(att.Data) != "ok" {
		t.Errorf("get = %+v, %v", att, err)
	}
	if _, err := s.get("user_kyle", want{id: u.id, sha256: strings.ToUpper(ok), filename: "a.txt", mimeType: "TEXT/PLAIN", size: 2}, false); err != nil {
		t.Errorf("upper-case sha256: %v", err)
	}
	if _, err := s.get("user_kyle", want{id: u.id, sha256: strings.Repeat("0", 64), filename: "a.txt"}, false); !errors.Is(err, errBadInput) || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("wrong sha256: err = %v", err)
	}
	if _, err := s.get("user_other", want{id: u.id, sha256: ok, filename: "a.txt"}, false); !errors.Is(err, errBadInput) {
		t.Errorf("another subject: err = %v", err)
	}
	if s.held != 2 {
		t.Errorf("held = %d, want 2", s.held)
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fill opens a slot and stores data in it, as a PUT would.
func fill(t *testing.T, s *uploadStore, data []byte) *upload {
	t.Helper()
	u, err := s.open("user_kyle", "a.bin", "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	got, _, status := s.begin(u.secret, int64(len(data)))
	if status != 0 || !s.finish(got, data) {
		t.Fatalf("fill: status %d", status)
	}
	return u
}

func TestUploadsExpireAndAreCapped(t *testing.T) {
	s := newUploadStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	if _, err := s.open("", "a.txt", "text/plain"); !errors.Is(err, errBadInput) {
		t.Errorf("no subject: err = %v", err)
	}

	// An empty slot waits unfilledTTL for its PUT; a filled one uploadTTL
	// from when it was filled.
	empty, _ := s.open("user_kyle", "a.txt", "text/plain")
	full := fill(t, s, []byte("0123456789"))
	now = now.Add(unfilledTTL + time.Second)
	if _, _, status := s.begin(empty.secret, 1); status != http.StatusNotFound {
		t.Errorf("expired empty slot: begin = %d, want 404", status)
	}
	if _, err := s.get("user_kyle", want{id: full.id, sha256: sum([]byte("0123456789")), filename: "a.bin"}, false); err != nil {
		t.Errorf("filled slot gone after %s: %v", unfilledTTL, err)
	}
	now = now.Add(uploadTTL)
	if _, err := s.get("user_kyle", want{id: full.id, sha256: sum([]byte("0123456789")), filename: "a.bin"}, false); !errors.Is(err, errBadInput) {
		t.Errorf("expired: err = %v", err)
	}
	if s.held != 0 || len(s.byID) != 0 {
		t.Errorf("held = %d, slots = %d after expiry", s.held, len(s.byID))
	}

	// With every slot filled, a new one is refused.
	for range maxOpenUploads {
		fill(t, s, []byte("x"))
	}
	if _, err := s.open("user_kyle", "a.txt", "text/plain"); !errors.Is(err, errUploadsFull) {
		t.Fatalf("over the cap: err = %v", err)
	}
}

// Abandoned empty slots cannot lock uploads out: when all are taken, the
// oldest empty one gives way, and a filled one never does.
func TestOpenEvictsOldestEmptySlot(t *testing.T) {
	s := newUploadStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	kept := fill(t, s, []byte("keep"))
	var empties []*upload
	for range maxOpenUploads - 1 {
		now = now.Add(time.Second)
		u, err := s.open("user_kyle", "a.txt", "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		empties = append(empties, u)
	}
	now = now.Add(evictGrace)
	if _, err := s.open("user_kyle", "a.txt", "text/plain"); err != nil {
		t.Fatalf("open with all slots taken: %v", err)
	}
	if _, _, status := s.begin(empties[0].secret, 1); status != http.StatusNotFound {
		t.Errorf("oldest empty slot: begin = %d, want 404 (evicted)", status)
	}
	if _, _, status := s.begin(empties[1].secret, 1); status != 0 {
		t.Errorf("next empty slot: begin = %d, want kept", status)
	}
	if _, err := s.get("user_kyle", want{id: kept.id, sha256: sum([]byte("keep")), filename: "a.bin"}, false); err != nil {
		t.Errorf("filled slot evicted: %v", err)
	}
}

// A PUT reserves what it declares, so two parallel PUTs of ordinary files
// both fit, and one that would not fit is told to retry rather than that its
// file is too big.
func TestUploadReservationsShareRoom(t *testing.T) {
	s := newUploadStore()
	a, _ := s.open("user_kyle", "a.bin", "application/octet-stream")
	b, _ := s.open("user_kyle", "b.bin", "application/octet-stream")
	c, _ := s.open("user_kyle", "c.bin", "application/octet-stream")
	d, _ := s.open("user_kyle", "d.bin", "application/octet-stream")
	ua, roomA, _ := s.begin(a.secret, 20<<20)
	_, roomB, status := s.begin(b.secret, 15<<20)
	if status != 0 || roomA != 20<<20 || roomB != 15<<20 {
		t.Fatalf("rooms = %d, %d (status %d)", roomA, roomB, status)
	}
	if _, _, status := s.begin(c.secret, 10<<20); status != http.StatusServiceUnavailable {
		t.Errorf("no room left: begin = %d, want 503", status)
	}
	if _, _, status := s.begin(c.secret, maxAttachmentBytes+1); status != http.StatusRequestEntityTooLarge {
		t.Errorf("declared over the limit: begin = %d, want 413", status)
	}
	if _, _, status := s.begin(c.secret, 0); status != http.StatusBadRequest {
		t.Errorf("declared empty: begin = %d, want 400", status)
	}
	if _, _, status := s.begin(a.secret, 1); status != http.StatusConflict {
		t.Errorf("second PUT to one slot = %d, want 409", status)
	}
	s.finish(ua, nil) // a failed PUT releases its room
	if _, room, status := s.begin(c.secret, 10<<20); status != 0 || room != 10<<20 {
		t.Errorf("after release: %d, %d", room, status)
	}
	// A chunked PUT declares nothing, so it has to be given the most it may send.
	if _, room, status := s.begin(d.secret, -1); status != http.StatusServiceUnavailable || room != 0 {
		t.Errorf("chunked with 15 MB free: %d, %d; want 503", room, status)
	}
}

// A slot that expires while its PUT runs is not swept from under it: the
// reservation stays counted, and finish reports the slot gone instead of
// storing data nobody can use.
func TestUploadExpiringMidPut(t *testing.T) {
	s := newUploadStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	u, _ := s.open("user_kyle", "a.bin", "application/octet-stream")
	got, _, _ := s.begin(u.secret, 1000)
	now = now.Add(unfilledTTL + time.Minute)
	s.sweep()
	if s.held != 1000 || len(s.byID) != 1 {
		t.Errorf("mid-PUT sweep: held %d, slots %d", s.held, len(s.byID))
	}
	if s.finish(got, make([]byte, 1000)) {
		t.Error("finish stored data in an expired slot")
	}
	if s.held != 0 || len(s.byID) != 0 {
		t.Errorf("after finish: held %d, slots %d", s.held, len(s.byID))
	}
}

// The PUT handler says so when its slot expired mid-PUT.
func TestUploadHandlerGoneMidPut(t *testing.T) {
	s := newUploadStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	srv := newTestHTTP(t, s.handler(quietLogger()))
	u, _ := s.open("user_kyle", "a.bin", "application/octet-stream")
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPut, srv+uploadsPath+u.secret, pr)
	req.ContentLength = 4
	done := make(chan *http.Response)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	_, _ = pw.Write([]byte("ab"))
	// Wait until the handler has reserved the slot, then let it expire.
	for i := 0; ; i++ {
		s.mu.Lock()
		r := s.byID[u.id].reserved
		s.mu.Unlock()
		if r > 0 {
			break
		}
		if i > 200 {
			t.Fatal("PUT never began")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	now = now.Add(unfilledTTL + time.Minute)
	s.mu.Unlock()
	_, _ = pw.Write([]byte("cd"))
	_ = pw.Close()
	if resp := <-done; resp == nil || resp.StatusCode != http.StatusGone {
		t.Errorf("resp = %+v, want 410", resp)
	}
}

// A slow PUT outlasts the server's ReadTimeout and WriteTimeout, which are
// moved out for it, and only for it.
func TestUploadPutOutlastsServerTimeouts(t *testing.T) {
	s := newUploadStore()
	srv := httptest.NewUnstartedServer(s.handler(quietLogger()))
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	u, _ := s.open("user_kyle", "a.bin", "application/octet-stream")

	pr, pw := io.Pipe()
	go func() {
		for range 8 {
			_, _ = pw.Write(bytes.Repeat([]byte("x"), 1000))
			time.Sleep(100 * time.Millisecond)
		}
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPut, srv.URL+uploadsPath+u.secret, pr)
	req.ContentLength = 8000
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("slow PUT: %v", err)
	}
	defer resp.Body.Close()
	var rc uploadReceipt
	if err := json.NewDecoder(resp.Body).Decode(&rc); err != nil || resp.StatusCode != http.StatusOK || rc.Size != 8000 {
		t.Errorf("slow PUT: status %d, receipt %+v, err %v", resp.StatusCode, rc, err)
	}
}

// A send of an upload frees it; a draft keeps it, so the draft can be sent.
func TestSendReleasesUploads(t *testing.T) {
	f := newFakeGmail(t)
	s := newUploadStore()
	tl := &tools{gmail: f.client(), uploads: s, log: quietLogger()}
	data := []byte("photo bytes")
	u := fill(t, s, data)
	in := composeInput{To: []string{"a@example.com"}, Subject: "x", Body: "x", Attachments: []attachmentInput{{UploadID: u.id, Filename: "a.bin", SHA256: sum(data)}}}

	r, _, _ := tl.draft(signedIn("user_kyle"), nil, in)
	if r == nil || r.IsError {
		t.Fatalf("draft: %s", resultText(r))
	}
	if !strings.Contains(resultText(r), sum(data)) {
		t.Errorf("draft result has no attachment summary: %s", resultText(r))
	}
	r, _, _ = tl.send(signedIn("user_kyle"), nil, in)
	if r == nil || r.IsError {
		t.Fatalf("send: %s", resultText(r))
	}
	if !strings.Contains(resultText(r), `"size": 11`) {
		t.Errorf("send result has no attachment summary: %s", resultText(r))
	}
	if s.held != 0 || len(s.byID) != 0 {
		t.Errorf("after send: held %d, slots %d", s.held, len(s.byID))
	}
	r, _, _ = tl.send(signedIn("user_kyle"), nil, in)
	if r == nil || !r.IsError || !strings.Contains(resultText(r), "no such upload") {
		t.Errorf("second send of one upload: %s", resultText(r))
	}
}

// A send Gmail refuses keeps the upload for a retry.
func TestRefusedSendKeepsUpload(t *testing.T) {
	f := newFakeGmail(t)
	f.uploadStatus = http.StatusBadRequest
	s := newUploadStore()
	tl := &tools{gmail: f.client(), uploads: s, log: quietLogger()}
	u := fill(t, s, []byte("x"))
	r, _, _ := tl.send(signedIn("user_kyle"), nil, composeInput{To: []string{"a@example.com"}, Subject: "x", Body: "x", Attachments: []attachmentInput{{UploadID: u.id, Filename: "a.bin", SHA256: sum([]byte("x"))}}})
	if r == nil || !r.IsError {
		t.Fatalf("send: %s", resultText(r))
	}
	if len(s.byID) != 1 {
		t.Errorf("refused send dropped the upload")
	}
}

// Two sends of one upload at once: one claims it, the other is refused, and
// only one message reaches Gmail.
func TestConcurrentSendsOfOneUpload(t *testing.T) {
	f := newFakeGmail(t)
	f.mediaDelay = 300 * time.Millisecond
	s := newUploadStore()
	tl := &tools{gmail: f.client(), uploads: s, heavySlots: make(chan struct{}, maxHeavyCalls), log: quietLogger()}
	data := []byte("photo bytes")
	u := fill(t, s, data)
	in := composeInput{To: []string{"a@example.com"}, Subject: "x", Body: "x", Attachments: []attachmentInput{{UploadID: u.id, Filename: "a.bin", SHA256: sum(data)}}}
	results := make(chan *mcp.CallToolResult, 2)
	for range 2 {
		go func() {
			r, _, _ := tl.send(signedIn("user_kyle"), nil, in)
			results <- r
		}()
	}
	var ok, busy int
	for range 2 {
		r := <-results
		switch {
		case r != nil && !r.IsError:
			ok++
		case r != nil && strings.Contains(resultText(r), "already being sent"):
			busy++
		default:
			t.Errorf("unexpected result: %s", resultText(r))
		}
	}
	if ok != 1 || busy != 1 {
		t.Errorf("ok %d, busy %d; want 1 and 1", ok, busy)
	}
	f.mu.Lock()
	n := 0
	for _, c := range f.calls {
		if c.Path == "/upload/messages/send" {
			n++
		}
	}
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("%d sends reached Gmail, want 1", n)
	}
}

// The name restated with an upload_id is the one the person approving sees,
// so it must be the upload's own.
func TestUploadNeedsItsOwnName(t *testing.T) {
	f := newFakeGmail(t)
	s := newUploadStore()
	tl := &tools{gmail: f.client(), uploads: s, log: quietLogger()}
	data := []byte("x")
	u := fill(t, s, data)
	for name, a := range map[string]attachmentInput{
		"other name": {UploadID: u.id, Filename: "holiday.jpg", SHA256: sum(data)},
		"other type": {UploadID: u.id, Filename: "a.bin", MimeType: "image/png", SHA256: sum(data)},
		"wrong size": {UploadID: u.id, Filename: "a.bin", Size: 2, SHA256: sum(data)},
	} {
		r, _, _ := tl.send(signedIn("user_kyle"), nil, composeInput{To: []string{"a@example.com"}, Subject: "x", Body: "x", Attachments: []attachmentInput{a}})
		if r == nil || !r.IsError || !strings.Contains(resultText(r), "Nothing was sent") {
			t.Errorf("%s: %s", name, resultText(r))
		}
	}
	// None of those refusals left the upload claimed.
	r, _, _ := tl.send(signedIn("user_kyle"), nil, composeInput{To: []string{"a@example.com"}, Subject: "x", Body: "x",
		Attachments: []attachmentInput{{UploadID: u.id, Filename: "a.bin", MimeType: "application/octet-stream", Size: 1, SHA256: sum(data)}}})
	if r == nil || r.IsError {
		t.Errorf("matching send: %s", resultText(r))
	}
}

// A refusal partway through the attachments hands back the uploads already
// claimed.
func TestRefusedAttachmentUnclaimsEarlierUploads(t *testing.T) {
	f := newFakeGmail(t)
	s := newUploadStore()
	tl := &tools{gmail: f.client(), uploads: s, log: quietLogger()}
	u := fill(t, s, []byte("x"))
	good := attachmentInput{UploadID: u.id, Filename: "a.bin", SHA256: sum([]byte("x"))}
	r, _, _ := tl.send(signedIn("user_kyle"), nil, composeInput{To: []string{"a@example.com"}, Subject: "x", Body: "x",
		Attachments: []attachmentInput{good, {Filename: "a.exe", ContentBase64: "eA=="}}})
	if r == nil || !r.IsError {
		t.Fatalf("send: %s", resultText(r))
	}
	if s.byID[u.id].sending {
		t.Error("upload left claimed after a refusal")
	}
}

// Bytes that contradict the slot's name or type are refused at the PUT.
func TestUploadRefusesContradictingBytes(t *testing.T) {
	s := newUploadStore()
	srv := newTestHTTP(t, s.handler(quietLogger()))
	u, _ := s.open("user_kyle", "photo.jpg", "image/jpeg")
	resp, body := put(t, srv+uploadsPath+u.secret, []byte("<!DOCTYPE html><html><script>alert(1)</script>"))
	if resp.StatusCode != http.StatusUnsupportedMediaType || !strings.Contains(string(body), "text/html") {
		t.Errorf("status %d: %s", resp.StatusCode, body)
	}
	if s.held != 0 || s.byID[u.id].filled {
		t.Errorf("refused PUT kept data: held %d", s.held)
	}
	jpeg := append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), make([]byte, 100)...)
	if resp, body := put(t, srv+uploadsPath+u.secret, jpeg); resp.StatusCode != http.StatusOK {
		t.Errorf("real JPEG after a refusal: %d %s", resp.StatusCode, body)
	}
}
