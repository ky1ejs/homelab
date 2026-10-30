package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeSetupHub is just enough of Beszel 0.21.0's API for runSetup, including the
// behaviours that bit real runs: create-user only on an empty hub, the
// user_settings create hook that overwrites "emails" with the owner's own
// address, and a host change that only reconnects on status "pending".
type fakeSetupHub struct {
	mu       sync.Mutex
	users    map[string]*fakeUser // by id
	systems  map[string]map[string]any
	alerts   map[string]alertSpec // by name; one system
	settings map[string]any       // user_settings.settings, one owner
	settID   string
	smtp     map[string]any
	key      string
	writes   []string // method + path of every write, for "changed nothing" checks
	nextID   int
}

type fakeUser struct {
	ID, Email, Password, Role string
}

func newFakeSetupHub() *fakeSetupHub {
	return &fakeSetupHub{
		users:   map[string]*fakeUser{},
		systems: map[string]map[string]any{},
		alerts:  map[string]alertSpec{},
		smtp:    map[string]any{"enabled": false},
		key:     "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGhTXCGQphIff33c7VX4fss+MBT/qMjWdiXTX7CqCi5w",
	}
}

func (h *fakeSetupHub) id() string { h.nextID++; return fmt.Sprintf("id%03d", h.nextID) }

var filterValue = regexp.MustCompile(`= "([^"]*)"`)

func (h *fakeSetupHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	str := func(k string) string { s, _ := body[k].(string); return s }
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	fail := func(code int, v any) { w.WriteHeader(code); reply(v) }
	auth := r.Header.Get("Authorization")
	path := r.URL.Path
	isAuth := strings.HasSuffix(path, "/auth-with-password")
	if r.Method != http.MethodGet && !isAuth {
		h.writes = append(h.writes, r.Method+" "+path)
	}
	filter := ""
	if m := filterValue.FindStringSubmatch(r.URL.Query().Get("filter")); m != nil {
		filter = m[1]
	}

	switch {
	case path == "/api/health":
		reply(map[string]any{})
	case path == "/api/beszel/first-run":
		reply(map[string]bool{"firstRun": len(h.users) == 0})
	case path == "/api/beszel/create-user":
		if len(h.users) > 0 {
			fail(403, map[string]string{"err": "bootstrap unavailable"})
			return
		}
		u := &fakeUser{h.id(), str("email"), str("password"), "admin"}
		h.users[u.ID] = u
		reply(map[string]string{"msg": "User created"})
	case path == "/api/collections/users/auth-with-password", path == "/api/collections/_superusers/auth-with-password":
		for _, u := range h.users {
			if u.Email == str("identity") && u.Password == str("password") {
				if strings.Contains(path, "_superusers") {
					if u.Role != "admin" {
						break
					}
					reply(map[string]any{"token": "su", "record": map[string]string{"id": "su"}})
					return
				}
				reply(map[string]any{"token": "u:" + u.ID, "record": map[string]string{"id": u.ID, "role": u.Role}})
				return
			}
		}
		fail(400, map[string]string{"message": "Failed to authenticate."})
	case path == "/api/beszel/info":
		reply(map[string]string{"key": h.key})
	case path == "/api/collections/systems/records" && r.Method == http.MethodGet:
		var items []map[string]any
		for _, s := range h.systems {
			if s["name"] == filter {
				items = append(items, s)
			}
		}
		reply(map[string]any{"items": items})
	case path == "/api/collections/systems/records" && r.Method == http.MethodPost:
		id := h.id()
		body["id"] = id
		h.systems[id] = body
		reply(body)
	case strings.HasPrefix(path, "/api/collections/systems/records/") && r.Method == http.MethodPatch:
		s := h.systems[strings.TrimPrefix(path, "/api/collections/systems/records/")]
		for k, v := range body {
			s[k] = v
		}
		reply(s)
	case path == "/api/collections/users/records" && r.Method == http.MethodGet:
		if auth != "su" {
			fail(403, map[string]string{"message": "superuser only"})
			return
		}
		var items []map[string]string
		for _, u := range h.users {
			if u.Email == filter {
				items = append(items, map[string]string{"id": u.ID, "role": u.Role})
			}
		}
		reply(map[string]any{"items": items})
	case path == "/api/collections/users/records" && r.Method == http.MethodPost:
		u := &fakeUser{h.id(), str("email"), str("password"), str("role")}
		h.users[u.ID] = u
		reply(map[string]string{"id": u.ID})
	case strings.HasPrefix(path, "/api/collections/users/records/") && r.Method == http.MethodPatch:
		u := h.users[strings.TrimPrefix(path, "/api/collections/users/records/")]
		u.Password, u.Role = str("password"), str("role")
		reply(map[string]string{"id": u.ID})
	case path == "/api/settings" && r.Method == http.MethodGet:
		reply(map[string]any{"smtp": h.smtp})
	case path == "/api/settings" && r.Method == http.MethodPatch:
		h.smtp, _ = body["smtp"].(map[string]any)
		reply(map[string]any{"smtp": h.smtp})
	case path == "/api/collections/user_settings/records" && r.Method == http.MethodGet:
		if h.settID == "" {
			reply(map[string]any{"items": []any{}})
			return
		}
		reply(map[string]any{"items": []any{map[string]any{"id": h.settID, "settings": h.settings}}})
	case path == "/api/collections/user_settings/records" && r.Method == http.MethodPost:
		// The real hook: emails become the owner's address, whatever was sent.
		owner := h.users[str("user")]
		h.settID = h.id()
		h.settings = map[string]any{"chartTime": "1h", "emails": []any{owner.Email}}
		reply(map[string]any{"id": h.settID, "settings": h.settings})
	case strings.HasPrefix(path, "/api/collections/user_settings/records/") && r.Method == http.MethodPatch:
		h.settings, _ = body["settings"].(map[string]any)
		reply(map[string]any{"id": h.settID, "settings": h.settings})
	case path == "/api/collections/alerts/records":
		var items []alertSpec
		for _, a := range h.alerts {
			items = append(items, a)
		}
		reply(map[string]any{"items": items})
	case path == "/api/beszel/user-alerts" && r.Method == http.MethodPost:
		v, _ := body["value"].(float64)
		m, _ := body["min"].(float64)
		h.alerts[str("name")] = alertSpec{str("name"), v, uint8(m)}
		reply(map[string]bool{"success": true})
	case path == "/api/beszel/user-alerts" && r.Method == http.MethodDelete:
		delete(h.alerts, str("name"))
		reply(map[string]bool{"success": true})
	default:
		fail(404, map[string]string{"message": "fake: no route " + r.Method + " " + path})
	}
}

