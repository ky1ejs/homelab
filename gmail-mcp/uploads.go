package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Uploads: how a real file reaches an attachment.
//
// A file a person wants to send (a photo, a PDF) is far too large for a model
// to write out as base64 in a tool call. So create_upload, an authenticated
// tool, opens a slot and returns a capability URL; the client PUTs the bytes
// there from wherever the file is; send_message or create_draft then names the
// slot's id.
//
// The PUT is the one route on this server that takes no OAuth token, because
// the thing holding the file (an agent's shell) is not the thing holding the
// token (claude.ai). The URL's 256-bit secret is the credential instead, and
// it can do exactly one thing: fill one empty slot, once, within its lifetime,
// with at most maxAttachmentBytes. There is no GET: nothing on this server
// hands an upload back out, so a leaked URL cannot be used to read a file or
// to host one. Using an upload in mail still needs an OAuth token for the
// same subject that opened the slot, and the SHA-256 of the file it meant.
//
// A client that cannot reach the PUT route (a cloud sandbox whose egress is
// an allowlist) sends the file over MCP instead: append_upload_chunk, an
// authenticated tool, adds base64 chunks to the same slot in order. Chunk 0
// states the whole file's size, which is reserved then, as a PUT reserves its
// Content-Length; the last states its SHA-256, which is checked before the
// slot counts as filled. A slot takes one route or the other, never both.
// Every chunk is base64 a model writes out, so this route costs tokens the
// PUT does not, and it is built to fail early and to let one bad chunk be
// sent again rather than the whole file.
//
// Uploads live in memory only and go when they expire or the server restarts.
// The caps below keep the total inside the container's 256 MB.

const (
	// How long a filled upload can be attached, from when its PUT finished.
	uploadTTL = 30 * time.Minute
	// How long an empty slot waits for its PUT, or a chunked one for its next
	// chunk: each chunk moves the deadline out again.
	unfilledTTL   = 10 * time.Minute
	maxChunkBytes = 256 << 10 // decoded bytes in one append_upload_chunk
	maxChunks     = 4096      // per upload; bounds the per-chunk overhead
	// A slot touched more recently than this is never evicted to make room:
	// its sender is most likely about to PUT or send the next chunk.
	evictGrace     = time.Minute
	maxOpenUploads = 10       // slots, filled or not, across all subjects
	maxHeldBytes   = 40 << 20 // filled and in-flight bytes, across all slots
	uploadsPath    = "/uploads/"
	// A 25 MB PUT over a slow uplink needs longer than the server's 30s
	// ReadTimeout and 60s WriteTimeout, which are right for every other
	// request. Both are moved out for a PUT to a valid slot, and only then.
	uploadIOTimeout = 5 * time.Minute
)

var (
	errUploadUnknown  = errors.New("no such upload: it has expired, was used by a send, or the id is wrong. Call create_upload again")
	errUploadEmpty    = errors.New("that upload has no file yet: PUT the file to its upload_url, or finish its chunks with last=true, first")
	errUploadMismatch = errors.New("sha256 does not match the file in that upload, so it is not the file you meant. Nothing was sent or drafted")
	errUploadSending  = errors.New("that upload is already being sent by another call. This call sent nothing; check Sent before trying again")
	errUploadNotNamed = errors.New("that upload holds a different file: give the filename (and mime_type, if you give one) exactly as create_upload returned them")
	errUploadsFull    = errors.New("too many uploads are open. Use one already open, or wait up to 10 minutes for unused ones to expire")
	errUploadHeld     = errors.New("the server is holding too much upload data right now to reserve this file. Nothing was kept; send chunk 0 again in a few minutes")
	errUploadFilled   = errors.New("that upload is already complete. Use it in attachments, or open a new upload for another file")
	errUploadPutting  = errors.New("that upload is being filled by a PUT; it cannot take chunks as well")
	errUploadEvicted  = errors.New("that upload was discarded to make room for a newer one, after waiting over a minute for its next chunk or PUT. Open a new upload")
	errChunkTooBig    = fmt.Errorf("a chunk is at most %d bytes decoded; split it", maxChunkBytes)
)

