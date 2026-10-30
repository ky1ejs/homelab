// homelab-report: a weekly email saying how the NAS has been doing, and
// whether any of it says to upgrade or change approach.
//
// It reads Beszel's hub over the compose network as a readonly user, and mails
// a plain-text summary over SMTP. It serves nothing, holds no socket and mounts
// nothing but /etc/localtime. Beszel is the dashboard and the alerting; this is
// only the part Beszel does not do, which is a digest you did not have to go
// and look at. See README.md.
//
// Go rather than a shell script in a cron container, for the same reason as
// dashboard/: the work is parsing JSON and doing arithmetic on it, which is the
// case DECISIONS.md#runtime-node-not-bun-or-go says a compiler earns its keep
// on. Same conventions as the other Go stacks -- stdlib only, -version, tests
// in the Dockerfile.
package main

import (
	"context"
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
	System       string
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
		setup       = flag.Bool("setup", false, "configure the hub from this repo (run by `homelab setup monitoring`; see setup.go)")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(Version)
		return
	}
	if *setup {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		sc, err := loadSetupConfig(os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		if err := runSetup(ctx, sc, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
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
		subject, body, err := build(ctx, cfg, time.Now())
		if err != nil {
			log.Fatal(err)
		}
		if *dryRun {
			fmt.Printf("Subject: %s\n\n%s", subject, body)
			return
		}
		if err := deliver(cfg, subject, body); err != nil {
			log.Fatal(err)
		}
		log.Printf("sent %q to %s", subject, strings.Join(cfg.Mail.To, ", "))
		return
	}

	// A slot the container is down for (a reboot, a redeploy at 08:00) is
	// SKIPPED, not caught up: nothing records the last run, and catching up
	// on start would double-send after any restart shortly after a report.
	// REPORT_PING_URL is what turns a skipped week into an alert.
	log.Printf("homelab-report %s: weekly at %s %02d:%02d %s, for %s",
		Version, cfg.Schedule.Weekday, cfg.Schedule.Hour, cfg.Schedule.Minute, time.Local, cfg.System)
	for {
		at := cfg.Schedule.next(time.Now())
		log.Printf("next report %s", at.Format(time.RFC1123))
		if !sleepUntil(ctx, at) {
			return
		}
		runScheduled(ctx, cfg, at)
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

// runScheduled builds and sends one report, retrying a failed build: the hub
// restarting during a deploy is the likeliest failure and it fixes itself. If
// every attempt fails, the failure itself is mailed.
func runScheduled(ctx context.Context, cfg config, at time.Time) {
	const attempts = 3
	var subject, body string
	var err error
	for i := 1; i <= attempts; i++ {
		subject, body, err = build(ctx, cfg, at)
		if err == nil {
			break
		}
		log.Printf("build attempt %d/%d: %v", i, attempts, err)
		if i < attempts && !sleepUntil(ctx, time.Now().Add(10*time.Minute)) {
			return
		}
	}
	// Shut down mid-build: say nothing. A "report failed: context canceled"
	// email for a redeploy that happened to land on Monday morning is noise.
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		subject, body = failureReport(cfg.System, err, cfg.DashboardURL)
	}
	if err := deliver(cfg, subject, body); err != nil {
		// No retry loop around the mail itself: if SMTP is down the next
		// attempt is next week, and REPORT_PING_URL is what notices.
		log.Printf("send failed: %v", err)
		return
	}
	log.Printf("sent %q", subject)
	// Only a report that was built AND sent counts. A failure email reaching
	// you is not a healthy week, so it does not ping.
	if err == nil && cfg.PingURL != "" {
		ping(cfg.PingURL)
	}
}

func build(ctx context.Context, cfg config, to time.Time) (string, string, error) {
	// A calendar week back, not 168 hours: next() keeps local wall-clock time,
	// so in a DST week this keeps each report starting where the last ended.
	from := to.AddDate(0, 0, -7)
	b := newBeszel(cfg.BeszelURL, cfg.Email, cfg.Password)
	if err := b.login(ctx); err != nil {
		return "", "", err
	}
	sys, err := b.system(ctx, cfg.System)
	if err != nil {
		return "", "", err
	}
	// Every query is bounded above by `to` as well as below. A retry runs ten
	// minutes late and the hub writes records on its own schedule, so without
	// the upper bound a report could count samples from after its own window.
	week, err := b.systemStats(ctx, sys.ID, "120m", from, to)
	if err != nil {
		return "", "", err
	}
	month, err := b.systemStats(ctx, sys.ID, "480m", to.AddDate(0, 0, -30), to)
	if err != nil {
		return "", "", err
	}
	ctrs, err := b.containerStats(ctx, sys.ID, "120m", from, to)
	if err != nil {
		return "", "", err
	}
	s := summarize(sys, week, month, ctrs, from, to)
	subject, body := render(s, evaluate(s), cfg.DashboardURL, time.Local)
	return subject, body, nil
}

func deliver(cfg config, subject, body string) error {
	msg, err := buildMessage(cfg.Mail, subject, body, time.Now())
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
