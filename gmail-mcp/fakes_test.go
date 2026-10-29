package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- a stand-in authorization server, the same shape as vault-mcp's --------

// TLS, and reached through the production egress client (see testVerifier),
// so the tests exercise the same transport rules as the real server.
type fakeAS struct {
	*httptest.Server
	key  *rsa.PrivateKey
	hits atomic.Int32 // requests of any kind
	// jwksURI, when set, replaces the advertised key URL.
	jwksURI string
	// down makes discovery fail.
	down atomic.Bool
}

const testClient = "client_claude"

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	as := &fakeAS{key: key}
	mux := http.NewServeMux()
	as.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.hits.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(as.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if as.down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                as.URL,
			"authorization_endpoint":                as.URL + "/authorize",
			"token_endpoint":                        as.URL + "/token",
			"jwks_uri":                              firstNonEmpty(as.jwksURI, as.URL+"/jwks"),
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

// mint signs a token as claude.ai's would look: the test client, an array
// audience.
func (as *fakeAS) mint(t *testing.T, sub, aud string, expires time.Time) string {
	t.Helper()
	return as.mintClaims(t, map[string]any{"sub": sub, "aud": []string{aud}, "exp": expires.Unix()})
}

// mintClaims signs the defaults overlaid with claims; a nil value deletes one.
func (as *fakeAS) mintClaims(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: as.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]any{
		"iss": as.URL, "sub": "user_kyle", "aud": []string{testResource}, "client_id": testClient,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(all, k)
		} else {
			all[k] = v
		}
	}
	payload, _ := json.Marshal(all)
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

// rotatingTokens hands out "stale" until invalidated, then "g-access".
type rotatingTokens struct{ invalidated atomic.Int32 }

func (r *rotatingTokens) Get(context.Context) (string, error) {
	if r.invalidated.Load() == 0 {
		return "stale", nil
	}
	return "g-access", nil
}
func (r *rotatingTokens) Invalidate() { r.invalidated.Add(1) }

type recorded struct {
	Method, Path string
	Body         []byte
}

type fakeGmail struct {
	*httptest.Server
	mu    sync.Mutex
	calls []recorded

	// Knobs, set before the first request.
	sendAs       []sendAs // default testSendAs
	searchIDs    []string // default t1
	sentFrom     string   // the From Gmail puts on the sent copy; default: as built
	sendStatus   int      // non-zero: /messages/send answers with this status
	sendAsStatus int      // non-zero: /settings/sendAs answers with this status
	lastFrom     string
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
	f := &fakeGmail{sendAs: testSendAs, searchIDs: []string{"t1"}}
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
		status := func(code int) {
			w.WriteHeader(code)
			write(map[string]any{"error": map[string]string{"message": http.StatusText(code)}})
		}
		th := testThread()
		switch {
		case r.URL.Path == "/settings/sendAs" && f.sendAsStatus != 0:
			status(f.sendAsStatus)
		case r.URL.Path == "/settings/sendAs":
			write(map[string]any{"sendAs": f.sendAs})
		case r.URL.Path == "/threads" && r.Method == http.MethodGet:
			ids := []map[string]string{}
			for _, id := range f.searchIDs {
				ids = append(ids, map[string]string{"id": id})
			}
			write(map[string]any{"threads": ids, "nextPageToken": "next"})
		case r.URL.Path == "/threads/gone":
			status(http.StatusNotFound)
		case r.URL.Path == "/threads/busy":
			status(http.StatusTooManyRequests)
		case r.URL.Path == "/threads/long":
			write(longThread())
		case strings.HasPrefix(r.URL.Path, "/threads/t"):
			th.ID = strings.TrimPrefix(r.URL.Path, "/threads/")
			write(th)
		case r.URL.Path == "/messages/m10":
			f.mu.Lock()
			from := firstNonEmpty(f.sentFrom, f.lastFrom, "Me <me@example.com>")
			f.mu.Unlock()
			write(gmailMessage{ID: "m10", Payload: gmailPart{Headers: []gmailHeader{{"From", from}}}})
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
			if f.sendStatus != 0 {
				status(f.sendStatus)
				return
			}
			var in rawMessage
			_ = json.Unmarshal(body, &in)
			if dec, err := base64.URLEncoding.DecodeString(in.Raw); err == nil {
				if m, err := mail.ReadMessage(strings.NewReader(string(dec))); err == nil {
					f.mu.Lock()
					f.lastFrom = m.Header.Get("From")
					f.mu.Unlock()
				}
			}
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

// longThread is eight messages of 19,000 characters each, over the thread
// budget, oldest first.
func longThread() gmailThread {
	th := gmailThread{ID: "long"}
	for i := range 8 {
		p := gmailPart{MimeType: "text/plain"}
		p.Body.Data = b64(strings.Repeat(string(rune('a'+i)), 19000))
		th.Messages = append(th.Messages, gmailMessage{ID: fmt.Sprintf("L%d", i), Snippet: fmt.Sprintf("snippet %d", i), Payload: p})
	}
	return th
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
