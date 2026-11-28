package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var testNow = time.Date(2026, 11, 28, 15, 0, 0, 0, time.UTC)

// fixture is a snapshot in the collector's shape: fast five minutes old, the
// disk scan two hours old, the usage summary half an hour old.
func fixture(over map[string]any) map[string]any {
	ts := func(ago time.Duration) string { return testNow.Add(-ago).Format(time.RFC3339) }
	s := map[string]any{
		"schema": 1,
		"fast": map[string]any{
			"collected": ts(5 * time.Minute),
			"host":      map[string]any{"load": []any{1.5, 2.0, 2.5}, "mem": map[string]any{"used_gb": 40.1}, "uptime_days": 3.2},
			"versions":  map[string]any{"macos": "27.2.1", "xcode": "27.1"},
			"ci":        []any{map[string]any{"slot": 1, "phase": "idle"}},
			"colima":    map[string]any{"status": "Running"},
			"docker":    map[string]any{"cut_postgres": map[string]any{"worktree": map[string]any{"count": 3, "gb": 1.2}}},
		},
		"disk": map[string]any{
			"collected":    ts(2 * time.Hour),
			"apfs":         map[string]any{"free_gb": 480.0},
			"top":          []any{map[string]any{"key": "tart-vms", "gb": 120.0}},
			"growth_since": ts(8 * time.Hour),
			"categories": []any{
				map[string]any{"key": "tart-vms", "gb": 120.0},
				map[string]any{"key": "tart-vm-cut-ci", "parent": "tart-vms", "gb": 60.0},
				map[string]any{"key": "derived-data", "gb": 18.0},
			},
		},
		"usage": map[string]any{
			"collected": ts(30 * time.Minute),
			"summary":   map[string]any{"memory": "ok", "disk": "watch", "disk_why": "grew 20 GB a day"},
			"days":      []any{},
		},
	}
	for k, v := range over {
		if v == nil {
			delete(s, k)
		} else {
			s[k] = v
		}
	}
	return s
}

func writeSnapshot(t *testing.T, path string, s map[string]any) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}
}

type testEnvDirs struct {
	snapshot, queue string
}

func testTools(t *testing.T, s map[string]any) (*tools, testEnvDirs) {
	t.Helper()
	dir := t.TempDir()
	d := testEnvDirs{snapshot: filepath.Join(dir, "snapshot.json"), queue: filepath.Join(dir, "queue")}
	if err := os.Mkdir(d.queue, 0o700); err != nil {
		t.Fatal(err)
	}
	if s != nil {
		writeSnapshot(t, d.snapshot, s)
	}
	return &tools{snap: &snapshotFile{path: d.snapshot, now: func() time.Time { return testNow }}, queue: d.queue, log: quietLogger()}, d
}

func testServer(t *testing.T, as *fakeAS, tl *tools) *httptest.Server {
	t.Helper()
	s := &server{oauth: testVerifier(as), tools: tl, log: quietLogger()}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func text(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func connect(t *testing.T, srv *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}},
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if !res.IsError {
		if err := json.Unmarshal([]byte(text(res)), &out); err != nil {
			t.Fatalf("%s: not JSON: %v\n%s", name, err, text(res))
		}
	}
	return res, out
}

func TestUnauthorized(t *testing.T) {
	as := newFakeAS(t)
	tl, _ := testTools(t, fixture(nil))
	srv := testServer(t, as, tl)
	for name, tok := range map[string]string{
		"none":           "",
		"garbage":        "not-a-jwt",
		"gmail audience": as.mint(t, "user_kyle", "https://gmail-mcp.example.ts.net/mcp", time.Now().Add(time.Hour)),
		"other user":     as.mint(t, "user_mallory", testResource, time.Now().Add(time.Hour)),
	} {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
		if c := resp.Header.Get("WWW-Authenticate"); !strings.Contains(c, resourceMetadataPath) {
			t.Errorf("%s: challenge %q has no metadata URL", name, c)
		}
	}
}

func TestResourceMetadata(t *testing.T) {
	as := newFakeAS(t)
	tl, _ := testTools(t, fixture(nil))
	srv := testServer(t, as, tl)
	for _, p := range []string{resourceMetadataPath, resourceMetadataPath + "/mcp"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
		if m["resource"] != testResource {
			t.Errorf("%s: resource = %v", p, m["resource"])
		}
	}
}

