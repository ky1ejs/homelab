package main

// `homelab-report -setup`: everything about the hub that can be stated in
// code, applied over its API. `homelab setup monitoring` runs this inside the
// weekly-report container -- so it reaches the hub over the compose network
// with your password or your account's token, where the trusted identity
// header is refused -- after doing the host-side steps itself. See
// README.md#setup.
//
// IDEMPOTENT, and that is the point: the same command is the first install and
// the restore. Every step looks before it writes, and a second run against a
// configured hub changes nothing and says so -- with one deliberate exception:
// typing the hub's SMTP password re-applies the mail settings, because the hub
// never returns the password to compare against, so typing one means "set it".
//
// EVERYTHING IS VALIDATED BEFORE THE FIRST WRITE. A run that would fail on a
// blank SMTP_USERNAME must fail before it has claimed the hub or rotated the
// reporter's password, not after -- otherwise the hub holds values .env never
// received.
//
// What it does, in order:
//
//	claim       the first-run account, if the hub has none (admin + superuser)
//	logins      the hub's new-login emails off for dashboard accounts, so the
//	            reporter's sign-ins do not mail its placeholder address
//	system      the NAS, on the agent's unix socket
//	key         read the hub's public key, for MON_HUB_KEY
//	reporter    the readonly account the weekly report logs in as
//	smtp        the hub's own mail settings, only when a password is given
//	notify      where your alert emails go: REPORT_TO
//	alerts      alerts.json, exactly: thresholds not in the file are removed
//	mac studio  the mac studio at MON_MAC_STUDIO_HOST, and its alerts; skipped
//	            when that is blank
//
// Secrets arrive on stdin, one per line: your Beszel password, the hub's SMTP
// app password (blank to leave mail settings alone), then a sign-in token.
// Not flags and not environment variables, because both are visible to
// anything that can list processes or inspect containers. Values for .env go
// to stdout as KEY=VALUE lines for bin/homelab to write; everything else is a
// log line on stderr.
//
// TWO WAYS TO SIGN IN. With your password, setup signs in as the /_/
// superuser and gets your account's token from it, and can do everything.
// With a token and no password, it signs in as you only: bin/homelab gets that token on the host from the hub's
// trusted identity header, which any process on the NAS can already use
// (README.md#trust-boundary), so it asks for nothing new. Without the
// superuser it cannot claim the hub, turn off the login emails, repair the
// reporter's account, or read or write the mail settings; it checks the first
// three before writing anything and stops with "needs your Beszel password",
// and skips the fourth. Every other step needs only your account, so a re-run
// on a configured hub asks for no password at all.
//
// NO PASSWORD SIGN-IN AS YOUR ACCOUNT. PocketBase emails "Login from a new
// location" when an account signs in by password from an address and user
// agent it has not seen, and every setup run is a new container. So setup
// gets your account's token without one: by impersonating it as the
// superuser, or from the token above. Only the superuser signs in by
// password, and its emails stay on: a /_/ sign-in you did not make is worth
// one.
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
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
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

// loadAlerts reads alerts.json: each host's alerts under its key (hosts.go).
// Every host must have an entry, so a host added to the report without alerts
// fails here rather than going unwatched.
func loadAlerts() (map[string][]alertSpec, error) {
	var byHost map[string][]alertSpec
	if err := json.Unmarshal(alertsJSON, &byHost); err != nil {
		return nil, fmt.Errorf("alerts.json: %w", err)
	}
	keys := map[string]bool{}
	for _, h := range hosts("") {
		keys[h.Key] = true
		if len(byHost[h.Key]) == 0 {
			return nil, fmt.Errorf("alerts.json: no alerts for %q", h.Key)
		}
	}
	for key, specs := range byHost {
		if !keys[key] {
			return nil, fmt.Errorf("alerts.json: %q is not a host the report knows", key)
		}
		seen := map[string]bool{}
		for _, a := range specs {
			if !knownAlerts[a.Name] {
				return nil, fmt.Errorf("alerts.json: %s: %q is not a Beszel alert name", key, a.Name)
			}
			if seen[a.Name] {
				return nil, fmt.Errorf("alerts.json: %s: %q listed twice", key, a.Name)
			}
			seen[a.Name] = true
		}
	}
	return byHost, nil
}

