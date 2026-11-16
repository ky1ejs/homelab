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
// same subject that opened the slot.
//
// Uploads live in memory only and go when they expire or the server restarts.
// The caps below keep the total inside the container's 256 MB.

const (
	uploadTTL      = 30 * time.Minute
	maxOpenUploads = 10       // slots, filled or not, across all subjects
	maxHeldBytes   = 40 << 20 // filled and in-flight bytes, across all slots
	uploadsPath    = "/uploads/"
	// A 25 MB PUT over a slow uplink needs longer than the server's 30s
	// ReadTimeout, which is right for every other request.
	uploadReadTimeout = 5 * time.Minute
)

var (
	errUploadUnknown = errors.New("no such upload: it has expired, or the id is wrong. Call create_upload again")
	errUploadEmpty   = errors.New("that upload has no file yet: PUT the file to its upload_url first")
	errUploadsFull   = errors.New("too many uploads are open. Wait for older ones to expire, or use one already open")
)

type upload struct {
	id, secret string
	sub        string // the OAuth subject that opened it
	filename   string
	mimeType   string
	expires    time.Time
	data       []byte
	filled     bool
	reserved   int // bytes held for a PUT in progress
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

// sweep drops expired uploads. Callers hold mu.
func (s *uploadStore) sweep() {
	now := s.now()
	for id, u := range s.byID {
		if now.After(u.expires) {
			s.held -= len(u.data) + u.reserved
			delete(s.byID, id)
			delete(s.bySecret, u.secret)
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

// open reserves a slot. filename and mimeType are already checked.
func (s *uploadStore) open(sub, filename, mimeType string) (*upload, error) {
	if sub == "" {
		return nil, fmt.Errorf("%w: no signed-in subject", errBadInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	if len(s.byID) >= maxOpenUploads {
		return nil, errUploadsFull
	}
	u := &upload{id: "up_" + token(), secret: token(), sub: sub, filename: filename, mimeType: mimeType, expires: s.now().Add(uploadTTL)}
	s.byID[u.id] = u
	s.bySecret[u.secret] = u
	cp := *u
	return &cp, nil
}

// get returns a filled upload's attachment, for the subject that opened it.
// Another subject's upload is reported as unknown, not as forbidden.
func (s *uploadStore) get(sub, id string) (outgoingAttachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	u, ok := s.byID[id]
	if !ok || sub == "" || u.sub != sub {
		return outgoingAttachment{}, fmt.Errorf("%w: %s", errBadInput, errUploadUnknown)
	}
	if !u.filled {
		return outgoingAttachment{}, fmt.Errorf("%w: %s", errBadInput, errUploadEmpty)
	}
	// data is never written again once filled, so sharing it is safe.
	return outgoingAttachment{Filename: u.filename, MimeType: u.mimeType, Data: u.data}, nil
}

// begin claims an empty slot for a PUT and reserves room for it, returning how
// many bytes the PUT may write.
func (s *uploadStore) begin(secret string) (*upload, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	u, ok := s.bySecret[secret]
	switch {
	case !ok:
		return nil, 0, http.StatusNotFound
	case u.filled || u.reserved > 0:
		return nil, 0, http.StatusConflict
	}
	room := min(maxAttachmentBytes, maxHeldBytes-s.held)
	if room <= 0 {
		return nil, 0, http.StatusServiceUnavailable
	}
	u.reserved = room
	s.held += room
	return u, room, 0
}

// finish stores what the PUT read, or releases the slot for a retry.
func (s *uploadStore) finish(u *upload, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID[u.id] != u {
		return // swept while the PUT ran; its reservation went with it
	}
	s.held -= u.reserved
	u.reserved = 0
	if data != nil {
		u.data, u.filled = data, true
		s.held += len(data)
	}
}

type uploadReceipt struct {
	UploadID     string `json:"upload_id"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mime_type"`
	Size         int    `json:"size"`
	SHA256       string `json:"sha256"`
	DetectedType string `json:"detected_type"`
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
		u, room, status := s.begin(secret)
		if status != 0 {
			msg := map[int]string{
				http.StatusNotFound:           "no such upload, or it has expired",
				http.StatusConflict:           "this upload already has a file; open a new one with create_upload",
				http.StatusServiceUnavailable: "the server is holding too many uploads; try again later",
			}[status]
			http.Error(w, msg, status)
			return
		}
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(uploadReadTimeout))
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(room)))
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			s.finish(u, nil)
			http.Error(w, fmt.Sprintf("the file is over the %d-byte limit", room), http.StatusRequestEntityTooLarge)
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
		s.finish(u, data)
		sum := sha256.Sum256(data)
		log.Info("upload received", "sub", u.sub, "upload", u.id, "bytes", len(data))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(uploadReceipt{
			UploadID: u.id, Filename: u.filename, MimeType: u.mimeType, Size: len(data),
			SHA256: hex.EncodeToString(sum[:]), DetectedType: http.DetectContentType(data),
		})
	})
}
