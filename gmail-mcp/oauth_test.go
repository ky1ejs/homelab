package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testResource = "https://gmail-mcp.example.ts.net/mcp"

func testVerifier(as *fakeAS) *oauthVerifier {
	return newOAuthVerifier(as.URL, testResource, []string{"user_kyle"}, http.DefaultClient, quietLogger())
}

func TestVerify(t *testing.T) {
	as := newFakeAS(t)
	o := testVerifier(as)
	ctx := context.Background()
	hour := time.Now().Add(time.Hour)

	if sub, err := o.verify(ctx, as.mint(t, "user_kyle", testResource, hour)); err != nil || sub != "user_kyle" {
		t.Fatalf("good token: %q, %v", sub, err)
	}
	cases := map[string]struct {
		token string
		want  error
	}{
		// The shared-tenant case: a token minted for the vault must not open mail.
		"vault audience": {as.mint(t, "user_kyle", "https://vault-mcp.example.ts.net/mcp", hour), errNoAudience},
		"other subject":  {as.mint(t, "user_someone", testResource, hour), errSubjectNotAllowed},
		"expired":        {as.mint(t, "user_kyle", testResource, time.Now().Add(-time.Minute)), nil},
		"not a jwt":      {"abc.def.ghi", nil},
		"another issuer": {newFakeAS(t).mint(t, "user_kyle", testResource, hour), nil},
	}
	for name, c := range cases {
		_, err := o.verify(ctx, c.token)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func testServer(t *testing.T, as *fakeAS, g *fakeGmail) *httptest.Server {
	t.Helper()
	s := &server{oauth: testVerifier(as), tools: &tools{gmail: g.client(), log: quietLogger()}, log: quietLogger()}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestUnauthenticatedGetsChallenge(t *testing.T) {
	as := newFakeAS(t)
	srv := testServer(t, as, newFakeGmail(t))
	for _, auth := range []string{"", "Bearer nonsense", "Basic dXNlcjpwYXNz"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d", auth, resp.StatusCode)
		}
		want := `Bearer resource_metadata="` + testResource + resourceMetadataPath + `"`
		if got := resp.Header.Get("WWW-Authenticate"); got != want {
			t.Errorf("auth %q: challenge %q, want %q", auth, got, want)
		}
	}
}

func TestResourceMetadata(t *testing.T) {
	as := newFakeAS(t)
	srv := testServer(t, as, newFakeGmail(t))
	for _, path := range []string{resourceMetadataPath, resourceMetadataPath + "/mcp"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
		if m.Resource != testResource || len(m.Servers) != 1 || m.Servers[0] != as.URL {
			t.Errorf("%s: %+v", path, m)
		}
	}
}