const (
	// Where the agent listens, as the hub sees it (docker-compose.yml).
	agentSocket = "/beszel_socket/beszel.sock"
	// Stored by the hub for every system; ignored for a unix socket.
	agentPort = "45876"
	// Where the mac studio's agent listens: port in mac/beszel/default.nix.
	macAgentPort = "45876"
	// Nothing is ever sent to the reporter's address, so it need not exist.
	defaultReporterEmail = "reporter@homelab.invalid"
)

type setupConfig struct {
	BeszelURL   string
	System      string
	Owner       string // your Beszel email: your Tailscale login
	Password    string
	HubSMTPPass string
	Token       string // your account's sign-in token, used when Password is blank

	ReporterEmail string
	ReporterPass  string // the current one from .env, possibly blank

	// MON_HUB_LOGIN_ALERTS from .env: "off" once a password run has turned
	// the login emails off, which is how a token run, which cannot read the
	// setting, knows it is done.
	LoginAlertsOff bool

	SMTPHost, SMTPPort, SMTPUser string
	From                         string
	NotifyTo                     []string

	// The mac studio's LAN address; blank leaves it out of the hub.
	MacStudioHost string
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
		MacStudioHost: env("MON_MAC_STUDIO_HOST", ""),

		LoginAlertsOff: env("MON_HUB_LOGIN_ALERTS", "") == "off",
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
	c.Token = line()

	if c.Owner == "" {
		return c, errors.New("SETUP_OWNER_EMAIL is not set: it is your Tailscale login, and bin/homelab passes it")
	}
	if c.Password == "" && c.Token == "" {
		return c, errors.New("no password or token on stdin")
	}
	return c, c.validate()
}

// validate checks everything a later step would need, so no step writes to the
// hub on a run that is going to fail.
func (c setupConfig) validate() error {
	if err := accountEmail(c.Owner); err != nil {
		return fmt.Errorf("your Beszel email: %w", err)
	}
	if err := accountEmail(c.ReporterEmail); err != nil {
		return fmt.Errorf("REPORT_BESZEL_EMAIL: %w", err)
	}
	// The reporter must never be your own account: setup makes the reporter
	// readonly and owns its password, and doing that to you would lock you out.
	if strings.EqualFold(strings.TrimSpace(c.ReporterEmail), strings.TrimSpace(c.Owner)) {
		return fmt.Errorf("REPORT_BESZEL_EMAIL is your own address (%s); it must be a separate account - leave it blank for %s", c.Owner, defaultReporterEmail)
	}
	for _, a := range c.NotifyTo {
		if _, err := mail.ParseAddress(a); err != nil {
			return fmt.Errorf("REPORT_TO: %q is not an email address", a)
		}
	}
	if c.MacStudioHost != "" && net.ParseIP(c.MacStudioHost) == nil && !hostnameShape.MatchString(c.MacStudioHost) {
		return fmt.Errorf("MON_MAC_STUDIO_HOST %q is not an IP address or a host name", c.MacStudioHost)
	}
	if c.HubSMTPPass != "" {
		if c.Password == "" {
			return errNeedsPassword("setting the hub's mail")
		}
		if c.SMTPHost == "" || c.SMTPUser == "" || c.From == "" {
			return errors.New("an SMTP password was given, so SMTP_HOST, SMTP_USERNAME and REPORT_FROM must be set in .env")
		}
		if _, err := strconv.Atoi(c.SMTPPort); err != nil {
			return fmt.Errorf("SMTP_PORT %q is not a number", c.SMTPPort)
		}
		if _, err := mail.ParseAddress(c.From); err != nil {
			return fmt.Errorf("REPORT_FROM %q is not an email address", c.From)
		}
	}
	return nil
}

// needsPassword is a step only the /_/ superuser can take, met on a run signed
// in by token. main tells bin/homelab with a SETUP_NEEDS_PASSWORD line, and
// bin/homelab asks for your password and runs setup again.
type needsPassword struct{ step string }

func (e needsPassword) Error() string { return e.step + " needs your Beszel password" }

func errNeedsPassword(step string) error { return needsPassword{step} }

