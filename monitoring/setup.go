package main

// `homelab-report -setup`: everything about the hub that can be stated in
// code, applied over its API. `homelab setup monitoring` runs this inside the
// weekly-report container -- so it reaches the hub over the compose network
// with a password, never through the trusted identity header -- after doing the
// host-side steps itself. See README.md#setup.
//
// IDEMPOTENT, and that is the point: the same command is the first install and
// the restore. Every step looks before it writes, and a second run against a
// configured hub changes nothing and says so.
//
// What it does, in order:
//
//	claim       the first-run account, if the hub has none (admin + superuser)
//	system      the one system, on the agent's unix socket
//	key         read the hub's public key, for MON_HUB_KEY
//	reporter    the readonly account the weekly report logs in as
//	smtp        the hub's own mail settings, only when a password is given
//	notify      where your alert emails go: REPORT_TO
//	alerts      alerts.json, exactly: thresholds not in the file are removed
//
// Secrets arrive on stdin, one per line: your Beszel password, then the hub's
// SMTP app password (blank to leave mail settings alone). Not flags and not
// environment variables, because both are visible to anything that can list
// processes or inspect containers. Values for .env go to stdout as KEY=VALUE
// lines for bin/homelab to write; everything else is a log line on stderr.
//
// The endpoints are Beszel's and PocketBase's at the pinned version, read from
// their source: /api/beszel/first-run and create-user (internal/users),
// /api/beszel/user-alerts (internal/alerts), /api/settings (PocketBase core).
// A Beszel upgrade is a change to this file too, which is what CI's end-to-end
// run against the pinned images is there to catch.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed alerts.json
var alertsJSON []byte

type alertSpec struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Min   uint8   `json:"min"` // minutes over the threshold before it fires
}

// knownAlerts are Beszel's alert names at the pinned version
// (internal/site/src/lib/alerts.ts). The hub does NOT validate the name it is
// given, so a typo would create an alert that never fires; alerts_test.go
// checks alerts.json against this list instead.
var knownAlerts = map[string]bool{
	"Status": true, "CPU": true, "CPUIOWait": true, "CPUSteal": true, "Memory": true,
	"Disk": true, "Bandwidth": true, "NetworkMonitorLoss": true, "GPU": true,
	"Temperature": true, "LoadAvg1": true, "LoadAvg5": true, "LoadAvg15": true,
	"Battery": true, "ContainerHealth": true, "SystemdFailed": true,
}

func loadAlerts() ([]alertSpec, error) {
	var specs []alertSpec
	if err := json.Unmarshal(alertsJSON, &specs); err != nil {
		return nil, fmt.Errorf("alerts.json: %w", err)
	}
	seen := map[string]bool{}
	for _, a := range specs {
		if !knownAlerts[a.Name] {
			return nil, fmt.Errorf("alerts.json: %q is not a Beszel alert name", a.Name)
		}
		if seen[a.Name] {
			return nil, fmt.Errorf("alerts.json: %q listed twice", a.Name)
		}
		seen[a.Name] = true
	}
	return specs, nil
}

const (
	// Where the agent listens, as the hub sees it (docker-compose.yml).
	agentSocket = "/beszel_socket/beszel.sock"
	// Stored by the hub for every system; ignored for a unix socket.
	agentPort = "45876"
	// Nothing is ever sent to the reporter's address, so it need not exist.
	defaultReporterEmail = "reporter@homelab.invalid"
)

type setupConfig struct {
	BeszelURL   string
	System      string
	Owner       string // your Beszel email: your Tailscale login
	Password    string
	HubSMTPPass string

	ReporterEmail string
	ReporterPass  string // the current one from .env, possibly blank

	SMTPHost, SMTPPort, SMTPUser string
	From                         string
	NotifyTo                     []string
}