func testSetupConfig(url string) setupConfig {
	return setupConfig{
		BeszelURL: url, System: "kyles-nas", Owner: "owner@example.com", Password: "pw",
		ReporterEmail: defaultReporterEmail,
		SMTPHost:      "smtp.example.com", SMTPPort: "587", SMTPUser: "nas@example.com",
		From: "nas@example.com", NotifyTo: []string{"ops@example.com"},
	}
}

func runFake(t *testing.T, h *fakeSetupHub, c setupConfig) (map[string]string, string, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	c.BeszelURL = srv.URL
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(log.Writer())
	var out bytes.Buffer
	err := runSetup(context.Background(), c, &out)
	kv := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	return kv, logs.String(), err
}

// The first run builds everything; the second, with the values the first
// handed back and no SMTP password, must not write anything at all.
func TestSetupFreshThenNothingChanges(t *testing.T) {
	h := newFakeSetupHub()
	c := testSetupConfig("")
	c.HubSMTPPass = "app-password"
	kv, _, err := runFake(t, h, c)
	if err != nil {
		t.Fatal(err)
	}
	if kv["MON_HUB_KEY"] != h.key || kv["REPORT_BESZEL_PASSWORD"] == "" {
		t.Fatalf("values for .env: %v", kv)
	}
	if got := mustJSON(h.settings["emails"]); got != `["ops@example.com"]` {
		t.Errorf("alert emails = %s, want REPORT_TO - the create hook's owner address survived", got)
	}
	if len(h.alerts) != 8 || h.smtp["enabled"] != true {
		t.Errorf("alerts %d, smtp %v", len(h.alerts), h.smtp)
	}

	h.writes = nil
	c.HubSMTPPass = ""
	c.ReporterPass = kv["REPORT_BESZEL_PASSWORD"]
	kv2, logs, err := runFake(t, h, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.writes) != 0 {
		t.Errorf("second run wrote %v", h.writes)
	}
	if _, rotated := kv2["REPORT_BESZEL_PASSWORD"]; rotated {
		t.Error("second run rotated the reporter's password")
	}
	if strings.Contains(logs, "WARNING") {
		t.Errorf("warned with mail enabled:\n%s", logs)
	}
}