// hostnameShape is a bare host name: no scheme, port or path, which the hub
// would store and fail to connect to.
var hostnameShape = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// accountEmail refuses an address the hub would refuse as an account's email,
// which is stricter than net/mail: PocketBase wants a bare address whose domain
// has a dot. A Tailscale login on a GitHub-backed tailnet, `you@github`, passes
// net/mail and fails the hub's first-run page with a 500, which is how this
// was found.
func accountEmail(a string) error {
	parsed, err := mail.ParseAddress(a)
	if err != nil || parsed.Address != a {
		return fmt.Errorf("%q is not a bare email address", a)
	}
	if at := strings.LastIndex(a, "@"); !strings.Contains(a[at+1:], ".") {
		return fmt.Errorf("%q has no dot in its domain, and the hub refuses it - use a real address", a)
	}
	return nil
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
		ID    string `json:"id"`
		Role  string `json:"role"`
		Email string `json:"email"`
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

// hubKeyShape is what a hub's public key looks like. Checked before it is
// printed for .env: compose interpolates ${...} inside .env values, so a key
// carrying one could pull another secret into the agent's environment.
var hubKeyShape = regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/]+={0,2}$`)

// runSetup applies everything, writing KEY=VALUE lines to out for .env. Each
// line is written as soon as the hub holds the value, so bin/homelab can save
// what a later failure would otherwise strand.
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
	if first.FirstRun && c.Password == "" {
		return errNeedsPassword("claiming the hub")
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

	user, su, err := signIn(ctx, b, c, first.FirstRun)
	if err != nil {
		return err
	}

	// logins --------------------------------------------------------------
	// Before the reporter check below, which is itself a sign-in.
	if su.Token == "" {
		if !c.LoginAlertsOff {
			return errNeedsPassword("turning off the hub's new-login emails, which bounce off the reporter's placeholder address")
		}
		// Only the superuser can read the setting, so a restored hub/ or a
		// Beszel upgrade that turned it back on goes unseen until a
		// --password run (README.md#login-emails).
		log.Printf("logins: off per MON_HUB_LOGIN_ALERTS in .env, not re-checked without your password")
	} else if err := ensureLoginAlertsOff(ctx, b, su.Token, out); err != nil {
		return err
	}

	// A token run cannot repair the reporter, so it finds out now, before
	// the first write, rather than after the system and key steps.
	if su.Token == "" {
		ok, err := reporterWorks(ctx, b, c)
		if err != nil {
			return err
		}
		if !ok {
			return errNeedsPassword("repairing the reporter's account (REPORT_BESZEL_* does not sign in)")
		}
	}

	// system --------------------------------------------------------------
	sysID, err := ensureSystem(ctx, b, user, c.System, agentSocket, agentPort)
	if err != nil {
		return err
	}

	// key -----------------------------------------------------------------
	var info struct {
		Key string `json:"key"`
	}
	if err := b.call(ctx, http.MethodGet, "/api/beszel/info", user.Token, nil, &info); err != nil {
		return fmt.Errorf("reading the hub's key: %w", err)
	}
	if !hubKeyShape.MatchString(info.Key) {
		return fmt.Errorf("the hub returned a key that is not an ed25519 public key (%d bytes); not writing it to .env", len(info.Key))
	}
	fmt.Fprintf(out, "MON_HUB_KEY=%s\n", info.Key)

	// reporter ------------------------------------------------------------
	if err := ensureReporter(ctx, b, su.Token, c, out); err != nil {
		return err
	}

	// smtp ----------------------------------------------------------------
	if su.Token == "" {
		log.Printf("smtp: not checked - this run signed in by your Tailscale login, and the mail settings need your password (homelab setup monitoring --password)")
	} else if err := ensureSMTP(ctx, b, su.Token, c); err != nil {
		return err
	}

	// notify --------------------------------------------------------------
	if err := ensureNotify(ctx, b, user, c.NotifyTo); err != nil {
		return err
	}

	// alerts --------------------------------------------------------------
	if err := ensureAlerts(ctx, b, user, c.System, sysID, alerts["nas"]); err != nil {
		return err
	}

	// mac studio ----------------------------------------------------------
	// The hub pulls from the mac studio's agent over the LAN
	// (mac/beszel/default.nix). Blank leaves the hub as it is: a mac studio
	// added earlier stays, and is removed in the hub's UI if that is meant.
	if c.MacStudioHost == "" {
		log.Printf("%s: MON_MAC_STUDIO_HOST is blank, not added", macStudioHost.System)
		return nil
	}
	macID, err := ensureSystem(ctx, b, user, macStudioHost.System, c.MacStudioHost, macAgentPort)
	if err != nil {
		return err
	}
	return ensureAlerts(ctx, b, user, macStudioHost.System, macID, alerts[macStudioHost.Key])
}

// signIn signs in as you, and as the /_/ superuser when a password was given.
// With only a token, su is empty and the caller skips the superuser's steps.
func signIn(ctx context.Context, b *beszel, c setupConfig, firstRun bool) (user, su authResult, err error) {
	if c.Password == "" {
		// auth-refresh with the token returns the account it belongs to, so
		// a token for anyone else is caught here rather than trusted.
		if err := b.call(ctx, http.MethodPost, "/api/collections/users/auth-refresh", c.Token, nil, &user); err != nil {
			return user, su, fmt.Errorf("signing in with the token from your Tailscale login: %w", err)
		}
		if !strings.EqualFold(user.Record.Email, c.Owner) {
			return user, su, fmt.Errorf("the token signs in as %q, not %s", user.Record.Email, c.Owner)
		}
		if user.Record.Role == "readonly" {
			return user, su, fmt.Errorf("%s is a readonly account; setup needs yours", c.Owner)
		}
		log.Printf("sign-in: %s, by your Tailscale login (no password needed)", c.Owner)
		return user, su, nil
	}
	su, err = b.authWith(ctx, "_superusers", c.Owner, c.Password)
	if err != nil {
		if firstRun {
			return user, su, fmt.Errorf("signing in to /_/ as %s straight after claiming the hub: %w", c.Owner, err)
		}
		// Either a mistyped password, or the hub was claimed by someone else.
		// The second is the one that matters, so say it.
		return user, su, fmt.Errorf("signing in to /_/ as %s failed: %w\n"+
			"  The hub already had an account. If it is yours, the password is wrong.\n"+
			"  If you never claimed it, someone else may have: stop the hub, delete\n"+
			"  MON_DATA_HOST_PATH/hub, and run setup again", c.Owner, err)
	}
	user, err = impersonate(ctx, b, su.Token, c.Owner)
	if err != nil {
		return user, su, err
	}
	if user.Record.Role == "readonly" {
		return user, su, fmt.Errorf("%s is a readonly account; setup needs yours", c.Owner)
	}
	return user, su, nil
}

// impersonate gets a short-lived token for your dashboard account from the
// superuser. Unlike a password sign-in it sends no "Login from a new
// location" email and records no login origin (PocketBase's
// recordAuthImpersonate passes no auth method).
func impersonate(ctx context.Context, b *beszel, suToken, email string) (authResult, error) {
	var user authResult
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	// Case-insensitive, because the /_/ superuser and your dashboard account
	// are separate records whose addresses can differ in case. The users
	// collection's unique index is case-sensitive, so two accounts can match;
	// setup then refuses to guess.
	q := url.Values{"filter": {"email:lower = " + pbQuote(strings.ToLower(email))}}
	if err := b.call(ctx, http.MethodGet, "/api/collections/users/records?"+q.Encode(), suToken, nil, &list); err != nil {
		return user, fmt.Errorf("looking up %s: %w", email, err)
	}
	if len(list.Items) != 1 {
		return user, fmt.Errorf("the hub has a /_/ superuser %s but %d dashboard accounts with that email (any case); setup needs exactly one, so fix the users collection in /_/, then run setup again", email, len(list.Items))
	}
	err := b.call(ctx, http.MethodPost, "/api/collections/users/impersonate/"+list.Items[0].ID, suToken,
		map[string]int{"duration": impersonateSeconds}, &user)
	if err != nil {
		return user, fmt.Errorf("getting a token for %s from /_/: %w", email, err)
	}
	return user, nil
}

// Long enough for a setup run that waits on a slow hub, short enough that the
// token is worthless soon after: it never leaves this process.
const impersonateSeconds = 15 * 60

// ensureLoginAlertsOff turns off PocketBase's "Login from a new location"
// emails for the users collection (the dashboard accounts), and tells
// bin/homelab to record that in .env.
//
// The reporter is a users account with a placeholder address on a domain that
// cannot exist (reporter@homelab.invalid), and it signs in by password from
// whichever container runs it: the weekly report, and setup's check that its
// password still works. Each new container address is a "new location", so
// its emails could only ever bounce back to your mailbox. The switch is per
// collection, so your own dashboard account loses them too; it signs in by
// your Tailscale login (an auth-refresh, which never sends one) and, in
// setup, by impersonation, so in practice nothing changes for it. The /_/
// superuser's emails are left on.
func ensureLoginAlertsOff(ctx context.Context, b *beszel, suToken string, out io.Writer) error {
	var coll struct {
		AuthAlert map[string]any `json:"authAlert"`
	}
	if err := b.call(ctx, http.MethodGet, "/api/collections/users", suToken, nil, &coll); err != nil {
		return fmt.Errorf("logins: reading the users collection: %w", err)
	}
	// A missing or non-bool field means this PocketBase names the setting
	// differently, and "off" would then be a guess.
	on, ok := coll.AuthAlert["enabled"].(bool)
	if !ok {
		return fmt.Errorf("logins: the users collection has no authAlert.enabled setting; this Beszel version is not one setup knows")
	}
	if on {
		// The whole object, read back with one field changed, so the email
		// template is left as it was.
		coll.AuthAlert["enabled"] = false
		var updated struct {
			AuthAlert struct {
				Enabled *bool `json:"enabled"`
			} `json:"authAlert"`
		}
		if err := b.call(ctx, http.MethodPatch, "/api/collections/users", suToken,
			map[string]any{"authAlert": coll.AuthAlert}, &updated); err != nil {
			return fmt.Errorf("logins: turning off new-login emails: %w", err)
		}
		if e := updated.AuthAlert.Enabled; e == nil || *e {
			return fmt.Errorf("logins: the hub accepted the change but still has new-login emails on")
		}
		log.Printf("logins: new-login emails turned off for dashboard accounts (the reporter's bounced); /_/ keeps them")
	} else {
		log.Printf("logins: new-login emails already off for dashboard accounts")
	}
	fmt.Fprintln(out, "MON_HUB_LOGIN_ALERTS=off")
	return nil
}

// ensureSystem makes the system called name exist at host:port.
func ensureSystem(ctx context.Context, b *beszel, user authResult, name, host, port string) (string, error) {
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Host string `json:"host"`
			Port string `json:"port"`
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
			"name": name, "host": host, "port": port, "users": []string{user.Record.ID},
		}, &created)
		if err != nil {
			return "", fmt.Errorf("adding system %s: %w", name, err)
		}
		log.Printf("system: added %s on %s", name, host)
		return created.ID, nil
	}
	s := list.Items[0]
	if s.Host != host || s.Port != port {
		// status "pending" is what makes the hub reconnect: it copies host and
		// port into its connection only on create or on a move to pending
		// (internal/hub/systems/system_manager.go). Beszel's own edit dialog
		// sends it for the same reason.
		if err := b.call(ctx, http.MethodPatch, "/api/collections/systems/records/"+s.ID, user.Token,
			map[string]any{"host": host, "port": port, "status": "pending"}, nil); err != nil {
			return "", fmt.Errorf("correcting %s's host: %w", name, err)
		}
		log.Printf("system: %s pointed at %s (was %q), reconnecting", name, host, s.Host)
	} else {
		log.Printf("system: %s already present", name)
	}
	return s.ID, nil
}

// ensureReporter keeps the current reporter credentials if they work and are
// readonly, and otherwise creates the account, or resets one that is ALREADY
// readonly, with a new random password. It never changes the role of an
// account: an account at that address with any other role is a conflict for
// you to resolve, not something to demote.
func ensureReporter(ctx context.Context, b *beszel, suToken string, c setupConfig, out io.Writer) error {
	if ok, err := reporterWorks(ctx, b, c); err != nil || ok {
		if ok {
			log.Printf("reporter: %s already works and is readonly", c.ReporterEmail)
		}
		return err
	}
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"items"`
	}
	q := url.Values{"filter": {"email = " + pbQuote(c.ReporterEmail)}}
	if err := b.call(ctx, http.MethodGet, "/api/collections/users/records?"+q.Encode(), suToken, nil, &list); err != nil {
		return fmt.Errorf("looking up the reporter: %w", err)
	}
	if len(list.Items) > 0 && list.Items[0].Role != "readonly" {
		return fmt.Errorf("reporter: an account %s already exists with role %q; setup will not demote it - give the reporter its own address", c.ReporterEmail, list.Items[0].Role)
	}
	pass, err := randomPassword()
	if err != nil {
		return err
	}
	fields := map[string]any{
		"email": c.ReporterEmail, "password": pass, "passwordConfirm": pass,
		"role": "readonly", "verified": true,
	}
	if len(list.Items) == 0 {
		err = b.call(ctx, http.MethodPost, "/api/collections/users/records", suToken, fields, nil)
		log.Printf("reporter: created %s (readonly)", c.ReporterEmail)
	} else {
		err = b.call(ctx, http.MethodPatch, "/api/collections/users/records/"+list.Items[0].ID, suToken, fields, nil)
		log.Printf("reporter: reset %s's password (already readonly)", c.ReporterEmail)
	}
	if err != nil {
		return fmt.Errorf("writing the reporter account: %w", err)
	}
	fmt.Fprintf(out, "REPORT_BESZEL_EMAIL=%s\nREPORT_BESZEL_PASSWORD=%s\n", c.ReporterEmail, pass)
	return nil
}