func loadSetupConfig(stdin io.Reader) (setupConfig, error) {
	c := setupConfig{
		BeszelURL:     env("BESZEL_URL", "http://beszel:8090"),
		System:        env("REPORT_SYSTEM", "kyles-nas"),
		Owner:         env("SETUP_OWNER_EMAIL", ""),
		ReporterEmail: env("REPORT_BESZEL_EMAIL", defaultReporterEmail),
		ReporterPass:  env("REPORT_BESZEL_PASSWORD", ""),
		SMTPHost:      env("SMTP_HOST", ""),
		SMTPPort:      env("SMTP_PORT", "587"),
		SMTPUser:      env("SMTP_USERNAME", ""),
		From:          env("REPORT_FROM", ""),
	}
	for _, a := range strings.Split(env("REPORT_TO", ""), ",") {
		if a = strings.TrimSpace(a); a != "" {
			c.NotifyTo = append(c.NotifyTo, a)
		}
	}
	r := bufio.NewReader(stdin)
	line := func() string {
		s, _ := r.ReadString('\n')
		return strings.TrimRight(s, "\r\n")
	}
	c.Password = line()
	c.HubSMTPPass = line()

	if c.Owner == "" {
		return c, errors.New("SETUP_OWNER_EMAIL is not set: it is your Tailscale login, and bin/homelab passes it")
	}
	if c.Password == "" {
		return c, errors.New("no password on stdin")
	}
	return c, nil
}

// call is one JSON request with a given token ("" for none).
func (b *beszel) call(ctx context.Context, method, path, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	saved := b.token
	b.token = "" // do() would otherwise add its own
	defer func() { b.token = saved }()
	if out == nil {
		out = &json.RawMessage{}
	}
	return b.do(req, out)
}