func TestSetupWillNotDemoteAnAccount(t *testing.T) {
	h := newFakeSetupHub()
	h.users["a"] = &fakeUser{"a", "owner@example.com", "pw", "admin"}
	h.users["b"] = &fakeUser{"b", defaultReporterEmail, "theirs", "user"}
	_, _, err := runFake(t, h, testSetupConfig(""))
	if err == nil || !strings.Contains(err.Error(), "will not demote") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if b := h.users["b"]; b.Role != "user" || b.Password != "theirs" {
		t.Fatalf("account changed: %+v", b)
	}
}

func TestSetupRefusesOwnerAsReporter(t *testing.T) {
	c := testSetupConfig("")
	c.ReporterEmail = "Owner@Example.com"
	if err := c.validate(); err == nil {
		t.Fatal("reporter = owner accepted")
	}
}

// Everything a later step needs is checked before the first write, so a
// failing run leaves the hub as it found it.
func TestSetupValidatesBeforeWriting(t *testing.T) {
	c := testSetupConfig("")
	c.HubSMTPPass = "app-password"
	c.SMTPUser = ""
	if err := c.validate(); err == nil {
		t.Fatal("SMTP password with no SMTP_USERNAME accepted")
	}
	c = testSetupConfig("")
	c.NotifyTo = []string{"not an address"}
	if err := c.validate(); err == nil {
		t.Fatal("bad REPORT_TO accepted")
	}
}

func TestSetupCorrectsHostWithPending(t *testing.T) {
	h := newFakeSetupHub()
	h.users["a"] = &fakeUser{"a", "owner@example.com", "pw", "admin"}
	h.systems["s"] = map[string]any{"id": "s", "name": "kyles-nas", "host": "10.0.0.5", "port": "45876", "status": "up"}
	if _, _, err := runFake(t, h, testSetupConfig("")); err != nil {
		t.Fatal(err)
	}
	if s := h.systems["s"]; s["host"] != agentSocket || s["status"] != "pending" {
		t.Fatalf("system after correction: %v - without pending the hub keeps the old host", s)
	}
}

func TestSetupMakesAlertsExactlyTheFile(t *testing.T) {
	h := newFakeSetupHub()
	h.users["a"] = &fakeUser{"a", "owner@example.com", "pw", "admin"}
	h.alerts["Bandwidth"] = alertSpec{"Bandwidth", 50, 10}
	h.alerts["Memory"] = alertSpec{"Memory", 60, 30}
	if _, _, err := runFake(t, h, testSetupConfig("")); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.alerts["Bandwidth"]; ok {
		t.Error("an alert not in alerts.json survived")
	}
	if h.alerts["Memory"].Value != memAvgAct {
		t.Errorf("Memory = %v, want %v", h.alerts["Memory"].Value, memAvgAct)
	}
}

func TestSetupRejectsAMalformedKey(t *testing.T) {
	h := newFakeSetupHub()
	h.key = "${SMTP_PASSWORD}"
	kv, _, err := runFake(t, h, testSetupConfig(""))
	if err == nil || kv["MON_HUB_KEY"] != "" {
		t.Fatalf("malformed key accepted: err %v, kv %v", err, kv)
	}
}

func TestSetupWarnsWhenHubMailIsOff(t *testing.T) {
	h := newFakeSetupHub()
	_, logs, err := runFake(t, h, testSetupConfig("")) // no SMTP password, mail off
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "WARNING - the hub's mail is OFF") {
		t.Fatalf("no warning:\n%s", logs)
	}
}

func TestSetupSaysWhenSomeoneElseMayHoldTheHub(t *testing.T) {
	h := newFakeSetupHub()
	h.users["x"] = &fakeUser{"x", "owner@example.com", "not-yours", "admin"}
	_, _, err := runFake(t, h, testSetupConfig(""))
	if err == nil || !strings.Contains(err.Error(), "someone else may have") {
		t.Fatalf("want the someone-else hint, got %v", err)
	}
	if len(h.writes) != 0 {
		t.Fatalf("wrote after a failed sign-in: %v", h.writes)
	}
}
