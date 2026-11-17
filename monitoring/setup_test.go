package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	alerts   map[string]map[string]alertSpec // by system id, then name
	settings map[string]any                  // user_settings.settings, one owner
	settID   string
	smtp     map[string]any
	key      string
	writes   []string // method + path of every write, for "changed nothing" checks
	nextID   int

	authAlert      map[string]any // the users collection's authAlert, on as Beszel ships it
	passwordLogins []string       // "<collection> <identity>" of every password sign-in
}

type fakeUser struct {
	ID, Email, Password, Role string
}

func newFakeSetupHub() *fakeSetupHub {
	return &fakeSetupHub{
		users:   map[string]*fakeUser{},
		systems: map[string]map[string]any{},
		alerts:  map[string]map[string]alertSpec{},
		smtp:    map[string]any{"enabled": false},
		authAlert: map[string]any{"enabled": true,
			"emailTemplate": map[string]any{"subject": "Login from a new location"}},
		key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGhTXCGQphIff33c7VX4fss+MBT/qMjWdiXTX7CqCi5w",
	}
}

// alertsOf is the alerts on the system called name.
func (h *fakeSetupHub) alertsOf(name string) map[string]alertSpec {
	for id, s := range h.systems {
		if s["name"] == name {
			return h.alerts[id]
		}
	}
	return nil
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
	isAuth := strings.HasSuffix(path, "/auth-with-password") || strings.HasSuffix(path, "/auth-refresh") ||
		strings.HasPrefix(path, "/api/collections/users/impersonate/")
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
		h.passwordLogins = append(h.passwordLogins, strings.Split(path, "/")[3]+" "+str("identity"))
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
	case path == "/api/collections/users/auth-refresh":
		// A user token is "u:<id>"; the superuser's is not a users token.
		if u := h.users[strings.TrimPrefix(auth, "u:")]; u != nil && strings.HasPrefix(auth, "u:") {
			reply(map[string]any{"token": auth, "record": map[string]string{"id": u.ID, "role": u.Role, "email": u.Email}})
			return
		}
		fail(401, map[string]string{"message": "The request requires valid record authorization token."})
	case strings.HasPrefix(path, "/api/collections/users/impersonate/") && r.Method == http.MethodPost:
		u := h.users[strings.TrimPrefix(path, "/api/collections/users/impersonate/")]
		if auth != "su" || u == nil {
			fail(403, map[string]string{"message": "superuser only"})
			return
		}
		reply(map[string]any{"token": "u:" + u.ID, "record": map[string]string{"id": u.ID, "role": u.Role, "email": u.Email}})
	case path == "/api/collections/users":
		if auth != "su" {
			fail(403, map[string]string{"message": "superuser only"})
			return
		}
		if r.Method == http.MethodPatch {
			h.authAlert, _ = body["authAlert"].(map[string]any)
		}
		reply(map[string]any{"name": "users", "authAlert": h.authAlert})
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
		items := []alertSpec{}
		for _, a := range h.alerts[filter] {
			items = append(items, a)
		}
		reply(map[string]any{"items": items})
	case path == "/api/beszel/user-alerts":
		systems, _ := body["systems"].([]any)
		for _, id := range systems {
			id := id.(string)
			if h.alerts[id] == nil {
				h.alerts[id] = map[string]alertSpec{}
			}
			if r.Method == http.MethodDelete {
				delete(h.alerts[id], str("name"))
				continue
			}
			v, _ := body["value"].(float64)
			m, _ := body["min"].(float64)
			h.alerts[id][str("name")] = alertSpec{str("name"), v, uint8(m)}
		}
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
	if len(h.alertsOf("kyles-nas")) != 8 || h.smtp["enabled"] != true {
		t.Errorf("alerts %d, smtp %v", len(h.alertsOf("kyles-nas")), h.smtp)
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

// The hub refuses what net/mail accepts. `you@github` is a real Tailscale login
// and the first install failed on it, with a 500 from the first-run page.
func TestSetupRefusesAnEmailTheHubWould(t *testing.T) {
	for _, owner := range []string{"ky1ejs@github", "Kyle <kyle@example.com>", "not an address"} {
		c := testSetupConfig("")
		c.Owner = owner
		if err := c.validate(); err == nil {
			t.Errorf("owner %q accepted", owner)
		}
	}
	c := testSetupConfig("")
	c.ReporterEmail = "reporter@homelab"
	if err := c.validate(); err == nil {
		t.Error("reporter without a dot in its domain accepted")
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
	h.systems["s"] = map[string]any{"id": "s", "name": "kyles-nas", "host": agentSocket, "port": agentPort}
	h.alerts["s"] = map[string]alertSpec{
		"Bandwidth": {"Bandwidth", 50, 10},
		"Memory":    {"Memory", 60, 30},
	}
	if _, _, err := runFake(t, h, testSetupConfig("")); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.alerts["s"]["Bandwidth"]; ok {
		t.Error("an alert not in alerts.json survived")
	}
	if h.alerts["s"]["Memory"].Value != memAvgAct {
		t.Errorf("Memory = %v, want %v", h.alerts["s"]["Memory"].Value, memAvgAct)
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

// The mac studio is a second system at its LAN address, with its own alerts
// and none of the NAS's; a second run changes nothing.
func TestSetupAddsTheMacStudio(t *testing.T) {
	h := newFakeSetupHub()
	c := testSetupConfig("")
	c.MacStudioHost = "192.168.40.2"
	kv, _, err := runFake(t, h, c)
	if err != nil {
		t.Fatal(err)
	}
	var mac map[string]any
	for _, s := range h.systems {
		if s["name"] == "mac-studio" {
			mac = s
		}
	}
	if mac == nil || mac["host"] != "192.168.40.2" || mac["port"] != macAgentPort {
		t.Fatalf("mac studio system: %v", mac)
	}
	got := h.alertsOf("mac-studio")
	if len(got) != 2 || got["Disk"].Value != macStudioHost.Disk.Act {
		t.Errorf("mac studio alerts: %v", got)
	}
	if _, ok := got["Memory"]; ok {
		t.Error("the mac studio got the NAS's memory alert")
	}
	if len(h.alertsOf("kyles-nas")) != 8 {
		t.Errorf("NAS alerts: %v", h.alertsOf("kyles-nas"))
	}

	h.writes = nil
	c.ReporterPass = kv["REPORT_BESZEL_PASSWORD"]
	if _, _, err := runFake(t, h, c); err != nil {
		t.Fatal(err)
	}
	if len(h.writes) != 0 {
		t.Errorf("second run wrote %v", h.writes)
	}
}

func TestSetupLeavesTheMacStudioOutWhenBlank(t *testing.T) {
	h := newFakeSetupHub()
	_, logs, err := runFake(t, h, testSetupConfig(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.systems) != 1 {
		t.Errorf("systems: %v", h.systems)
	}
	if !strings.Contains(logs, "MON_MAC_STUDIO_HOST is blank") {
		t.Errorf("no line saying the mac studio was left out:\n%s", logs)
	}
}

func TestSetupRefusesAMalformedMacStudioHost(t *testing.T) {
	for _, bad := range []string{"192.168.40.2:45876", "http://192.168.40.2", "mac studio", "-mac", "mac..lan"} {
		c := testSetupConfig("")
		c.MacStudioHost = bad
		if err := c.validate(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"192.168.40.2", "mac-studio.lan", "fd00::2"} {
		c := testSetupConfig("")
		c.MacStudioHost = good
		if err := c.validate(); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
}

// configuredHub is a hub after a first run with a password, and the config
// for a token run against it: no password, your account's token, and the
// reporter's password the first run handed back.
func configuredHub(t *testing.T) (*fakeSetupHub, setupConfig) {
	t.Helper()
	h := newFakeSetupHub()
	c := testSetupConfig("")
	c.HubSMTPPass = "app-password"
	kv, _, err := runFake(t, h, c)
	if err != nil {
		t.Fatal(err)
	}
	h.writes = nil
	c.HubSMTPPass, c.Password = "", ""
	c.ReporterPass = kv["REPORT_BESZEL_PASSWORD"]
	c.LoginAlertsOff = kv["MON_HUB_LOGIN_ALERTS"] == "off"
	for _, u := range h.users {
		if u.Email == c.Owner {
			c.Token = "u:" + u.ID
		}
	}
	return h, c
}

// A re-run on a configured hub needs no password: your account's token does
// every step but the superuser's, and the run still changes nothing.
func TestSetupByTokenChangesNothing(t *testing.T) {
	h, c := configuredHub(t)
	c.MacStudioHost = "192.0.2.10"
	if _, _, err := runFake(t, h, c); err != nil {
		t.Fatal(err)
	}
	h.writes = nil
	kv, logs, err := runFake(t, h, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.writes) != 0 {
		t.Errorf("token run wrote %v", h.writes)
	}
	if _, rotated := kv["REPORT_BESZEL_PASSWORD"]; rotated {
		t.Error("token run rotated the reporter's password")
	}
	for _, want := range []string{"by your Tailscale login", "smtp: not checked", "reporter: " + defaultReporterEmail + " already works", "mac-studio already present"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}

// A token run adds what is new, like the mac studio, without a password.
func TestSetupByTokenAddsTheMacStudio(t *testing.T) {
	h, c := configuredHub(t)
	c.MacStudioHost = "192.0.2.10"
	if _, _, err := runFake(t, h, c); err != nil {
		t.Fatal(err)
	}
	if len(h.alertsOf("mac-studio")) == 0 {
		t.Fatal("mac studio not added")
	}
}

// What only the superuser can do stops a token run before its first write,
// with the error bin/homelab turns into a password prompt.
func TestSetupByTokenNeedsAPasswordForTheSuperusersSteps(t *testing.T) {
	cases := map[string]func(*fakeSetupHub, *setupConfig){
		"an unclaimed hub": func(h *fakeSetupHub, c *setupConfig) {
			h.users = map[string]*fakeUser{}
		},
		"a reporter that does not sign in": func(h *fakeSetupHub, c *setupConfig) {
			c.ReporterPass = "stale"
		},
		"login emails not yet turned off": func(h *fakeSetupHub, c *setupConfig) {
			c.LoginAlertsOff = false
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h, c := configuredHub(t)
			breakIt(h, &c)
			_, _, err := runFake(t, h, c)
			if !errors.As(err, new(needsPassword)) {
				t.Fatalf("want needsPassword, got %v", err)
			}
			if len(h.writes) != 0 {
				t.Errorf("wrote %v before stopping", h.writes)
			}
		})
	}
	c := testSetupConfig("")
	c.Password, c.Token, c.HubSMTPPass = "", "u:x", "app-password"
	if err := c.validate(); !errors.As(err, new(needsPassword)) {
		t.Fatalf("SMTP password with no Beszel password: want needsPassword, got %v", err)
	}
}

// The token must be your own account's: auth-refresh says whose it is.
func TestSetupRefusesATokenForAnotherAccount(t *testing.T) {
	h, c := configuredHub(t)
	for _, u := range h.users {
		if u.Email == defaultReporterEmail {
			c.Token = "u:" + u.ID
		}
	}
	_, _, err := runFake(t, h, c)
	if err == nil || !strings.Contains(err.Error(), "not owner@example.com") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if len(h.writes) != 0 {
		t.Errorf("wrote %v", h.writes)
	}
}

func TestSetupReadsTheTokenFromStdin(t *testing.T) {
	t.Setenv("SETUP_OWNER_EMAIL", "owner@example.com")
	c, err := loadSetupConfig(strings.NewReader("\n\ntok\n"))
	if err != nil || c.Token != "tok" || c.Password != "" {
		t.Fatalf("config %+v, err %v", c, err)
	}
	if _, err := loadSetupConfig(strings.NewReader("\n\n\n")); err == nil {
		t.Fatal("no password and no token accepted")
	}
}

// The reporter's "Login from a new location" emails go to an address that
// cannot exist, so they bounce to you: setup turns them off for the users
// collection, keeping the template, and records it in .env for token runs.
// Your own account is never signed in to by password, so it records no new
// login either; only the superuser is.
func TestSetupTurnsOffLoginEmailsAndNeverSignsInAsYouByPassword(t *testing.T) {
	h := newFakeSetupHub()
	c := testSetupConfig("")
	kv, logs, err := runFake(t, h, c)
	if err != nil {
		t.Fatal(err)
	}
	if h.authAlert["enabled"] != false || h.authAlert["emailTemplate"] == nil {
		t.Errorf("users authAlert = %v, want off with its template kept", h.authAlert)
	}
	if kv["MON_HUB_LOGIN_ALERTS"] != "off" {
		t.Errorf("values for .env: %v", kv)
	}
	if !strings.Contains(logs, "logins: new-login emails turned off") {
		t.Errorf("no line saying so:\n%s", logs)
	}
	for _, l := range h.passwordLogins {
		if l == "users "+c.Owner {
			t.Errorf("signed in as your dashboard account by password: %v", h.passwordLogins)
		}
	}

	h.writes = nil
	c.ReporterPass = kv["REPORT_BESZEL_PASSWORD"]
	kv, logs, err = runFake(t, h, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.writes) != 0 || kv["MON_HUB_LOGIN_ALERTS"] != "off" || !strings.Contains(logs, "already off") {
		t.Errorf("second run: writes %v, kv %v\n%s", h.writes, kv, logs)
	}
}