type authResult struct {
	Token  string `json:"token"`
	Record struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"record"`
}

func (b *beszel) authWith(ctx context.Context, collection, identity, password string) (authResult, error) {
	var out authResult
	err := b.call(ctx, http.MethodPost, "/api/collections/"+collection+"/auth-with-password", "",
		map[string]string{"identity": identity, "password": password}, &out)
	return out, err
}

func (b *beszel) waitHealthy(ctx context.Context, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		err := b.call(ctx, http.MethodGet, "/api/health", "", nil, nil)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("hub not healthy after %s: %w", within, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func randomPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	// URL-safe alphabet: nothing a .env line or a shell would reinterpret.
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// runSetup applies everything, writing KEY=VALUE lines to out for .env.
func runSetup(ctx context.Context, c setupConfig, out io.Writer) error {
	alerts, err := loadAlerts()
	if err != nil {
		return err
	}
	b := newBeszel(c.BeszelURL, "", "")
	if err := b.waitHealthy(ctx, 90*time.Second); err != nil {
		return err
	}

	// claim ---------------------------------------------------------------
	var first struct {
		FirstRun bool `json:"firstRun"`
	}
	if err := b.call(ctx, http.MethodGet, "/api/beszel/first-run", "", nil, &first); err != nil {
		return fmt.Errorf("first-run check: %w", err)
	}
	if first.FirstRun {
		if err := b.call(ctx, http.MethodPost, "/api/beszel/create-user", "",
			map[string]string{"email": c.Owner, "password": c.Password}, nil); err != nil {
			return fmt.Errorf("claiming the hub: %w", err)
		}
		log.Printf("claim: created %s as admin and superuser", c.Owner)
	} else {
		log.Printf("claim: hub already has an account")
	}

	user, err := b.authWith(ctx, "users", c.Owner, c.Password)
	if err != nil {
		return fmt.Errorf("signing in as %s (the hub is claimed, so this must be its password): %w", c.Owner, err)
	}
	if user.Record.Role == "readonly" {
		return fmt.Errorf("%s is a readonly account; setup needs yours", c.Owner)
	}
	su, err := b.authWith(ctx, "_superusers", c.Owner, c.Password)
	if err != nil {
		return fmt.Errorf("signing in to /_/ as %s: %w", c.Owner, err)
	}

	// system --------------------------------------------------------------
	sysID, err := ensureSystem(ctx, b, user, c.System)
	if err != nil {
		return err
	}

	// key -----------------------------------------------------------------
	var info struct {
		Key string `json:"key"`
	}
	if err := b.call(ctx, http.MethodGet, "/api/beszel/info", user.Token, nil, &info); err != nil || info.Key == "" {
		return fmt.Errorf("reading the hub's key: %v", err)
	}
	fmt.Fprintf(out, "MON_HUB_KEY=%s\n", info.Key)

	// reporter ------------------------------------------------------------
	if err := ensureReporter(ctx, b, su.Token, c, out); err != nil {
		return err
	}

	// smtp ----------------------------------------------------------------
	if c.HubSMTPPass == "" {
		log.Printf("smtp: no hub password given, mail settings left as they are")
	} else {
		if err := applySMTP(ctx, b, su.Token, c); err != nil {
			return err
		}
	}

	// notify --------------------------------------------------------------
	if err := ensureNotify(ctx, b, user, c.NotifyTo); err != nil {
		return err
	}

	// alerts --------------------------------------------------------------
	return ensureAlerts(ctx, b, user, sysID, alerts)
}

func ensureSystem(ctx context.Context, b *beszel, user authResult, name string) (string, error) {
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Host string `json:"host"`
		} `json:"items"`
	}
	q := url.Values{"filter": {"name = " + pbQuote(name)}}
	if err := b.call(ctx, http.MethodGet, "/api/collections/systems/records?"+q.Encode(), user.Token, nil, &list); err != nil {
		return "", fmt.Errorf("listing systems: %w", err)
	}
	if len(list.Items) == 0 {
		var created struct {
			ID string `json:"id"`
		}
		err := b.call(ctx, http.MethodPost, "/api/collections/systems/records", user.Token, map[string]any{
			"name": name, "host": agentSocket, "port": agentPort, "users": []string{user.Record.ID},
		}, &created)
		if err != nil {
			return "", fmt.Errorf("adding system %s: %w", name, err)
		}
		log.Printf("system: added %s on %s", name, agentSocket)
		return created.ID, nil
	}
	s := list.Items[0]
	if s.Host != agentSocket {
		if err := b.call(ctx, http.MethodPatch, "/api/collections/systems/records/"+s.ID, user.Token,
			map[string]any{"host": agentSocket, "port": agentPort}, nil); err != nil {
			return "", fmt.Errorf("correcting %s's host: %w", name, err)
		}
		log.Printf("system: %s pointed at %s (was %q)", name, agentSocket, s.Host)
	} else {
		log.Printf("system: %s already present", name)
	}
	return s.ID, nil
}

// ensureReporter keeps the current reporter credentials if they work and are
// readonly, and otherwise creates or resets the account with a new random
// password. You never need to know that password: it goes straight to .env.
func ensureReporter(ctx context.Context, b *beszel, suToken string, c setupConfig, out io.Writer) error {
	if c.ReporterPass != "" {
		if r, err := b.authWith(ctx, "users", c.ReporterEmail, c.ReporterPass); err == nil && r.Record.Role == "readonly" {
			log.Printf("reporter: %s already works and is readonly", c.ReporterEmail)
			return nil
		}
	}
	pass, err := randomPassword()
	if err != nil {
		return err
	}
	fields := map[string]any{
		"email": c.ReporterEmail, "password": pass, "passwordConfirm": pass,
		"role": "readonly", "verified": true,
	}
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	q := url.Values{"filter": {"email = " + pbQuote(c.ReporterEmail)}}
	if err := b.call(ctx, http.MethodGet, "/api/collections/users/records?"+q.Encode(), suToken, nil, &list); err != nil {
		return fmt.Errorf("looking up the reporter: %w", err)
	}
	if len(list.Items) == 0 {
		err = b.call(ctx, http.MethodPost, "/api/collections/users/records", suToken, fields, nil)
		log.Printf("reporter: created %s (readonly)", c.ReporterEmail)
	} else {
		err = b.call(ctx, http.MethodPatch, "/api/collections/users/records/"+list.Items[0].ID, suToken, fields, nil)
		log.Printf("reporter: reset %s (readonly, new password)", c.ReporterEmail)
	}
	if err != nil {
		return fmt.Errorf("writing the reporter account: %w", err)
	}
	fmt.Fprintf(out, "REPORT_BESZEL_EMAIL=%s\nREPORT_BESZEL_PASSWORD=%s\n", c.ReporterEmail, pass)
	return nil
}

func applySMTP(ctx context.Context, b *beszel, suToken string, c setupConfig) error {
	if c.SMTPHost == "" || c.SMTPUser == "" || c.From == "" {
		return errors.New("smtp: SMTP_HOST, SMTP_USERNAME and REPORT_FROM must be set to configure the hub's mail")
	}
	port, err := strconv.Atoi(c.SMTPPort)
	if err != nil {
		return fmt.Errorf("smtp: SMTP_PORT %q: %w", c.SMTPPort, err)
	}
	from, err := mail.ParseAddress(c.From)
	if err != nil {
		return fmt.Errorf("smtp: REPORT_FROM %q: %w", c.From, err)
	}
	// PocketBase: tls=true is implicit TLS (465); false negotiates STARTTLS.
	body := map[string]any{
		"smtp": map[string]any{
			"enabled": true, "host": c.SMTPHost, "port": port,
			"username": c.SMTPUser, "password": c.HubSMTPPass,
			"tls": port == 465, "authMethod": "PLAIN",
		},
		"meta": map[string]any{"senderAddress": from.Address, "senderName": "homelab monitoring"},
	}
	if err := b.call(ctx, http.MethodPatch, "/api/settings", suToken, body, nil); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	log.Printf("smtp: hub mail set to %s:%d as %s", c.SMTPHost, port, c.SMTPUser)
	return nil
}

// ensureNotify sets the addresses your alert emails go to, keeping every other
// UI preference in the same record (chart time, units, webhooks).
func ensureNotify(ctx context.Context, b *beszel, user authResult, to []string) error {
	if len(to) == 0 {
		log.Printf("notify: REPORT_TO is blank, alert addresses left as they are")
		return nil
	}
	var list struct {
		Items []struct {
			ID       string         `json:"id"`
			Settings map[string]any `json:"settings"`
		} `json:"items"`
	}
	q := url.Values{"filter": {"user = " + pbQuote(user.Record.ID)}}
	if err := b.call(ctx, http.MethodGet, "/api/collections/user_settings/records?"+q.Encode(), user.Token, nil, &list); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	if len(list.Items) == 0 {
		err := b.call(ctx, http.MethodPost, "/api/collections/user_settings/records", user.Token, map[string]any{
			"user": user.Record.ID, "settings": map[string]any{"emails": to},
		}, nil)
		if err != nil {
			return fmt.Errorf("notify: %w", err)
		}
		log.Printf("notify: alerts go to %s", strings.Join(to, ", "))
		return nil
	}
	rec := list.Items[0]
	if rec.Settings == nil {
		rec.Settings = map[string]any{}
	}
	if current, _ := json.Marshal(rec.Settings["emails"]); string(current) == mustJSON(to) {
		log.Printf("notify: alerts already go to %s", strings.Join(to, ", "))
		return nil
	}
	rec.Settings["emails"] = to
	if err := b.call(ctx, http.MethodPatch, "/api/collections/user_settings/records/"+rec.ID, user.Token,
		map[string]any{"settings": rec.Settings}, nil); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	log.Printf("notify: alerts now go to %s", strings.Join(to, ", "))
	return nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ensureAlerts makes the system's alerts exactly alerts.json: each listed alert
// written with overwrite, each unlisted one removed. An alert tuned in the UI
// and not in the file does not survive the next run -- the file is the truth,
// and the place to change a threshold is a commit.
func ensureAlerts(ctx context.Context, b *beszel, user authResult, sysID string, specs []alertSpec) error {
	var list struct {
		Items []struct {
			Name  string  `json:"name"`
			Value float64 `json:"value"`
			Min   uint8   `json:"min"`
		} `json:"items"`
	}
	q := url.Values{"filter": {"system = " + pbQuote(sysID)}, "perPage": {"200"}}
	if err := b.call(ctx, http.MethodGet, "/api/collections/alerts/records?"+q.Encode(), user.Token, nil, &list); err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	have := map[string]alertSpec{}
	for _, a := range list.Items {
		have[a.Name] = alertSpec{a.Name, a.Value, a.Min}
	}
	want := map[string]bool{}
	var changed int
	for _, s := range specs {
		want[s.Name] = true
		if h, ok := have[s.Name]; ok && h == s {
			continue
		}
		if err := b.call(ctx, http.MethodPost, "/api/beszel/user-alerts", user.Token, map[string]any{
			"name": s.Name, "value": s.Value, "min": s.Min, "systems": []string{sysID}, "overwrite": true,
		}, nil); err != nil {
			return fmt.Errorf("alerts: setting %s: %w", s.Name, err)
		}
		changed++
		log.Printf("alerts: %s set to %g for %d min", s.Name, s.Value, s.Min)
	}
	for name := range have {
		if want[name] {
			continue
		}
		if err := b.call(ctx, http.MethodDelete, "/api/beszel/user-alerts", user.Token, map[string]any{
			"name": name, "systems": []string{sysID},
		}, nil); err != nil {
			return fmt.Errorf("alerts: removing %s: %w", name, err)
		}
		changed++
		log.Printf("alerts: removed %s (not in alerts.json)", name)
	}
	if changed == 0 {
		log.Printf("alerts: all %d already match alerts.json", len(specs))
	}
	return nil
}