type upload struct {
	id, secret string
	sub        string // the OAuth subject that opened it
	filename   string
	mimeType   string
	expires    time.Time
	opened     time.Time
	data       []byte
	sha256     string
	filled     bool
	reserved   int  // bytes held for a PUT in progress
	sending    bool // claimed by a send in progress; see get
	// A chunked upload: the size chunk 0 gave, held in uploadStore.held
	// from then on; the chunks so far and their total, until the last one
	// joins them into data; how many have been taken; the hash of the newest,
	// so a retried chunk whose answer was lost is recognised rather than
	// added twice; a running hash of the file; and whether its first bytes
	// have been checked against its name and type.
	chunkSize int
	parts     [][]byte
	partBytes int
	chunks    int
	lastPart  [32]byte
	running   hash.Hash
	sniffed   bool
}

type uploadStore struct {
	mu       sync.Mutex
	now      func() time.Time
	byID     map[string]*upload
	bySecret map[string]*upload
	held     int
	// Uploads evicted to make room, by id, with their subject, so the next
	// chunk is told why. Pruned after unfilledTTL.
	evicted map[string]evicted
}

type evicted struct {
	sub string
	at  time.Time
}

func newUploadStore() *uploadStore {
	return &uploadStore{now: time.Now, byID: map[string]*upload{}, bySecret: map[string]*upload{}, evicted: map[string]evicted{}}
}

// token is 32 random bytes, base64url without padding: 43 characters.
func token() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b)
}

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func (s *uploadStore) drop(u *upload) {
	s.held -= len(u.data) + u.reserved + u.chunkSize
	delete(s.byID, u.id)
	delete(s.bySecret, u.secret)
}

// sweep drops expired uploads. A slot with a PUT in flight is left for finish
// to settle, so its reservation stays counted while the PUT holds the bytes.
// Callers hold mu.
func (s *uploadStore) sweep() {
	now := s.now()
	for _, u := range s.byID {
		if u.reserved == 0 && !u.sending && now.After(u.expires) {
			s.drop(u)
		}
	}
	for id, e := range s.evicted {
		if now.Sub(e.at) > unfilledTTL {
			delete(s.evicted, id)
		}
	}
}

// signedInSubject is the request's verified OAuth subject. Unlike
// subjectFrom, which is for logs, it has no placeholder: an upload is bound to
// a real subject or not opened at all.
func signedInSubject(ctx context.Context) (string, error) {
	if s, ok := ctx.Value(subjectKey{}).(string); ok && s != "" {
		return s, nil
	}
	return "", fmt.Errorf("%w: no signed-in subject", errBadInput)
}