// reporterWorks says whether REPORT_BESZEL_* signs in. An account that signs
// in with any role but readonly is an error, never something to fix.
func reporterWorks(ctx context.Context, b *beszel, c setupConfig) (bool, error) {
	if c.ReporterPass == "" {
		return false, nil
	}
	r, err := b.authWith(ctx, "users", c.ReporterEmail, c.ReporterPass)
	if err != nil {
		return false, nil
	}
	if r.Record.Role != "readonly" {
		return false, fmt.Errorf("reporter: %s signs in but has role %q, not readonly - give the reporter its own address", c.ReporterEmail, r.Record.Role)
	}
	return true, nil
}

// ensureSMTP applies the hub's mail settings when a password was typed. When
// none was, it leaves them alone but says loudly if mail is OFF: on a restore
// the hub's SMTP password is gone with its database, and alerts from a hub with
// no mail go nowhere, silently.
func ensureSMTP(ctx context.Context, b *beszel, suToken string, c setupConfig) error {
	if c.HubSMTPPass == "" {
		var cur struct {
			SMTP struct {
				Enabled bool `json:"enabled"`
			} `json:"smtp"`
		}
		if err := b.call(ctx, http.MethodGet, "/api/settings", suToken, nil, &cur); err != nil {
			return fmt.Errorf("smtp: reading settings: %w", err)
		}
		if cur.SMTP.Enabled {
			log.Printf("smtp: no hub password given, mail settings left as they are (enabled)")
		} else {
			log.Printf("smtp: WARNING - the hub's mail is OFF, so alert emails will not send. Run setup again and give the hub's SMTP app password")
		}
		return nil
	}
	port, _ := strconv.Atoi(c.SMTPPort)  // validated in validate()
	from, _ := mail.ParseAddress(c.From) // likewise
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
	log.Printf("smtp: hub mail set to %s:%d as %s (password applied)", c.SMTPHost, port, c.SMTPUser)
	return nil
}

