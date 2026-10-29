package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- a stand-in authorization server, the same shape as vault-mcp's --------

type fakeAS struct {
	*httptest.Server
	key *rsa.PrivateKey
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	as := &fakeAS{key: key}
	mux := http.NewServeMux()
	as.Server = httptest.NewServer(mux)
	t.Cleanup(as.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                as.URL,
			"authorization_endpoint":                as.URL + "/authorize",
			"token_endpoint":                        as.URL + "/token",
			"jwks_uri":                              as.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig",
		}}})
	})
	return as
}

func (as *fakeAS) mint(t *testing.T, sub, aud string, expires time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: as.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"iss": as.URL, "sub": sub, "aud": []string{aud},
		"exp": expires.Unix(), "iat": time.Now().Add(-time.Minute).Unix(),
	})
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// --- a stand-in Gmail API ----------------------------------------------------

type staticTokens struct{}

func (staticTokens) Get(context.Context) (string, error) { return "g-access", nil }

type recorded struct {
	Method, Path string
	Body         []byte
}

type fakeGmail struct {
	*httptest.Server
	mu    sync.Mutex
	calls []recorded
}

func b64(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

var testSendAs = []sendAs{
	{SendAsEmail: "me@example.com", DisplayName: "Me", IsPrimary: true, IsDefault: true},
	{SendAsEmail: "alias@example.org", DisplayName: "Me Alias", VerificationStatus: "accepted"},
	{SendAsEmail: "pending@example.net", VerificationStatus: "pending"},
}

func testThread() gmailThread {
	plain := gmailPart{MimeType: "text/plain", Headers: []gmailHeader{{"Content-Type", `text/plain; charset="UTF-8"`}}}
	plain.Body.Data = b64("Are you coming? \U0001F983")
	htmlPart := gmailPart{MimeType: "text/html"}
	htmlPart.Body.Data = b64("<div>See attached&nbsp;menu<br>Thanks &amp; bye</div><script>x()</script>")
	pdf := gmailPart{MimeType: "application/pdf", Filename: "menu.pdf"}
	pdf.Body.Size, pdf.Body.AttachmentID = 1234, "a1"
	return gmailThread{ID: "t1", Messages: []gmailMessage{
		{ID: "m1", ThreadID: "t1", LabelIDs: []string{"INBOX"}, Snippet: "Are you coming?", Payload: gmailPart{
			MimeType: "multipart/alternative",
			Headers: []gmailHeader{
				{"From", "Sam <sam@example.com>"}, {"To", "me@example.com"},
				{"Subject", "Thanksgiving"}, {"Date", "Wed, 28 Oct 2026 10:00:00 -0600"},
				{"Message-ID", "<orig-1@example.com>"},
			},
			Parts: []gmailPart{plain, {MimeType: "text/html"}},
		}},
		{ID: "m2", ThreadID: "t1", LabelIDs: []string{"INBOX", "UNREAD"}, Snippet: "See attached menu", Payload: gmailPart{
			MimeType: "multipart/mixed",
			Headers: []gmailHeader{
				{"From", "Alex <alex@example.com>"}, {"Subject", "Re: Thanksgiving"},
				{"Date", "Wed, 28 Oct 2026 12:00:00 -0600"},
				{"Message-ID", "<orig-2@example.com>"}, {"References", "<orig-1@example.com>"},
			},
			Parts: []gmailPart{htmlPart, pdf},
		}},
	}}
}

func newFakeGmail(t *testing.T) *fakeGmail {
	t.Helper()
	f := &fakeGmail{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, recorded{r.Method, r.URL.Path, body})
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer g-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		th := testThread()
		switch {
		case r.URL.Path == "/settings/sendAs":
			write(map[string]any{"sendAs": testSendAs})
		case r.URL.Path == "/threads" && r.Method == http.MethodGet:
			write(map[string]any{"threads": []map[string]string{{"id": "t1"}}, "nextPageToken": "next"})
		case r.URL.Path == "/threads/t1":
			write(th)
		case r.URL.Path == "/messages/m2":
			m := th.Messages[1]
			m.Payload.Parts = nil
			write(m)
		case r.URL.Path == "/drafts" && r.Method == http.MethodPost:
			var in struct {
				Message rawMessage `json:"message"`
			}
			_ = json.Unmarshal(body, &in)
			write(map[string]any{"id": "d1", "message": map[string]string{"id": "m9", "threadId": firstNonEmpty(in.Message.ThreadID, "t9")}})
		case r.URL.Path == "/messages/send" && r.Method == http.MethodPost:
			var in rawMessage
			_ = json.Unmarshal(body, &in)
			write(map[string]any{"id": "m10", "threadId": firstNonEmpty(in.ThreadID, "t10")})
		default:
			w.WriteHeader(http.StatusNotFound)
			write(map[string]any{"error": map[string]string{"message": "Not Found"}})
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGmail) client() *gmailClient {
	return &gmailClient{base: f.URL, http: f.Client(), tokens: staticTokens{}}
}

// sent returns the decoded raw message of the first call to path, or "".
func (f *fakeGmail) sent(t *testing.T, path string) (raw, threadID string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Path != path {
			continue
		}
		var m rawMessage
		if path == "/drafts" {
			var d struct {
				Message rawMessage `json:"message"`
			}
			_ = json.Unmarshal(c.Body, &d)
			m = d.Message
		} else {
			_ = json.Unmarshal(c.Body, &m)
		}
		dec, err := base64.URLEncoding.DecodeString(m.Raw)
		if err != nil {
			t.Fatal(err)
		}
		return string(dec), m.ThreadID
	}
	return "", ""
}

func (f *fakeGmail) called(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Path == path {
			return true
		}
	}
	return false
}

// decodeBody pulls the base64 body back out of a built message.
func decodeBody(t *testing.T, raw string) (headers, body string) {
	t.Helper()
	h, b, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatalf("no header/body separator in %q", raw)
	}
	dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(b, "\r\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	return h, string(dec)
}