func TestEndToEnd(t *testing.T) {
	as := newFakeAS(t)
	tl, dirs := testTools(t, fixture(nil))
	srv := testServer(t, as, tl)
	cs := connect(t, srv, as.mint(t, "user_kyle", testResource, time.Now().Add(time.Hour)))

	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		ro := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		if ro == (tool.Name == "request_refresh") {
			t.Errorf("%s: read-only hint %v", tool.Name, ro)
		}
	}
	slices.Sort(names)
	want := []string{"get_disk", "get_host", "get_overview", "get_services", "get_usage_trend", "request_refresh"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	_, out := call(t, cs, "get_overview", nil)
	for _, k := range []string{"host", "versions", "ci", "colima", "cut_postgres", "disk", "usage_verdicts", "as_of"} {
		if _, ok := out[k]; !ok {
			t.Errorf("get_overview has no %s", k)
		}
	}
	if _, ok := out["warnings"]; ok {
		t.Errorf("fresh snapshot warned: %v", out["warnings"])
	}
	asOf := out["as_of"].(map[string]any)
	if age := asOf["fast"].(map[string]any)["age_minutes"]; age != 5.0 {
		t.Errorf("fast age = %v, want 5", age)
	}

	_, out = call(t, cs, "get_disk", map[string]any{"category": "tart-vms"})
	if rows := out["categories"].([]any); len(rows) != 2 {
		t.Errorf("tart-vms rows = %v, want the category and its child", rows)
	}
	res, _ := call(t, cs, "get_disk", map[string]any{"category": "nope"})
	if !res.IsError || !strings.Contains(text(res), "derived-data") {
		t.Errorf("unknown category: %v %q", res.IsError, text(res))
	}
	res, _ = call(t, cs, "get_disk", map[string]any{"category": "../etc"})
	if !res.IsError {
		t.Error("a path was accepted as a category")
	}
	_, out = call(t, cs, "get_disk", nil)
	if rows := out["categories"].([]any); len(rows) != 3 {
		t.Errorf("all rows = %d, want 3", len(rows))
	}

	_, out = call(t, cs, "get_usage_trend", nil)
	if out["summary"].(map[string]any)["disk"] != "watch" {
		t.Errorf("usage = %v", out)
	}

	// fast is five minutes old, older than its 20-second gap: it is asked.
	_, out = call(t, cs, "request_refresh", map[string]any{"scope": "fast"})
	if out["queued"] != true {
		t.Fatalf("refresh = %v", out)
	}
	if fi, err := os.Stat(filepath.Join(dirs.queue, "fast")); err != nil || fi.Size() != 0 {
		t.Fatalf("queue file: %v", err)
	}
	res, _ = call(t, cs, "request_refresh", map[string]any{"scope": "everything"})
	if !res.IsError {
		t.Error("unknown scope accepted")
	}
}

func TestRequestRefreshLimits(t *testing.T) {
	ts := testNow.Add(-10 * time.Second).Format(time.RFC3339)
	tl, dirs := testTools(t, fixture(map[string]any{"fast": map[string]any{"collected": ts}}))
	out, err := tl.requestRefresh("fast")
	if err != nil || out["queued"] != false {
		t.Fatalf("fresh data: %v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dirs.queue, "fast")); err == nil {
		t.Fatal("fresh data still queued a run")
	}

	// The disk scan is two hours old: one request queues, the next within
	// the gap does not, even once the collector has taken the first.
	out, _ = tl.requestRefresh("disk")
	if out["queued"] != true {
		t.Fatalf("disk: %v", out)
	}
	_ = os.Remove(filepath.Join(dirs.queue, "disk"))
	out, _ = tl.requestRefresh("disk")
	if out["queued"] != false {
		t.Fatalf("second disk request: %v", out)
	}
	if _, err := os.Stat(filepath.Join(dirs.queue, "disk")); err == nil {
		t.Fatal("second disk request queued a run")
	}

	running := fixture(map[string]any{"disk_scan": map[string]any{"running_since": testNow.Add(-time.Minute).Format(time.RFC3339)}})
	tl, _ = testTools(t, running)
	if out, _ := tl.requestRefresh("disk"); out["queued"] != false || !strings.Contains(out["why"].(string), "already running") {
		t.Fatalf("scan running: %v", out)
	}
}

func TestStaleAndMissing(t *testing.T) {
	old := testNow.Add(-2 * time.Hour).Format(time.RFC3339)
	tl, _ := testTools(t, fixture(map[string]any{"fast": map[string]any{"collected": old}, "usage": nil}))
	res, err := tl.read(tl.overview)
	if err != nil || res.IsError {
		t.Fatal(err, text(res))
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(text(res)), &out)
	warn, _ := json.Marshal(out["warnings"])
	if !strings.Contains(string(warn), "host, CI and service figures is 2h0m0s old") || !strings.Contains(string(warn), "no usage trend yet") {
		t.Errorf("warnings = %s", warn)
	}

	tl, _ = testTools(t, nil)
	res, _ = tl.read(tl.overview)
	if !res.IsError || !strings.Contains(text(res), "no snapshot yet") {
		t.Errorf("missing snapshot: %q", text(res))
	}

	tl, dirs := testTools(t, nil)
	writeSnapshot(t, dirs.snapshot, map[string]any{"schema": 2})
	res, _ = tl.read(tl.overview)
	if !res.IsError || !strings.Contains(text(res), "schema") {
		t.Errorf("wrong schema: %q", text(res))
	}
}
