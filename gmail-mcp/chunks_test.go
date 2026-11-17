package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpClient(t *testing.T) (*mcp.ClientSession, *fakeGmail, string) {
	t.Helper()
	as := newFakeAS(t)
	g := newFakeGmail(t)
	srv := testServer(t, as, g)
	token := as.mint(t, "user_kyle", testResource, time.Now().Add(time.Hour))
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, g, srv.URL
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

// The chunked route as a client with no way to the PUT sees it: create_upload,
// chunks over MCP, then send_message naming the upload.
func TestChunkedUploadEndToEnd(t *testing.T) {
	cs, g, _ := mcpClient(t)
	res := callTool(t, cs, "create_upload", map[string]any{"filename": "orvis-waist-snap.jpeg"})
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
		a := map[string]any{"upload_id": up.UploadID, "index": i, "content_base64": base64.StdEncoding.EncodeToString(parts[i]), "chunk_sha256": sum(parts[i])}
		if i == 0 {
			a["size"] = len(photo)
		}
		if last {
			a["last"], a["sha256"] = true, sum(photo)
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

	if r := decode(callTool(t, cs, "append_upload_chunk", chunkArgs(0, false))); r.ChunksReceived != 1 || r.BytesReceived != maxChunkBytes || r.Complete || r.ChunkSHA256 != sum(parts[0]) {
		t.Errorf("chunk 0: %+v", r)
	}
	// Out of order is refused, and says which index comes next.
	if res := callTool(t, cs, "append_upload_chunk", chunkArgs(2, false)); !res.IsError || !strings.Contains(resultText(res), "next is index 1") {
		t.Errorf("chunk 2 before 1: %s", resultText(res))
	}
	r := decode(callTool(t, cs, "append_upload_chunk", chunkArgs(1, false)))
	// A retry of the newest chunk, as after a lost answer, adds nothing.
	if again := decode(callTool(t, cs, "append_upload_chunk", chunkArgs(1, false))); again.BytesReceived != r.BytesReceived || !strings.Contains(again.Next, "already in") {
		t.Errorf("retried chunk 1: %+v, first %+v", again, r)
	}
	// A mistyped chunk is refused alone, and the upload goes on.
	bad := chunkArgs(2, false)
	bad["content_base64"] = base64.StdEncoding.EncodeToString(append([]byte{0x43}, parts[2][1:]...))
	if res := callTool(t, cs, "append_upload_chunk", bad); !res.IsError || !strings.Contains(resultText(res), "send chunk 2 again") {
		t.Errorf("mistyped chunk: %s", resultText(res))
	}

	// Not usable before the last chunk.
	attach := []map[string]any{{"upload_id": up.UploadID, "filename": "orvis-waist-snap.jpeg", "size": len(photo), "sha256": sum(photo)}}
	if res := callTool(t, cs, "send_message", map[string]any{"to": []string{"claims@example.com"}, "subject": "Claim", "body": "Photo attached.", "attachments": attach}); !res.IsError || !strings.Contains(resultText(res), "last=true") {
		t.Errorf("send before the last chunk: %s", resultText(res))
	}
	// A slot being filled by chunks takes no PUT.
	if resp, body := put(t, up.UploadURL, photo); resp.StatusCode != http.StatusConflict {
		t.Errorf("PUT to a chunked slot = %d %s, want 409", resp.StatusCode, body)
	}

	done := decode(callTool(t, cs, "append_upload_chunk", chunkArgs(2, true)))
	if !done.Complete || done.Receipt == nil || done.Receipt.SHA256 != sum(photo) || done.Receipt.Size != len(photo) || done.Receipt.DetectedType != "image/jpeg" {
		t.Fatalf("last chunk: %+v", done)
	}
	// The last chunk again, its answer lost, gets the same receipt.
	if again := decode(callTool(t, cs, "append_upload_chunk", chunkArgs(2, true))); again.Receipt == nil || again.Receipt.SHA256 != sum(photo) {
		t.Errorf("retried last chunk: %+v", again)
	}
	if res := callTool(t, cs, "append_upload_chunk", chunkArgs(1, false)); !res.IsError || !strings.Contains(resultText(res), "already complete") {
		t.Errorf("chunk after completion: %s", resultText(res))
	}

	if res := callTool(t, cs, "send_message", map[string]any{"to": []string{"claims@example.com"}, "subject": "Claim", "body": "Photo attached.", "attachments": attach}); res.IsError {
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

// The tool's own checks: the size of a chunk is judged before decoding, and
// a full store is a busy refusal, not a bad-input one.
func TestChunkToolRefusals(t *testing.T) {
	cs, _, _ := mcpClient(t)
	res := callTool(t, cs, "create_upload", map[string]any{"filename": "a.bin"})
	var up uploadResult
	_ = json.Unmarshal([]byte(resultText(res)), &up)

	huge := strings.Repeat("A", base64.StdEncoding.EncodedLen(maxChunkBytes*2)+8)
	if res := callTool(t, cs, "append_upload_chunk", map[string]any{"upload_id": up.UploadID, "index": 0, "size": 10, "content_base64": huge}); !res.IsError || !strings.Contains(resultText(res), "at most") {
		t.Errorf("oversized base64: %s", resultText(res))
	}
	if res := callTool(t, cs, "append_upload_chunk", map[string]any{"upload_id": up.UploadID, "index": 0, "size": 10, "content_base64": "not base64!"}); !res.IsError || !strings.Contains(resultText(res), "not valid base64") {
		t.Errorf("bad base64: %s", resultText(res))
	}
	// Room for the 40 MB is reserved at chunk 0: two 20 MB files take it all,
	// and a third is refused as busy before any more is written.
	reserve := func(size int) *mcp.CallToolResult {
		res := callTool(t, cs, "create_upload", map[string]any{"filename": "big.bin"})
		var u uploadResult
		_ = json.Unmarshal([]byte(resultText(res)), &u)
		return callTool(t, cs, "append_upload_chunk", map[string]any{"upload_id": u.UploadID, "index": 0, "size": size, "content_base64": base64.StdEncoding.EncodeToString([]byte("x"))})
	}
	for range 2 {
		if res := reserve(20 << 20); res.IsError {
			t.Fatalf("reserve 20 MB: %s", resultText(res))
		}
	}
	if res := reserve(1 << 20); !res.IsError || !strings.Contains(resultText(res), "send chunk 0 again in a few minutes") {
		t.Errorf("over the held cap: %s", resultText(res))
	}
}

func appendTo(t *testing.T, s *uploadStore, u *upload, index int, data []byte, size int) error {
	t.Helper()
	_, err := s.appendChunk("user_kyle", chunk{id: u.id, index: index, data: data, size: size})
	return err
}

func TestChunkRules(t *testing.T) {
	s := newUploadStore()
	u, _ := s.open("user_kyle", "a.txt", "text/plain")

	for name, c := range map[string]chunk{
		"empty":              {id: u.id, size: 1},
		"too big":            {id: u.id, data: make([]byte, maxChunkBytes+1), size: maxChunkBytes + 1},
		"chunk 0, no size":   {id: u.id, data: []byte("x")},
		"file over 25 MB":    {id: u.id, data: []byte("x"), size: maxAttachmentBytes + 1},
		"last, no sha256":    {id: u.id, data: []byte("x"), size: 1, last: true},
		"last, bad sha256":   {id: u.id, data: []byte("x"), size: 1, last: true, sha256: "abc"},
		"wrong chunk_sha256": {id: u.id, data: []byte("x"), size: 1, chunkSHA256: sum([]byte("y"))},
		"bad chunk_sha256":   {id: u.id, data: []byte("x"), size: 1, chunkSHA256: "xyz"},
	} {
		if _, err := s.appendChunk("user_kyle", c); !errors.Is(err, errBadInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Another subject's upload is unknown to it.
	if _, err := s.appendChunk("user_eve", chunk{id: u.id, data: []byte("x"), size: 1}); err == nil || !strings.Contains(err.Error(), "no such upload") {
		t.Errorf("other subject: err = %v", err)
	}
	if v := s.byID[u.id]; v.chunks != 0 || s.held != 0 {
		t.Fatalf("refused chunks were kept: chunks %d, held %d", v.chunks, s.held)
	}

	// Retries: the newest index again with other bytes is refused, and the
	// message does not claim the chunk already in was not added.
	if err := appendTo(t, s, u, 0, []byte("hello"), 10); err != nil {
		t.Fatal(err)
	}
	if err := appendTo(t, s, u, 0, []byte("HELLO"), 10); err == nil || !strings.Contains(err.Error(), "chunk 0 is already in, with other bytes") || strings.Contains(err.Error(), "chunk 0 was not added") {
		t.Errorf("newest index, other bytes: err = %v", err)
	}
	if err := appendTo(t, s, u, 1, []byte("world"), 11); err == nil || !strings.Contains(err.Error(), "not the 10 chunk 0 gave") {
		t.Errorf("a later chunk with another size: err = %v", err)
	}
	// The final chunk sent without last can still be closed, by resending it
	// with last or by an empty chunk with last.
	if err := appendTo(t, s, u, 1, []byte("world"), 0); err != nil {
		t.Fatal(err)
	}
	p, err := s.appendChunk("user_kyle", chunk{id: u.id, index: 1, data: []byte("world"), last: true, sha256: sum([]byte("helloworld"))})
	if err != nil || p.receipt == nil || p.receipt.SHA256 != sum([]byte("helloworld")) {
		t.Fatalf("resent final chunk with last: %+v %v", p, err)
	}
	// The completed last chunk again, with other bytes but the file's hash,
	// is refused.
	if _, err := s.appendChunk("user_kyle", chunk{id: u.id, index: 1, data: []byte("WORLD"), last: true, sha256: sum([]byte("helloworld"))}); err == nil || !strings.Contains(err.Error(), "already complete") {
		t.Errorf("completed last chunk, other bytes: err = %v", err)
	}
	e, _ := s.open("user_kyle", "a.txt", "text/plain")
	_ = appendTo(t, s, e, 0, []byte("abc"), 3)
	if p, err := s.appendChunk("user_kyle", chunk{id: e.id, index: 1, last: true, sha256: sum([]byte("abc"))}); err != nil || p.receipt == nil {
		t.Errorf("empty closing chunk: %+v %v", p, err)
	}
	if p, err := s.appendChunk("user_kyle", chunk{id: e.id, index: 1, last: true, sha256: sum([]byte("abc"))}); err != nil || p.receipt == nil || !p.repeat {
		t.Errorf("empty closing chunk again: %+v %v", p, err)
	}
	if _, err := s.appendChunk("user_kyle", chunk{id: e.id, index: 1}); err == nil {
		t.Error("an empty chunk without last was taken")
	}

	// A wrong whole-file hash or size, or more bytes than chunk 0 gave,
	// discards the upload and frees its room.
	heldBefore := s.held
	for name, cs := range map[string][]chunk{
		"wrong sha256": {{index: 0, data: []byte("hello"), size: 10}, {index: 1, data: []byte("world"), last: true, sha256: sum([]byte("something else"))}},
		"short":        {{index: 0, data: []byte("hello"), size: 11}, {index: 1, data: []byte("world"), last: true, sha256: sum([]byte("helloworld"))}},
		"over size":    {{index: 0, data: []byte("hello"), size: 6}, {index: 1, data: []byte("world")}},
	} {
		v, _ := s.open("user_kyle", "a.txt", "text/plain")
		var err error
		for _, c := range cs {
			c.id = v.id
			_, err = s.appendChunk("user_kyle", c)
		}
		if err == nil || !strings.Contains(err.Error(), "discarded") {
			t.Errorf("%s: err = %v", name, err)
		}
		if _, ok := s.byID[v.id]; ok {
			t.Errorf("%s: upload kept", name)
		}
	}
	if s.held != heldBefore {
		t.Errorf("held = %d after discards, want %d", s.held, heldBefore)
	}

	// Bytes that contradict the name are refused once 512 bytes are in, not
	// at the end, and steered to a new name rather than a resend.
	jpg, _ := s.open("user_kyle", "photo.jpg", "image/jpeg")
	html := append([]byte("<html><script>alert(1)</script>"), bytes.Repeat([]byte(" "), 600)...)
	if _, err := s.appendChunk("user_kyle", chunk{id: jpg.id, data: html, size: 1 << 20}); err == nil || !strings.Contains(err.Error(), "look like text/html") || !strings.Contains(err.Error(), "right name and type") {
		t.Errorf("html as a jpeg: err = %v", err)
	}
	if _, ok := s.byID[jpg.id]; ok {
		t.Error("the mislabelled upload was kept")
	}
	// A file under 512 bytes is checked once all of it is in.
	tiny, _ := s.open("user_kyle", "photo.jpg", "image/jpeg")
	if _, err := s.appendChunk("user_kyle", chunk{id: tiny.id, data: []byte("<html>"), size: 6}); err == nil || !strings.Contains(err.Error(), "look like text/html") {
		t.Errorf("short html as a jpeg: err = %v", err)
	}

	// A slot filled by PUT takes no chunks, nor does one with a PUT running.
	full := fill(t, s, []byte("0123456789"))
	if _, err := s.appendChunk("user_kyle", chunk{id: full.id, data: []byte("x"), size: 1}); err == nil || !strings.Contains(err.Error(), "already complete") {
		t.Errorf("chunk to a PUT-filled slot: err = %v", err)
	}
	putting, _ := s.open("user_kyle", "a.bin", "application/octet-stream")
	pu, _, _ := s.begin(putting.secret, 10)
	if _, err := s.appendChunk("user_kyle", chunk{id: putting.id, data: []byte("x"), size: 1}); err == nil || !strings.Contains(err.Error(), "PUT") {
		t.Errorf("chunk during a PUT: err = %v", err)
	}
	s.finish(pu, nil)
}

// Chunk 0 reserves the whole file against the 40 MB held, as a PUT reserves
// its Content-Length, so a file that will not fit is refused before the rest
// is written out, and one that is admitted cannot be starved.
func TestChunksReserveRoom(t *testing.T) {
	s := newUploadStore()
	a, _ := s.open("user_kyle", "a.bin", "application/octet-stream")
	if err := appendTo(t, s, a, 0, []byte("x"), 25<<20); err != nil {
		t.Fatal(err)
	}
	if s.held != 25<<20 {
		t.Errorf("held = %d after chunk 0, want the whole 25 MB", s.held)
	}
	b, _ := s.open("user_kyle", "b.bin", "application/octet-stream")
	if err := appendTo(t, s, b, 0, []byte("x"), 16<<20); !errors.Is(err, errUploadHeld) {
		t.Errorf("over the held cap: err = %v", err)
	}
	if v := s.byID[b.id]; v.chunks != 0 || s.held != 25<<20 {
		t.Errorf("a refused chunk 0 kept something: chunks %d, held %d", v.chunks, s.held)
	}
	// The reservation shuts out a PUT too.
	c, _ := s.open("user_kyle", "c.bin", "application/octet-stream")
	if _, _, status := s.begin(c.secret, 16<<20); status != http.StatusServiceUnavailable {
		t.Errorf("PUT with the room reserved: %d, want 503", status)
	}
	// Chunk 0 of b fits once a gives up its room.
	s.mu.Lock()
	s.drop(s.byID[a.id])
	s.mu.Unlock()
	if err := appendTo(t, s, b, 0, []byte("x"), 16<<20); err != nil {
		t.Errorf("chunk 0 again after room freed: %v", err)
	}
	s.mu.Lock()
	s.drop(s.byID[b.id])
	s.mu.Unlock()
	if s.held != 0 {
		t.Errorf("held = %d after dropping both", s.held)
	}
}

// Chunks to one upload from many goroutines at once: exactly one per index
// lands, the rest are refused, and the accounting holds. Run with -race.
func TestConcurrentChunks(t *testing.T) {
	s := newUploadStore()
	u, _ := s.open("user_kyle", "a.bin", "application/octet-stream")
	file := bytes.Repeat([]byte("abcdefgh"), 1024)
	chunkOf := func(i int) []byte { return file[i*1024 : (i+1)*1024] }
	_ = appendTo(t, s, u, 0, chunkOf(0), len(file))
	var wg sync.WaitGroup
	for i := 1; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.appendChunk("user_kyle", chunk{id: u.id, index: i, data: chunkOf(i)})
		}()
	}
	// And a PUT racing them for the same slot.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if got, _, status := s.begin(u.secret, 10); status == 0 {
			s.finish(got, nil)
			t.Error("a PUT was let into a chunked slot")
		}
	}()
	wg.Wait()
	v := s.byID[u.id]
	if v.partBytes != v.chunks*1024 || s.held != len(file) {
		t.Errorf("chunks %d, bytes %d, held %d", v.chunks, v.partBytes, s.held)
	}
}

// Each chunk moves the deadline out, so a slow sender is not cut off part way;
// a sender that stops loses the upload, and its room, unfilledTTL later.
func TestChunkedUploadExpiry(t *testing.T) {
	s := newUploadStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	u, _ := s.open("user_kyle", "a.txt", "text/plain")
	for i := range 3 {
		now = now.Add(unfilledTTL - time.Minute)
		if err := appendTo(t, s, u, i, []byte("abc"), 100); err != nil {
			t.Fatalf("chunk %d after %s: %v", i, time.Duration(i+1)*(unfilledTTL-time.Minute), err)
		}
	}
	now = now.Add(unfilledTTL + time.Second)
	if err := appendTo(t, s, u, 3, []byte("abc"), 0); err == nil || !strings.Contains(err.Error(), "no such upload") {
		t.Errorf("after going quiet: err = %v", err)
	}
	if s.held != 0 || len(s.byID) != 0 {
		t.Errorf("held = %d, slots = %d", s.held, len(s.byID))
	}

	// Once complete, it lasts uploadTTL from the last chunk.
	v, _ := s.open("user_kyle", "a.txt", "text/plain")
	if _, err := s.appendChunk("user_kyle", chunk{id: v.id, data: []byte("abc"), size: 3, last: true, sha256: sum([]byte("abc"))}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(uploadTTL - time.Second)
	if _, err := s.get("user_kyle", want{id: v.id, sha256: sum([]byte("abc")), filename: "a.txt"}, false); err != nil {
		t.Errorf("complete upload before uploadTTL: %v", err)
	}
}

// When every slot is taken, the slot idle longest gives way, empty or
// part-filled, but never one touched within evictGrace; and an evicted
// upload's next chunk is told why.
func TestOpenEvictsIdlestSlot(t *testing.T) {
	s := newUploadStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	var slots []*upload
	for range maxOpenUploads {
		u, _ := s.open("user_kyle", "a.txt", "text/plain")
		slots = append(slots, u)
		now = now.Add(time.Second)
	}
	// Slot 0 is the oldest, but got a chunk just now. Slot 1 is next oldest.
	now = now.Add(2 * evictGrace)
	if err := appendTo(t, s, slots[0], 0, []byte("x"), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.open("user_kyle", "a.txt", "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.byID[slots[0].id]; !ok {
		t.Error("a slot that got a chunk just now was evicted")
	}
	if _, ok := s.byID[slots[1].id]; ok {
		t.Error("the slot idle longest was kept")
	}
	if err := appendTo(t, s, slots[1], 0, []byte("x"), 10); err == nil || !strings.Contains(err.Error(), "discarded to make room") {
		t.Errorf("chunk to an evicted slot: err = %v", err)
	}
	if _, err := s.appendChunk("user_eve", chunk{id: slots[1].id, data: []byte("x"), size: 1}); err == nil || strings.Contains(err.Error(), "make room") {
		t.Errorf("another subject learns of the eviction: err = %v", err)
	}
	// With every slot touched within evictGrace, none gives way.
	s.mu.Lock()
	for _, u := range s.byID {
		u.expires = now.Add(unfilledTTL)
	}
	s.mu.Unlock()
	if _, err := s.open("user_kyle", "a.txt", "text/plain"); !errors.Is(err, errUploadsFull) {
		t.Errorf("all slots fresh: err = %v", err)
	}
	// The eviction note is forgotten after unfilledTTL.
	now = now.Add(unfilledTTL + time.Second)
	s.mu.Lock()
	s.sweep()
	n := len(s.evicted)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d eviction notes left after unfilledTTL", n)
	}
}