// open reserves a slot. filename and mimeType are already checked. When every
// slot is taken, the one still waiting for its file that has been idle
// longest gives way, empty or part-filled alike, so long as it has been idle
// for evictGrace: a filled upload is about to be attached, and one touched
// within the last minute is most likely about to get its PUT or next chunk.
func (s *uploadStore) open(sub, filename, mimeType string) (*upload, error) {
	if sub == "" {
		return nil, fmt.Errorf("%w: no signed-in subject", errBadInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	if len(s.byID) >= maxOpenUploads {
		var oldest *upload
		now := s.now()
		for _, u := range s.byID {
			// expires is unfilledTTL after the slot's last activity: its
			// opening, or its newest chunk.
			idle := now.Sub(u.expires.Add(-unfilledTTL))
			if u.filled || u.reserved > 0 || u.sending || idle < evictGrace {
				continue
			}
			if oldest == nil || u.expires.Before(oldest.expires) {
				oldest = u
			}
		}
		if oldest == nil {
			return nil, errUploadsFull
		}
		s.evicted[oldest.id] = evicted{sub: oldest.sub, at: now}
		s.drop(oldest)
	}
	now := s.now()
	u := &upload{id: "up_" + token(), secret: token(), sub: sub, filename: filename, mimeType: mimeType, opened: now, expires: now.Add(unfilledTTL)}
	s.byID[u.id] = u
	s.bySecret[u.secret] = u
	cp := *u
	return &cp, nil
}

// want is what a draft or send says it is attaching. The name and type are
// restated by the caller, so they are in the arguments a person approves, and
// the hash ties the bytes to the file the caller meant: whoever holds the URL
// fills the slot.
type want struct {
	id, sha256, filename, mimeType string
	size                           int
}

// get returns a filled upload's attachment, for the subject that opened it,
// and only if want matches it. Another subject's upload is reported as
// unknown, not as forbidden. With claim, the upload is also marked as being
// sent, and a second claim is refused until release or unclaim: that, not
// the order of calls, is what stops one upload going out twice.
func (s *uploadStore) get(sub string, w want, claim bool) (outgoingAttachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	u, ok := s.byID[w.id]
	if !ok || sub == "" || u.sub != sub {
		return outgoingAttachment{}, fmt.Errorf("%w: %s", errBadInput, errUploadUnknown)
	}
	if !u.filled {
		return outgoingAttachment{}, fmt.Errorf("%w: %s", errBadInput, errUploadEmpty)
	}
	if !strings.EqualFold(strings.TrimSpace(w.sha256), u.sha256) || (w.size != 0 && w.size != len(u.data)) {
		return outgoingAttachment{}, fmt.Errorf("%w: %s", errBadInput, errUploadMismatch)
	}
	if w.filename != u.filename || (w.mimeType != "" && !strings.EqualFold(strings.TrimSpace(w.mimeType), u.mimeType)) {
		return outgoingAttachment{}, fmt.Errorf("%w: %s", errBadInput, errUploadNotNamed)
	}
	if claim {
		if u.sending {
			return outgoingAttachment{}, fmt.Errorf("%w: %s", errBadInput, errUploadSending)
		}
		u.sending = true
	}
	// data is never written again once filled, so sharing it is safe.
	return outgoingAttachment{Filename: u.filename, MimeType: u.mimeType, Data: u.data}, nil
}

// release drops uploads once a send has used them, or may have, freeing their
// slot and bytes. A draft keeps them, so a draft can be followed by its send.
func (s *uploadStore) release(sub string, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if u, ok := s.byID[id]; ok && u.sub == sub && u.reserved == 0 {
			s.drop(u)
		}
	}
}

// unclaim hands uploads back after a send that definitely did not go, so they
// can be sent again.
func (s *uploadStore) unclaim(sub string, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if u, ok := s.byID[id]; ok && u.sub == sub {
			u.sending = false
		}
	}
}

// begin claims an empty slot for a PUT and reserves room for it. declared is
// the request's Content-Length, or -1 for a chunked body, which has to be
// given the most it could send. It returns how many bytes the PUT may write,
// or an HTTP status to refuse it with.
func (s *uploadStore) begin(secret string, declared int64) (*upload, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	u, ok := s.bySecret[secret]
	switch {
	case !ok:
		return nil, 0, http.StatusNotFound
	case u.filled || u.reserved > 0 || u.chunks > 0:
		return nil, 0, http.StatusConflict
	case declared == 0:
		return nil, 0, http.StatusBadRequest
	case declared > maxAttachmentBytes:
		return nil, 0, http.StatusRequestEntityTooLarge
	}
	need := maxAttachmentBytes
	if declared > 0 {
		need = int(declared)
	}
	if need > maxHeldBytes-s.held {
		return nil, 0, http.StatusServiceUnavailable
	}
	u.reserved = need
	s.held += need
	return u, need, 0
}

// finish stores what the PUT read, or, with nil, releases the slot for a
// retry. It reports false when the slot expired while the PUT ran, in which
// case nothing is kept.
func (s *uploadStore) finish(u *upload, data []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held -= u.reserved
	u.reserved = 0
	now := s.now()
	if now.After(u.expires) {
		s.drop(u)
		return false
	}
	if data != nil {
		sum := sha256.Sum256(data)
		u.data, u.filled, u.sha256 = data, true, hex.EncodeToString(sum[:])
		u.expires = now.Add(uploadTTL)
		s.held += len(data)
	}
	return true
}

// chunk is one append_upload_chunk call. size is the whole file's, required
// on chunk 0; sha256 is the whole file's, required on the last. chunkSHA256,
// if given, is this chunk's own, as the sender computed it.
type chunk struct {
	id          string
	index       int
	data        []byte
	last        bool
	size        int
	sha256      string
	chunkSHA256 string
}

