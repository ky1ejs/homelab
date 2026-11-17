// homelab-report: a weekly email for each machine the hub monitors -- the NAS,
// and the mac studio once it is added -- saying how it has been doing, and
// whether any of it says to upgrade or change approach.
//
// It reads Beszel's hub over the compose network as a readonly user, and mails
// a summary over SMTP, as HTML with a plain-text fallback. It serves nothing,
// holds no socket and mounts nothing but /etc/localtime. Beszel is the
// dashboard and the alerting; this is only the part Beszel does not do, which
// is a digest you did not have to go and look at. See README.md.
//
// Go rather than a shell script in a cron container, for the same reason as
// dashboard/: the work is parsing JSON and doing arithmetic on it, which is the
// case DECISIONS.md#runtime-node-not-bun-or-go says a compiler earns its keep
// on. Same conventions as the other Go stacks -- stdlib only, -version, tests
// in the Dockerfile.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Version is stamped by the Dockerfile with -ldflags. "dev" outside a build.
var Version = "dev"

type config struct {
	BeszelURL    string
	Email        string
	Password     string
	System       string // the NAS's name on the hub
	Only         string // with -dry-run or -once: one system's report alone
	Schedule     schedule
	DashboardURL string
	PingURL      string
	Mail         mailConfig
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	sched, err := parseSchedule(env("REPORT_SCHEDULE", "Mon 08:00"))
	if err != nil {
		return config{}, err
	}
	var to []string
	for _, a := range strings.Split(env("REPORT_TO", ""), ",") {
		if a = strings.TrimSpace(a); a != "" {
			to = append(to, a)
		}
	}
	c := config{
		BeszelURL:    env("BESZEL_URL", "http://beszel:8090"),
		Email:        env("REPORT_BESZEL_EMAIL", ""),
		Password:     os.Getenv("REPORT_BESZEL_PASSWORD"),
		System:       env("REPORT_SYSTEM", "kyles-nas"),
		Schedule:     sched,
		DashboardURL: env("REPORT_DASHBOARD_URL", ""),
		PingURL:      env("REPORT_PING_URL", ""),
		Mail: mailConfig{
			Host:     env("SMTP_HOST", ""),
			Port:     env("SMTP_PORT", "587"),
			Username: env("SMTP_USERNAME", ""),
			Password: os.Getenv("SMTP_PASSWORD"),
			From:     env("REPORT_FROM", ""),
			To:       to,
		},
	}
	if c.Email == "" || c.Password == "" {
		return config{}, fmt.Errorf("REPORT_BESZEL_EMAIL and REPORT_BESZEL_PASSWORD must be set (README.md#the-reporters-account)")
	}
	return c, nil
}

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		once        = flag.Bool("once", false, "build and send one report now, then exit")
		dryRun      = flag.Bool("dry-run", false, "build one report now and print it instead of sending it")
		asHTML      = flag.Bool("html", false, "with -dry-run, print the HTML part instead of the plain text")
		setup       = flag.Bool("setup", false, "configure the hub from this repo (run by `homelab setup monitoring`; see setup.go)")
		only        = flag.String("only", "", "with -dry-run or -once, only the report for this system (kyles-nas, mac-studio)")
	)
	flag.Parse()
	if *asHTML && !*dryRun {
		log.Fatal("-html only works with -dry-run")
	}
	if *only != "" && !*dryRun && !*once {
		log.Fatal("-only works with -dry-run or -once")
	}
	if *showVersion {
		fmt.Println(Version)
		return
	}
	if *setup {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		sc, err := loadSetupConfig(os.Stdin)
		if err == nil {
			err = runSetup(ctx, sc, os.Stdout)
		}
		if err != nil {
			// bin/homelab reads this line, asks for your password and
			// runs setup again.
			if errors.As(err, new(needsPassword)) {
				fmt.Println("SETUP_NEEDS_PASSWORD=1")
			}
			log.Fatal(err)
		}
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if *only != "" {
		known := false
		for _, h := range hosts(cfg.System) {
			known = known || h.System == *only
		}
		if !known {
			log.Fatalf("-only %q: not a system this report covers", *only)
		}
		cfg.Only = *only
	}
	// A dry run needs no mail settings: it is how you check the numbers
	// before you have set up an app password.
	if !*dryRun {
		if err := cfg.Mail.validate(); err != nil {
			log.Fatal(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *dryRun || *once {
		outs, err := build(ctx, cfg, time.Now(), reportHosts(cfg))
		if err != nil {
			log.Fatal(err)
		}
		ms, failed := split(outs)
		if *dryRun && *asHTML {
			if len(failed) > 0 {
				log.Fatal(strings.Join(failed, "; "))
			}
			if len(ms) != 1 {
				log.Fatalf("-html prints one report and there are %d: add -only", len(ms))
			}
			if ms[0].HTML == "" {
				log.Fatal("the HTML part did not render; see the log line above")
			}
			fmt.Print(ms[0].HTML)
			return
		}
		for i, m := range ms {
			if *dryRun {
				if i > 0 {
					fmt.Print("\n" + strings.Repeat("-", 72) + "\n\n")
				}
				fmt.Printf("Subject: %s\n\n%s", m.Subject, m.Text)
				continue
			}
			// Every email is tried before any failure stops the run, so
			// a rerun with -only resends only the one that did not go.
			if err := deliver(cfg, m); err != nil {
				failed = append(failed, fmt.Sprintf("sending %q: %v", m.Subject, err))
				continue
			}
			log.Printf("sent %q to %s", m.Subject, strings.Join(cfg.Mail.To, ", "))
		}
		if len(failed) > 0 {
			log.Fatalf("%s (rerun the failed one with -only <system>)", strings.Join(failed, "; "))
		}
		return
	}

	// A slot the container is down for (a reboot, a redeploy at 08:00) is
	// SKIPPED, not caught up: nothing records the last run, and catching up
	// on start would double-send after any restart shortly after a report.
	// REPORT_PING_URL is what turns a skipped week into an alert.
	var names []string
	for _, h := range hosts(cfg.System) {
		names = append(names, h.System)
	}
	log.Printf("homelab-report %s: weekly at %s %02d:%02d %s, for %s (each the hub has)",
		Version, cfg.Schedule.Weekday, cfg.Schedule.Hour, cfg.Schedule.Minute, time.Local, strings.Join(names, " and "))
	for {
		at := cfg.Schedule.next(time.Now())
		log.Printf("next report %s", at.Format(time.RFC1123))
		if !sleepUntil(ctx, at) {
			return
		}
		runScheduled(ctx, cfg, at, mailer{
			send:  func(m email) error { return deliver(cfg, m) },
			ping:  func() { ping(cfg.PingURL) },
			pause: func(ctx context.Context) bool { return sleepUntil(ctx, time.Now().Add(10*time.Minute)) },
		})
	}
}

// sleepUntil waits for the wall-clock time t, waking at least hourly so a host
// suspend or clock step is noticed rather than slept through. Returns false if
// the context was cancelled.
func sleepUntil(ctx context.Context, t time.Time) bool {
	for {
		d := time.Until(t)
		if d <= 0 {
			return true
		}
		timer := time.NewTimer(min(d, time.Hour))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// mailer is how a scheduled run sends, pings and waits between attempts;
// tests stand in for all three.
type mailer struct {
	send  func(email) error
	ping  func()
	pause func(context.Context) bool
}

// runScheduled builds and sends the week's reports, retrying a failed build:
// the hub restarting during a deploy is the likeliest failure and it fixes
// itself. Each host is built and sent on its own, so one host failing costs
// no other its email, and a retry rebuilds only the hosts still failing, so
// none goes out twice. A host that fails every attempt gets a failure email
// under its own subject, once.
func runScheduled(ctx context.Context, cfg config, at time.Time, mail mailer) {
	const attempts = 3
	pending := reportHosts(cfg)
	errs := map[string]error{}
	sent := true
	for i := 1; i <= attempts && len(pending) > 0; i++ {
		if i > 1 && !mail.pause(ctx) {
			return
		}
		outs, err := build(ctx, cfg, at, pending)
		if ctx.Err() != nil {
			// Shut down mid-build: say nothing. A "report failed: context
			// canceled" email for a redeploy that happened to land on
			// Monday morning is noise.
			return
		}
		if err != nil {
			// The hub refused the login, so no host could be built.
			log.Printf("build attempt %d/%d: %v", i, attempts, err)
			for _, h := range pending {
				// Keep a host's own failure from an earlier attempt: it says
				// more than the hub being away now.
				if _, ok := errs[h.System]; !ok {
					errs[h.System] = err
				}
			}
			continue
		}
		pending = nil
		for _, o := range outs {
			if o.err != nil {
				log.Printf("build attempt %d/%d: %v", i, attempts, o.err)
				errs[o.h.System] = o.err
				pending = append(pending, o.h)
				continue
			}
			delete(errs, o.h.System)
			sent = deliverOne(mail, o.m) && sent
		}
	}
	if ctx.Err() != nil {
		return
	}
	for _, h := range pending {
		err := errs[h.System]
		var he *hostError
		if errors.As(err, &he) {
			// failureReport names the host itself.
			err = he.err
		} else if h.Optional {
			// The hub never answered, so it cannot say whether it has an
			// optional host: the NAS's failure email covers the hub, alone.
			continue
		}
		var m email
		m.Subject, m.Text = failureReport(h, err, cfg.DashboardURL)
		deliverOne(mail, m)
	}
	// Only a week whose reports were all built AND sent counts. A failure
	// email reaching you is not a healthy week, so it does not ping.
	if len(pending) == 0 && sent && cfg.PingURL != "" {
		mail.ping()
	}
}

func deliverOne(mail mailer, m email) bool {
	if err := mail.send(m); err != nil {
		// No retry loop around the mail itself: if SMTP is down the next
		// attempt is next week, and REPORT_PING_URL is what notices.
		log.Printf("send failed for %q: %v", m.Subject, err)
		return false
	}
	log.Printf("sent %q", m.Subject)
	return true
}

// reportHosts is every host, or the one -only names.
func reportHosts(cfg config) []host {
	var out []host
	for _, h := range hosts(cfg.System) {
		if cfg.Only == "" || h.System == cfg.Only {
			out = append(out, h)
		}
	}
	return out
}

// outcome is one host's report, or why it could not be built.
type outcome struct {
	h   host
	m   email
	err error
}

// hostError is a failure building one host's report, as opposed to the hub
// refusing the login, which fails every host.
type hostError struct {
	system string
	err    error
}

func (e *hostError) Error() string { return e.system + ": " + e.err.Error() }
func (e *hostError) Unwrap() error { return e.err }

// split is the emails that built and a line for each host that did not.
func split(outs []outcome) ([]email, []string) {
	var ms []email
	var failed []string
	for _, o := range outs {
		if o.err != nil {
			failed = append(failed, o.err.Error())
			continue
		}
		ms = append(ms, o.m)
	}
	return ms, failed
}

// build makes one email for each of hs that the hub has, in order. err means
// the hub refused the login and no host was built; a failure on one host is
// in its outcome. An optional host the hub does not have is left out, unless
// -only named it: then its absence is the answer, and an error.
func build(ctx context.Context, cfg config, to time.Time, hs []host) ([]outcome, error) {
	b := newBeszel(cfg.BeszelURL, cfg.Email, cfg.Password)
	if err := b.login(ctx); err != nil {
		return nil, err
	}
	var out []outcome
	for _, h := range hs {
		sys, err := b.system(ctx, h.System)
		if errors.Is(err, errNoSystem) && h.Optional && cfg.Only == "" {
			log.Printf("%s is not on the hub, so no report for it", h.System)
			continue
		}
		var m email
		if err == nil {
			m, err = buildOne(ctx, b, cfg, h, sys, to)
		}
		if err != nil {
			err = &hostError{h.System, err}
		}
		out = append(out, outcome{h: h, m: m, err: err})
	}
	return out, nil
}

func buildOne(ctx context.Context, b *beszel, cfg config, h host, sys systemRecord, to time.Time) (email, error) {
	// A calendar week back, not 168 hours: next() keeps local wall-clock time,
	// so in a DST week this keeps each report starting where the last ended.
	from := to.AddDate(0, 0, -7)
	// Every query is bounded above by `to` as well as below. A retry runs ten
	// minutes late and the hub writes records on its own schedule, so without
	// the upper bound a report could count samples from after its own window.
	week, err := b.systemStats(ctx, sys.ID, "120m", from, to)
	if err != nil {
		return email{}, err
	}
	month, err := b.systemStats(ctx, sys.ID, "480m", to.AddDate(0, 0, -30), to)
	if err != nil {
		return email{}, err
	}
	ctrs, err := b.containerStats(ctx, sys.ID, "120m", from, to)
	if err != nil {
		return email{}, err
	}
	s := summarize(sys, week, month, ctrs, from, to)
	var m email
	m.Subject, m.Text, m.HTML, err = render(s, h, evaluate(s, h, time.Local), cfg.DashboardURL, time.Local)
	if err != nil {
		// A bug in report.html, not bad data. The text part is the whole
		// report, so send that rather than nothing.
		log.Printf("%v; sending the text part only", err)
	}
	return m, nil
}

func deliver(cfg config, m email) error {
	msg, err := buildMessage(cfg.Mail, m, time.Now())
	if err != nil {
		return err
	}
	return sendMail(cfg.Mail, msg)
}

// ping tells an external dead-man's switch (Healthchecks.io or similar) that a
// report went out. Best effort: a failed ping is exactly what that service is
// there to notice.
func ping(url string) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("ping: %v", err)
		return
	}
	resp.Body.Close()
}
