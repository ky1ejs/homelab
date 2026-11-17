package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The chunked route as a client with no way to the PUT sees it: create_upload,
// chunks over MCP, then send_message naming the upload.
func TestChunkedUploadEndToEnd(t *testing.T) {
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
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}

	res := call("create_upload", map[string]any{"filename": "orvis-waist-snap.jpeg"})
	var up uploadResult
	if err := json.Unmarshal([]byte(resultText(res)), &up); err != nil || res.IsError {
		t.Fatalf("create_upload: %v %s", err, resultText(res))
	}
	if !strings.Contains(up.Next, "append_upload_chunk") {
		t.Errorf("create_upload's next step does not mention the chunked route: %s", up.Next)
	}

	photo := append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), bytes.Repeat([]byte{0x42}, 600000)...)
	parts := [][]byte{photo[:maxChunkBytes], photo[maxChunkBytes : 2*maxChunkBytes], photo[2*maxChunkBytes:]}
	chunkArgs := func(i int, last bool) map[string]any {
		a := map[string]any{"upload_id": up.UploadID, "index": i, "content_base64": base64.StdEncoding.EncodeToString(parts[i])}
		if last {
			a["last"], a["size"], a["sha256"] = true, len(photo), sum(photo)
		}
		return a
	}
	decode := func(res *mcp.CallToolResult) chunkResult {
		t.Helper()
		var r chunkResult
		if res.IsError {
			t.Fatalf("append_upload_chunk refused: %s", resultText(res))
		}
		if err := json.Unmarshal([]byte(resultText(res)), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	if r := decode(call("append_upload_chunk", chunkArgs(0, false))); r.ChunksReceived != 1 || r.BytesReceived != maxChunkBytes || r.Complete {
		t.Errorf("chunk 0: %+v", r)
	}
	// Out of order is refused, and says which index comes next.
	if res := call("append_upload_chunk", chunkArgs(2, false)); !res.IsError || !strings.Contains(resultText(res), "index 1") {
		t.Errorf("chunk 2 before 1: %s", resultText(res))
	}
	r := decode(call("append_upload_chunk", chunkArgs(1, false)))
	// A retry of the newest chunk, as after a lost answer, adds nothing.
	if again := decode(call("append_upload_chunk", chunkArgs(1, false))); again.BytesReceived != r.BytesReceived || !strings.Contains(again.Next, "already in") {
		t.Errorf("retried chunk 1: %+v, first %+v", again, r)
	}

	// Not usable before the last chunk.
	attach := []map[string]any{{"upload_id": up.UploadID, "filename": "orvis-waist-snap.jpeg", "size": len(photo), "sha256": sum(photo)}}
	if res := call("send_message", map[string]any{"to": []string{"claims@example.com"}, "subject": "Claim", "body": "Photo attached.", "attachments": attach}); !res.IsError || !strings.Contains(resultText(res), "last=true") {
		t.Errorf("send before the last chunk: %s", resultText(res))
	}
	// A slot being filled by chunks takes no PUT.
	if resp, body := put(t, up.UploadURL, photo); resp.StatusCode != http.StatusConflict {
		t.Errorf("PUT to a chunked slot = %d %s, want 409", resp.StatusCode, body)
	}

	done := decode(call("append_upload_chunk", chunkArgs(2, true)))
	if !done.Complete || done.Receipt == nil || done.Receipt.SHA256 != sum(photo) || done.Receipt.Size != len(photo) || done.Receipt.DetectedType != "image/jpeg" {
		t.Fatalf("last chunk: %+v", done)
	}
	// The last chunk again, its answer lost, gets the same receipt.
	if again := decode(call("append_upload_chunk", chunkArgs(2, true))); again.Receipt == nil || again.Receipt.SHA256 != sum(photo) {
		t.Errorf("retried last chunk: %+v", again)
	}
	if res := call("append_upload_chunk", chunkArgs(1, false)); !res.IsError || !strings.Contains(resultText(res), "already complete") {
		t.Errorf("chunk after completion: %s", resultText(res))
	}

	if res := call("send_message", map[string]any{"to": []string{"claims@example.com"}, "subject": "Claim", "body": "Photo attached.", "attachments": attach}); res.IsError {
		t.Fatalf("send: %s", resultText(res))
	}
	u, ok := g.mediaAt("/messages/send")
	if !ok {
		t.Fatal("nothing reached the media endpoint")
	}
	_, got := parseMixed(t, u.raw)
	if len(got) != 2 || !bytes.Equal(got[1].data, photo) || got[1].filename != "orvis-waist-snap.jpeg" {
		t.Errorf("sent %d parts", len(got))
	}
}

func appendTo(t *testing.T, s *uploadStore, u *upload, index int, data []byte) error {
	t.Helper()
	_, err := s.appendChunk("user_kyle", chunk{id: u.id, index: index, data: data})
	return err
}

func TestChunkRules(t *testing.T) {
	s := newUploadStore()
	u, _ := s.open("user_kyle", "a.txt", "text/plain")

	for name, c := range map[string]chunk{
		"empty":            {id: u.id},
		"too big":          {id: u.id, data: make([]byte, maxChunkBytes+1)},
		"last, no sha256":  {id: u.id, data: []byte("x"), last: true, size: 1},
		"last, no size":    {id: u.id, data: []byte("x"), last: true, sha256: sum([]byte("x"))},
		"last, bad sha256": {id: u.id, data: []byte("x"), last: true, size: 1, sha256: "abc"},
	} {
		if _, err := s.appendChunk("user_kyle", c); !errors.Is(err, errBadInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Another subject's upload is unknown to it.
	if _, err := s.appendChunk("user_eve", chunk{id: u.id, data: []byte("x")}); err == nil || !strings.Contains(err.Error(), "no such upload") {
		t.Errorf("other subject: err = %v", err)
	}
	if u.chunks != 0 || s.held != 0 {
		t.Fatalf("refused chunks were kept: chunks %d, held %d", u.chunks, s.held)
	}

	// A wrong whole-file hash or size discards the upload and frees its bytes.
	for name, c := range map[string]chunk{
		"wrong sha256": {index: 1, data: []byte("world"), last: true, size: 10, sha256: sum([]byte("something else"))},
		"wrong size":   {index: 1, data: []byte("world"), last: true, size: 11, sha256: sum([]byte("helloworld"))},
	} {
		v, _ := s.open("user_kyle", "a.txt", "text/plain")
		if err := appendTo(t, s, v, 0, []byte("hello")); err != nil {
			t.Fatal(err)
		}
		c.id = v.id
		if _, err := s.appendChunk("user_kyle", c); err == nil || !strings.Contains(err.Error(), "discarded") {
			t.Errorf("%s: err = %v", name, err)
		}
		if _, ok := s.byID[v.id]; ok {
			t.Errorf("%s: upload kept", name)
		}
	}
	if s.held != 0 {
		t.Errorf("held = %d after discards", s.held)
	}

	// Bytes that contradict the name are refused when the file closes.
	jpg, _ := s.open("user_kyle", "photo.jpg", "image/jpeg")
	html := []byte("<html><script>alert(1)</script></html>")
	if _, err := s.appendChunk("user_kyle", chunk{id: jpg.id, data: html, last: true, size: len(html), sha256: sum(html)}); err == nil || !strings.Contains(err.Error(), "look like text/html") {
		t.Errorf("html as a jpeg: err = %v", err)
	}

	// A slot filled by PUT takes no chunks, nor does one with a PUT running.
	full := fill(t, s, []byte("0123456789"))
	if _, err := s.appendChunk("user_kyle", chunk{id: full.id, data: []byte("x")}); err == nil || !strings.Contains(err.Error(), "already complete") {
		t.Errorf("chunk to a PUT-filled slot: err = %v", err)
	}
	putting, _ := s.open("user_kyle", "a.bin", "application/octet-stream")
	pu, _, _ := s.begin(putting.secret, 10)
	if _, err := s.appendChunk("user_kyle", chunk{id: putting.id, data: []byte("x")}); err == nil || !strings.Contains(err.Error(), "PUT") {
		t.Errorf("chunk during a PUT: err = %v", err)
	}
	s.finish(pu, nil)
}

// Chunks count against the same 25 MB file limit and 40 MB of held bytes as
// PUTs, and a chunk that would not fit is not added.
func TestChunksShareTheCaps(t *testing.T) {
	s := newUploadStore()
	big := make([]byte, maxChunkBytes)
	a, _ := s.open("user_kyle", "a.bin", "application/octet-stream")
	n := maxAttachmentBytes / maxChunkBytes
	for i := range n {
		if err := appendTo(t, s, a, i, big); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	if err := appendTo(t, s, a, n, []byte("x")); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("over 25 MB: err = %v", err)
	}
	b, _ := s.open("user_kyle", "b.bin", "application/octet-stream")
	for i := 0; s.held+maxChunkBytes <= maxHeldBytes; i++ {
		if err := appendTo(t, s, b, i, big); err != nil {
			t.Fatalf("b chunk %d: %v", i, err)
		}
	}
	before := s.held
	if err := appendTo(t, s, b, s.byID[b.id].chunks, big); !errors.Is(err, errUploadHeld) {
		t.Errorf("over the held cap: err = %v", err)
	}
	if s.held != before {
		t.Errorf("held moved from %d to %d on a refused chunk", before, s.held)
	}
	// And a PUT finds no room either.
	c, _ := s.open("user_kyle", "c.bin", "application/octet-stream")
	if _, _, status := s.begin(c.secret, maxChunkBytes); status != http.StatusServiceUnavailable {
		t.Errorf("PUT with chunks holding the room: %d, want 503", status)
	}
	s.mu.Lock()
	s.drop(s.byID[a.id])
	s.drop(s.byID[b.id])
	s.mu.Unlock()
	if s.held != 0 {
		t.Errorf("held = %d after dropping both", s.held)
	}
}

// Each chunk moves the deadline out, so a slow sender is not cut off part way;
// a sender that stops loses the upload, and its bytes, unfilledTTL later.
func TestChunkedUploadExpiry(t *testing.T) {
	s := newUploadStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	u, _ := s.open("user_kyle", "a.txt", "text/plain")
	for i := range 3 {
		now = now.Add(unfilledTTL - time.Minute)
		if err := appendTo(t, s, u, i, []byte("abc")); err != nil {
			t.Fatalf("chunk %d after %s: %v", i, time.Duration(i+1)*(unfilledTTL-time.Minute), err)
		}
	}
	now = now.Add(unfilledTTL + time.Second)
	if err := appendTo(t, s, u, 3, []byte("abc")); err == nil || !strings.Contains(err.Error(), "no such upload") {
		t.Errorf("after going quiet: err = %v", err)
	}
	if s.held != 0 || len(s.byID) != 0 {
		t.Errorf("held = %d, slots = %d", s.held, len(s.byID))
	}

	// Once complete, it lasts uploadTTL from the last chunk.
	v, _ := s.open("user_kyle", "a.txt", "text/plain")
	if _, err := s.appendChunk("user_kyle", chunk{id: v.id, data: []byte("abc"), last: true, size: 3, sha256: sum([]byte("abc"))}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(uploadTTL - time.Second)
	if _, err := s.get("user_kyle", want{id: v.id, sha256: sum([]byte("abc")), filename: "a.txt"}, false); err != nil {
		t.Errorf("complete upload before uploadTTL: %v", err)
	}
}

// When every slot is taken, an empty one gives way before a part-filled one,
// and then the part-filled one that has waited longest for a chunk.
func TestOpenEvictsEmptyBeforePartial(t *testing.T) {
	s := newUploadStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	var partial []*upload
	for range maxOpenUploads - 1 {
		now = now.Add(time.Second)
		u, _ := s.open("user_kyle", "a.txt", "text/plain")
		partial = append(partial, u)
	}
	empty, _ := s.open("user_kyle", "a.txt", "text/plain")
	// The first slot opened gets its chunk last, so it waited least.
	for i := len(partial) - 1; i >= 0; i-- {
		now = now.Add(time.Second)
		if err := appendTo(t, s, partial[i], 0, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.open("user_kyle", "a.txt", "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.byID[empty.id]; ok {
		t.Error("the empty slot was kept over a part-filled one")
	}
	if _, err := s.open("user_kyle", "a.txt", "text/plain"); err != nil {
		t.Fatal(err)
	}
	// Now the new empty slot from the call above gives way, not a partial one.
	for _, u := range partial {
		if _, ok := s.byID[u.id]; !ok {
			t.Errorf("part-filled slot %s evicted while an empty one was open", u.id)
		}
	}
	// With only part-filled slots left, the one idle longest goes.
	s.mu.Lock()
	for _, u := range s.byID {
		if u.chunks == 0 {
			s.drop(u)
		}
	}
	s.mu.Unlock()
	for len(s.byID) < maxOpenUploads {
		u, _ := s.open("user_kyle", "a.txt", "text/plain")
		_ = appendTo(t, s, u, 0, []byte("x")) // newest activity
		now = now.Add(time.Second)
	}
	if _, err := s.open("user_kyle", "a.txt", "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.byID[partial[len(partial)-1].id]; ok {
		t.Error("the part-filled slot idle longest was kept")
	}
}