// chunkProgress is what an append answers: how many chunks are in, the hash
// of the one this call carried, and, once the file is complete, the receipt.
type chunkProgress struct {
	chunks, bytes int
	chunkSHA256   string
	expires       time.Time
	receipt       *uploadReceipt
	repeat        bool // the chunk was already in, from a retried call
}

// discard drops a chunked upload whose bytes cannot be right, and says so.
func (s *uploadStore) discard(u *upload, err error) error {
	s.drop(u)
	return fmt.Errorf("%w. The upload was discarded", err)
}

// appendChunk adds one chunk to a slot, for the subject that opened it.
//
// Chunks go in order from 0, one call at a time. Resending the newest chunk
// with the same bytes is a no-op, so a call whose answer was lost can be
// retried; resending it with last set closes the file, as does an empty
// chunk with last, for a sender that forgot last on its final chunk.
//
// Everything that can be decided early is: the size is reserved at chunk 0,
// so a file that will not fit is refused before the rest is written out; a
// chunk that does not match its chunk_sha256 is refused alone, so only it is
// sent again; and the name and type are checked as soon as the first 512
// bytes are in, which is all the check reads. What is left for the last
// chunk is the whole file's size and SHA-256. A failure there, or a type
// mismatch, discards the upload, since there is no telling which chunk was
// wrong.
func (s *uploadStore) appendChunk(sub string, c chunk) (chunkProgress, error) {
	if len(c.data) > maxChunkBytes {
		return chunkProgress{}, fmt.Errorf("%w: %s", errBadInput, errChunkTooBig)
	}
	if c.last && !sha256Re.MatchString(strings.TrimSpace(c.sha256)) {
		return chunkProgress{}, fmt.Errorf("%w: the last chunk needs the whole file's sha256 in hex, as sha256sum prints it", errBadInput)
	}
	if c.index == 0 && len(c.data) > 0 && c.size <= 0 {
		return chunkProgress{}, fmt.Errorf("%w: chunk 0 needs size, the whole file's size in bytes, so the server can reserve room for it before the rest is sent", errBadInput)
	}
	if c.index == 0 && c.size > maxAttachmentBytes {
		return chunkProgress{}, fmt.Errorf("%w: the file is over the %d-byte limit, so it cannot be attached", errBadInput, maxAttachmentBytes)
	}
	sum := sha256.Sum256(c.data)
	got := hex.EncodeToString(sum[:])
	if want := strings.TrimSpace(c.chunkSHA256); want != "" {
		if !sha256Re.MatchString(want) {
			return chunkProgress{}, fmt.Errorf("%w: chunk_sha256 must be hex, as sha256sum prints it", errBadInput)
		}
		if !strings.EqualFold(want, got) {
			return chunkProgress{}, fmt.Errorf("%w: chunk %d decodes to bytes whose sha256 is not chunk_sha256, so it was mistyped. Nothing was added: send chunk %d again", errBadInput, c.index, c.index)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	u, ok := s.byID[c.id]
	if !ok || sub == "" || u.sub != sub {
		if e, ok := s.evicted[c.id]; ok && sub != "" && e.sub == sub {
			return chunkProgress{}, fmt.Errorf("%w: %s", errBadInput, errUploadEvicted)
		}
		return chunkProgress{}, fmt.Errorf("%w: %s", errBadInput, errUploadUnknown)
	}
	newest := u.chunks > 0 && c.index == u.chunks-1 && sum == u.lastPart && len(c.data) > 0
	switch {
	case u.filled:
		// The last chunk again, from a call whose answer was lost.
		retried := newest || (len(c.data) == 0 && c.index == u.chunks && u.chunks > 0)
		if c.last && retried && strings.EqualFold(strings.TrimSpace(c.sha256), u.sha256) {
			r := receiptFor(u)
			return chunkProgress{chunks: u.chunks, bytes: len(u.data), chunkSHA256: got, expires: u.expires, receipt: &r, repeat: true}, nil
		}
		return chunkProgress{}, fmt.Errorf("%w: %s", errBadInput, errUploadFilled)
	case u.reserved > 0:
		return chunkProgress{}, fmt.Errorf("%w: %s", errBadInput, errUploadPutting)
	case newest && !c.last:
		return chunkProgress{chunks: u.chunks, bytes: u.partBytes, chunkSHA256: got, expires: u.expires, repeat: true}, nil
	case newest && c.last, len(c.data) == 0 && c.last && c.index == u.chunks && u.chunks > 0:
		// Closing a file whose final chunk went in without last.
		return s.close(u, c, got)
	case len(c.data) == 0:
		return chunkProgress{}, fmt.Errorf("%w: the chunk is empty. To finish an upload whose chunks are all in, send an empty chunk at the next index with last", errBadInput)
	case c.index < u.chunks:
		return chunkProgress{}, fmt.Errorf("%w: chunk %d is already in, with other bytes. Nothing was added. This upload has chunks 0 to %d; send index %d next, one call at a time", errBadInput, c.index, u.chunks-1, u.chunks)
	case c.index > u.chunks:
		return chunkProgress{}, fmt.Errorf("%w: chunks go in order, one call at a time, and this upload has %d, so the next is index %d. Nothing was added", errBadInput, u.chunks, u.chunks)
	case u.chunks >= maxChunks:
		return chunkProgress{}, s.discard(u, fmt.Errorf("%w: an upload takes at most %d chunks; open a new one and send bigger chunks", errBadInput, maxChunks))
	}

	if c.index == 0 {
		if c.size > maxHeldBytes-s.held {
			return chunkProgress{}, errUploadHeld
		}
		u.chunkSize = c.size
		s.held += c.size
		u.running = sha256.New()
	} else if c.size != 0 && c.size != u.chunkSize {
		return chunkProgress{}, fmt.Errorf("%w: size %d is not the %d chunk 0 gave. Nothing was added", errBadInput, c.size, u.chunkSize)
	}
	if u.partBytes+len(c.data) > u.chunkSize {
		return chunkProgress{}, s.discard(u, fmt.Errorf("%w: the chunks add up to more than the %d bytes chunk 0 gave as the file's size", errBadInput, u.chunkSize))
	}
	u.parts = append(u.parts, c.data)
	u.partBytes += len(c.data)
	u.chunks++
	u.lastPart = sum
	u.running.Write(c.data)
	u.expires = s.now().Add(unfilledTTL)
	if !u.sniffed && (u.partBytes >= 512 || u.partBytes == u.chunkSize) {
		if err := checkContent(u.filename, u.mimeType, u.head(512)); err != nil {
			return chunkProgress{}, s.discard(u, fmt.Errorf("%w; open an upload with the right name and type", err))
		}
		u.sniffed = true
	}
	if !c.last {
		return chunkProgress{chunks: u.chunks, bytes: u.partBytes, chunkSHA256: got, expires: u.expires}, nil
	}
	return s.close(u, c, got)
}

// head is the file's first n bytes so far, or all of it if shorter.
func (u *upload) head(n int) []byte {
	b := make([]byte, 0, min(n, u.partBytes))
	for _, p := range u.parts {
		if len(b) >= n {
			break
		}
		b = append(b, p[:min(len(p), n-len(b))]...)
	}
	return b
}

// close checks a chunked file is whole and the one the sender meant, then
// joins it into the upload's data. The running hash means only the copy
// runs under the lock. Callers hold mu.
func (s *uploadStore) close(u *upload, c chunk, chunkSum string) (chunkProgress, error) {
	whole := hex.EncodeToString(u.running.Sum(nil))
	switch {
	case u.partBytes != u.chunkSize:
		return chunkProgress{}, s.discard(u, fmt.Errorf("%w: the chunks add up to %d bytes, not the %d chunk 0 gave as the file's size", errBadInput, u.partBytes, u.chunkSize))
	case !strings.EqualFold(strings.TrimSpace(c.sha256), whole):
		return chunkProgress{}, s.discard(u, fmt.Errorf("%w: the chunks' sha256 is not the one the last chunk gives, so a chunk was wrong. Sending chunk_sha256 with each chunk catches a mistyped one when it arrives", errBadInput))
	}
	data := make([]byte, 0, u.partBytes)
	for _, p := range u.parts {
		data = append(data, p...)
	}
	s.held += len(data) - u.chunkSize
	u.parts, u.partBytes, u.chunkSize, u.running = nil, 0, 0, nil
	u.data, u.filled, u.sha256 = data, true, whole
	u.expires = s.now().Add(uploadTTL)
	r := receiptFor(u)
	return chunkProgress{chunks: u.chunks, bytes: len(data), chunkSHA256: chunkSum, expires: u.expires, receipt: &r}, nil
}

var sha256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func receiptFor(u *upload) uploadReceipt {
	return uploadReceipt{
		UploadID: u.id, Filename: u.filename, MimeType: u.mimeType, Size: len(u.data),
		SHA256: u.sha256, DetectedType: http.DetectContentType(u.data), ExpiresAt: u.expires.UTC().Format(time.RFC3339),
	}
}

type uploadReceipt struct {
	UploadID     string `json:"upload_id"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mime_type"`
	Size         int    `json:"size"`
	SHA256       string `json:"sha256"`
	DetectedType string `json:"detected_type"`
	ExpiresAt    string `json:"expires_at"`
}

// handler serves PUT /uploads/<secret>. Nothing about a refused request is
// written back beyond its status, and the secret is never logged.
func (s *uploadStore) handler(log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.URL.Path, uploadsPath)
		if !ok || !tokenRe.MatchString(secret) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", http.MethodPut)
			http.Error(w, "use PUT", http.StatusMethodNotAllowed)
			return
		}
		u, limit, status := s.begin(secret, r.ContentLength)
		if status != 0 {
			msg := map[int]string{
				http.StatusNotFound:              "no such upload, or it has expired",
				http.StatusConflict:              "this upload already has a file; if your earlier PUT got no answer, it may have landed: open a new upload to be sure",
				http.StatusBadRequest:            "the file is empty",
				http.StatusRequestEntityTooLarge: fmt.Sprintf("the file is over the %d-byte limit", maxAttachmentBytes),
				http.StatusServiceUnavailable:    "the server is holding too much upload data right now; try again in a minute",
			}[status]
			if status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", "60")
			}
			http.Error(w, msg, status)
			return
		}
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(uploadIOTimeout))
		_ = rc.SetWriteDeadline(time.Now().Add(uploadIOTimeout + 10*time.Second))
		data, err := readBody(http.MaxBytesReader(w, r.Body, int64(limit)), r.ContentLength)
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			s.finish(u, nil)
			http.Error(w, fmt.Sprintf("the file is over the %d-byte limit", maxAttachmentBytes), http.StatusRequestEntityTooLarge)
			return
		case err != nil:
			s.finish(u, nil)
			http.Error(w, "the upload did not complete; PUT it again", http.StatusBadRequest)
			return
		case len(data) == 0:
			s.finish(u, nil)
			http.Error(w, "the file is empty", http.StatusBadRequest)
			return
		}
		if err := checkContent(u.filename, u.mimeType, data); err != nil {
			s.finish(u, nil)
			http.Error(w, strings.TrimPrefix(err.Error(), errBadInput.Error()+": ")+"; open an upload with the right name and type", http.StatusUnsupportedMediaType)
			return
		}
		if !s.finish(u, data) {
			http.Error(w, "the upload expired while it was being sent; open a new one", http.StatusGone)
			return
		}
		log.Info("upload received", "sub", u.sub, "upload", u.id, "bytes", len(data))
		w.Header().Set("Content-Type", "application/json")
		s.mu.Lock()
		receipt := receiptFor(u)
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(receipt)
	})
}

// readBody reads a PUT body into one buffer of the declared size, so a 25 MB
// file costs 25 MB and not the doublings io.ReadAll would go through. net/http
// stops the body at Content-Length, so a short one is an error and a long one
// cannot happen. A chunked body has no size to go by and is read with
// io.ReadAll; curl -T sends Content-Length.
func readBody(body io.Reader, declared int64) ([]byte, error) {
	if declared <= 0 {
		return io.ReadAll(body)
	}
	data := make([]byte, declared)
	if _, err := io.ReadFull(body, data); err != nil {
		return nil, err
	}
	return data, nil
}
