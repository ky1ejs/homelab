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
// Uploads live in memory only and go when they expire or the server restarts.
// The caps below keep the total inside the container's 256 MB.

const (
	// How long a filled upload can be attached, from when its PUT finished.
	uploadTTL = 30 * time.Minute
	// How long an empty slot waits for its PUT.
	unfilledTTL    = 10 * time.Minute
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
	errUploadEmpty    = errors.New("that upload has no file yet: PUT the file to its upload_url first")
	errUploadMismatch = errors.New("sha256 does not match the file in that upload, so it is not the file you meant. Nothing was sent or drafted")
	errUploadSending  = errors.New("that upload is already being sent by another call. This call sent nothing; check Sent before trying again")
	errUploadNotNamed = errors.New("that upload holds a different file: give the filename (and mime_type, if you give one) exactly as create_upload returned them")
	errUploadsFull    = errors.New("too many uploads are open. Use one already open, or wait up to 10 minutes for unused ones to expire")
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
}

type uploadStore struct {
	mu       sync.Mutex
	now      func() time.Time
	byID     map[string]*upload
	bySecret map[string]*upload
	held     int
}

func newUploadStore() *uploadStore {
	return &uploadStore{now: time.Now, byID: map[string]*upload{}, bySecret: map[string]*upload{}}
}

// token is 32 random bytes, base64url without padding: 43 characters.
func token() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b)
}

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func (s *uploadStore) drop(u *upload) {
	s.held -= len(u.data) + u.reserved
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
// slot is taken, the oldest one still waiting for its PUT gives way: a filled
// upload is about to be attached, an empty one was most likely abandoned.
func (s *uploadStore) open(sub, filename, mimeType string) (*upload, error) {
	if sub == "" {
		return nil, fmt.Errorf("%w: no signed-in subject", errBadInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	if len(s.byID) >= maxOpenUploads {
		var oldest *upload
		for _, u := range s.byID {
			if !u.filled && u.reserved == 0 && (oldest == nil || u.opened.Before(oldest.opened)) {
				oldest = u
			}
		}
		if oldest == nil {
			return nil, errUploadsFull
		}
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
	case u.filled || u.reserved > 0:
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
		_ = json.NewEncoder(w).Encode(uploadReceipt{
			UploadID: u.id, Filename: u.filename, MimeType: u.mimeType, Size: len(data),
			SHA256: u.sha256, DetectedType: http.DetectContentType(data), ExpiresAt: u.expires.UTC().Format(time.RFC3339),
		})
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