type userSettingsRecord struct {
	ID       string         `json:"id"`
	Settings map[string]any `json:"settings"`
}

// ensureNotify sets the addresses your alert emails go to, keeping every other
// UI preference in the same record (chart time, units, webhooks).
//
// A new record is created EMPTY and then updated, never created with the
// addresses in it: the hub's create hook (InitializeUserSettings,
// internal/users/users.go) overwrites "emails" with the account's own address,
// so addresses sent on create are silently dropped.
func ensureNotify(ctx context.Context, b *beszel, user authResult, to []string) error {
	if len(to) == 0 {
		log.Printf("notify: REPORT_TO is blank, alert addresses left as they are")
		return nil
	}
	var list struct {
		Items []userSettingsRecord `json:"items"`
	}
	q := url.Values{"filter": {"user = " + pbQuote(user.Record.ID)}}
	if err := b.call(ctx, http.MethodGet, "/api/collections/user_settings/records?"+q.Encode(), user.Token, nil, &list); err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	var rec userSettingsRecord
	if len(list.Items) == 0 {
		if err := b.call(ctx, http.MethodPost, "/api/collections/user_settings/records", user.Token,
			map[string]any{"user": user.Record.ID}, &rec); err != nil {
			return fmt.Errorf("notify: %w", err)
		}
	} else {
		rec = list.Items[0]
	}
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
func ensureAlerts(ctx context.Context, b *beszel, user authResult, name, sysID string, specs []alertSpec) error {
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
			return fmt.Errorf("alerts: setting %s on %s: %w", s.Name, name, err)
		}
		changed++
		log.Printf("alerts: %s: %s set to %g for %d min", name, s.Name, s.Value, s.Min)
	}
	for a := range have {
		if want[a] {
			continue
		}
		if err := b.call(ctx, http.MethodDelete, "/api/beszel/user-alerts", user.Token, map[string]any{
			"name": a, "systems": []string{sysID},
		}, nil); err != nil {
			return fmt.Errorf("alerts: removing %s from %s: %w", a, name, err)
		}
		changed++
		log.Printf("alerts: %s: removed %s (not in alerts.json)", name, a)
	}
	if changed == 0 {
		log.Printf("alerts: %s: all %d already match alerts.json", name, len(specs))
	}
	return nil
}
