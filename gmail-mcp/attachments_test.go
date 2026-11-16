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
	_, parts := parseMixed(t, raw)
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
	if strings.Contains(raw, "multipart") || !strings.Contains(raw, "Content-Type: text/plain; charset=\"UTF-8\"\r\n") {
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
		"too many":           many,
		"inline too big":     {{Filename: "a.bin", ContentBase64: big}},
		"inline total":       {{Filename: "a.bin", ContentBase64: half}, {Filename: "b.bin", ContentBase64: half}},
		"no content":         {{Filename: "a.txt"}},
		"empty":              {{Filename: "a.txt", ContentBase64: "===="}},
		"not base64":         {{Filename: "a.txt", ContentBase64: "%%%"}},
		"header in filename": {{Filename: "a\r\nBcc: x@example.com", ContentBase64: ok}},
		"blocked type":       {{Filename: "a.exe", ContentBase64: ok}},
		"bad mime":           {{Filename: "a.txt", MimeType: "text/plain; boundary=x", ContentBase64: ok}},
		"unknown upload":     {{UploadID: "up_nope"}},
		"upload plus inline": {{UploadID: "up_nope", ContentBase64: ok}},
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
		"attachments": []map[string]any{{"upload_id": up.UploadID}},
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
	sum := sha256.Sum256(photo)
	if rc.Size != len(photo) || rc.SHA256 != hex.EncodeToString(sum[:]) || rc.DetectedType != "image/jpeg" || rc.UploadID != up.UploadID {
		t.Errorf("receipt = %+v", rc)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"to": []string{"claims@example.com"}, "subject": "Claim", "body": "Photo attached.", "from": "alias@example.org",
		"attachments": []map[string]any{{"upload_id": up.UploadID}},
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
	u, err := s.open("user_kyle", "a.jpg", "image/jpeg")
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

	if att, err := s.get("user_kyle", u.id); err != nil || string(att.Data) != "ok" {
		t.Errorf("get = %+v, %v", att, err)
	}
	if _, err := s.get("user_other", u.id); !errors.Is(err, errBadInput) {
		t.Errorf("another subject: err = %v", err)
	}
	if s.held != 2 {
		t.Errorf("held = %d, want 2", s.held)
	}
}

func TestUploadsExpireAndAreCapped(t *testing.T) {
	s := newUploadStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	first, _ := s.open("user_kyle", "a.txt", "text/plain")
	for range maxOpenUploads - 1 {
		if _, err := s.open("user_kyle", "a.txt", "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.open("user_kyle", "a.txt", "text/plain"); !errors.Is(err, errUploadsFull) {
		t.Fatalf("over the cap: err = %v", err)
	}
	if _, err := s.open("", "a.txt", "text/plain"); !errors.Is(err, errBadInput) {
		t.Errorf("no subject: err = %v", err)
	}

	got, room, status := s.begin(first.secret)
	if status != 0 || room != maxAttachmentBytes {
		t.Fatalf("begin = %d, %d", room, status)
	}
	s.finish(got, make([]byte, 10))

	now = now.Add(uploadTTL + time.Second)
	if _, err := s.get("user_kyle", first.id); !errors.Is(err, errBadInput) {
		t.Errorf("expired: err = %v", err)
	}
	if _, err := s.open("user_kyle", "a.txt", "text/plain"); err != nil {
		t.Errorf("after expiry: %v", err)
	}
	if s.held != 0 {
		t.Errorf("held = %d after expiry", s.held)
	}
}

// Room is shared: two large uploads in flight cannot both claim the full
// allowance.
func TestUploadReservationsShareRoom(t *testing.T) {
	s := newUploadStore()
	a, _ := s.open("user_kyle", "a.bin", "application/octet-stream")
	b, _ := s.open("user_kyle", "b.bin", "application/octet-stream")
	c, _ := s.open("user_kyle", "c.bin", "application/octet-stream")
	ua, roomA, _ := s.begin(a.secret)
	_, roomB, _ := s.begin(b.secret)
	if roomA != maxAttachmentBytes || roomB != maxHeldBytes-maxAttachmentBytes {
		t.Errorf("rooms = %d, %d", roomA, roomB)
	}
	if _, _, status := s.begin(c.secret); status != http.StatusServiceUnavailable {
		t.Errorf("third begin = %d, want 503", status)
	}
	if _, _, status := s.begin(a.secret); status != http.StatusConflict {
		t.Errorf("second PUT to one slot = %d, want 409", status)
	}
	s.finish(ua, nil) // a failed PUT releases its room
	if _, room, status := s.begin(c.secret); status != 0 || room != maxAttachmentBytes {
		t.Errorf("after release: %d, %d", room, status)
	}
}
