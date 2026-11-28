package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// `daily-brief publish`: one tick of the email job, run by supercronic every
// few minutes in the brief-publisher container.
//
// For each recent day it sends one email: the brief once the issue is
// published, or a short notice when the writer gave up or the issue fails the
// validator. With a show configured, the brief waits for the podcast job
// (podcast.go) up to PODCAST_WAIT after the issue was published, and links the
// episode or says why there is none. It reads the podcast job's progress from
// /podcast/days/ and has no other contact with Spotify.
//
// AT MOST ONE EMAIL A DAY. The send is recorded as started before Gmail is
// called, and nothing is sent if that cannot be written. A send that fails
// before Gmail could have accepted it (no network, a refused token, an error
// status from Gmail) clears the record and is retried next tick. Any other
// failure (a timeout or reset mid-request, or a disk too full to record the
// message id) leaves the send's outcome unknown, and an unknown send is never
// repeated: the tick logs it every time instead and does not ping
// HC_PING_URL, so the dead-man check raises it. A crash in that window skips
// the day's email rather than sending it twice.

type publisher struct {
	cfg    *config
	mailer Mailer
	http   *http.Client
	now    func() time.Time
	log    *log.Logger
}

// writeState is writeFileAtomic, as a variable so the tests can make a save
// fail, which is the case the at-most-once rules are for.
var writeState = writeFileAtomic

// dayState is one day's email.
type dayState struct {
	Date string `json:"date"`

	// SendStartedAt is set before Gmail is called. With Emailed still false
	// on a later tick, the send's outcome is unknown.
	SendStartedAt time.Time `json:"send_started_at,omitzero"`
	Kind          string    `json:"kind,omitempty"` // "brief" or "notice"
	Emailed       bool      `json:"emailed,omitempty"`
	EmailedAt     time.Time `json:"emailed_at,omitzero"`
	MessageID     string    `json:"message_id,omitempty"`
}

func (p *publisher) statePath(date string) string {
	return filepath.Join(p.cfg.stateDir, "issues", date+".json")
}

func (p *publisher) loadState(date string) (*dayState, error) {
	raw, err := os.ReadFile(p.statePath(date))
	if errors.Is(err, os.ErrNotExist) {
		return &dayState{Date: date}, nil
	}
	if err != nil {
		return nil, err
	}
	var st dayState
	if err := json.Unmarshal(raw, &st); err != nil || st.Date != date {
		return nil, fmt.Errorf("state file for %s is damaged; move it aside to start that day over", date)
	}
	return &st, nil
}

func (p *publisher) saveState(st *dayState) error {
	body, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeState(p.statePath(st.Date), append(body, '\n'), 0o600)
}

// Tick runs one pass. It holds an exclusive lock on /state for its whole run,
// so a slow tick and the next one never both act on the same day.
func (p *publisher) Tick(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(p.cfg.stateDir, "issues"), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(p.cfg.stateDir, ".publish.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		p.log.Print("another publish is still running; skipping this tick")
		return nil
	}

	var errs []error
	today := p.now()
	for i := p.cfg.lookbackDays - 1; i >= 0; i-- {
		date := today.AddDate(0, 0, -i).Format(time.DateOnly)
		if err := p.day(ctx, date); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", date, err))
		}
	}
	return errors.Join(errs...)
}

func (p *publisher) day(ctx context.Context, date string) error {
	st, err := p.loadState(date)
	if err != nil {
		return err
	}
	if st.Emailed {
		return nil
	}
	if !st.SendStartedAt.IsZero() {
		return fmt.Errorf("the %s was handed to Gmail at %s and Gmail never confirmed it, so it may have gone out. "+
			"It is not sent again. Check the Sent folder; to send it anyway, move %s aside",
			st.Kind, st.SendStartedAt.Format(time.RFC3339), p.statePath(date))
	}

	dir := filepath.Join(p.cfg.issuesDir, "published", date)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil // not written yet
	}
	if reason, ok := readFailed(filepath.Join(dir, "failed.txt")); ok {
		return p.notice(ctx, st, reason)
	}
	script := filepath.Join(dir, "script.json")
	is, err := LoadIssue(script, date)
	if err != nil {
		return p.notice(ctx, st, "the publisher refused the issue: "+err.Error())
	}
	fi, err := os.Stat(script)
	if err != nil {
		return err
	}

	pod, wait := p.podcastLine(date, fi.ModTime())
	if wait {
		return nil
	}
	when, _ := time.ParseInLocation(time.DateOnly, date, time.Local)
	raw, err := buildBrief(p.cfg.from, p.cfg.to, is, when, pod, p.now())
	if err != nil {
		return err
	}
	return p.send(ctx, st, raw, "brief")
}

// podcastLine says what the email says about the episode, or that the email
// should wait for it.
func (p *publisher) podcastLine(date string, publishedAt time.Time) (Podcast, bool) {
	if !p.cfg.podcasts {
		return Podcast{}, false
	}
	pd, err := p.readPodcast(date)
	if err != nil {
		p.log.Printf("%s: the podcast job's record is not usable, so the email goes without it: %v", date, err)
		return Podcast{Note: "No podcast today: the podcast job left a record the publisher could not use."}, false
	}
	if pd == nil {
		pd = &podDay{}
	}
	link := "https://open.spotify.com/episode/" + episodeID(pd.EpisodeURI)
	switch {
	case pd.Ready:
		return Podcast{URL: link}, false
	case pd.GaveUp == gaveUpUpload:
		return Podcast{Note: "No podcast today: the upload to Spotify failed."}, false
	case pd.GaveUp == gaveUpVoice:
		return Podcast{Note: "No podcast today: the voice render failed."}, false
	case pd.GaveUp != "":
		return Podcast{Note: "No podcast today."}, false
	case pd.EpisodeURI != "" && pd.ReadyChecks >= maxReadyChecks:
		return Podcast{URL: link}, false
	case p.now().Sub(publishedAt) < p.cfg.podcastWait:
		return Podcast{}, true
	case pd.EpisodeURI != "":
		return Podcast{URL: link}, false
	default:
		return Podcast{Note: "No podcast yet: it was not ready in time, and it will be on the show when it is."}, false
	}
}

func (p *publisher) readPodcast(date string) (*podDay, error) {
	days, err := os.OpenRoot(p.cfg.podcastDays)
	if err != nil {
		return nil, err
	}
	defer days.Close()
	return readPodDay(days, date)
}

func (p *publisher) notice(ctx context.Context, st *dayState, reason string) error {
	raw, err := buildNotice(p.cfg.from, p.cfg.to, st.Date, reason, p.now())
	if err != nil {
		return err
	}
	return p.send(ctx, st, raw, "notice")
}

func (p *publisher) send(ctx context.Context, st *dayState, raw []byte, kind string) error {
	st.SendStartedAt, st.Kind = p.now(), kind
	if err := p.saveState(st); err != nil {
		return fmt.Errorf("not sending the %s, because the attempt could not be recorded: %w", kind, err)
	}

	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	id, err := p.mailer.Send(sctx, raw)
	if err != nil && definitelyNotSent(err) {
		st.SendStartedAt, st.Kind = time.Time{}, ""
		if serr := p.saveState(st); serr != nil {
			return fmt.Errorf("send: %w; and the retry could not be recorded, so it will not be retried: %v", err, serr)
		}
		return fmt.Errorf("send (retried next tick): %w", err)
	}
	if err != nil {
		return fmt.Errorf("send: Gmail may or may not have sent the %s, so it is not sent again: %w", kind, err)
	}

	st.Emailed, st.EmailedAt, st.MessageID = true, p.now(), id
	if err := p.saveState(st); err != nil {
		return fmt.Errorf("sent message %s but could not record it; it is not sent again: %w", id, err)
	}
	p.log.Printf("%s: sent the %s (message %s)", st.Date, kind, id)
	p.ping(ctx, kind == "notice")
	return nil
}

// ping tells a dead-man's-switch monitor that a day's email went out, so a
// day with no email at all (the NAS down, the stack stopped, a send whose
// outcome is unknown) raises an alert somewhere other than the inbox it would
// have arrived in.
func (p *publisher) ping(ctx context.Context, failed bool) {
	if p.cfg.hcPingURL == "" {
		return
	}
	u := p.cfg.hcPingURL
	if failed {
		u = strings.TrimSuffix(u, "/") + "/fail"
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, u, nil)
	if err != nil {
		return
	}
	resp, err := p.http.Do(req)
	if err != nil {
		p.log.Printf("health check ping failed: %v", err)
		return
	}
	resp.Body.Close()
}

// readFailed reads a failed.txt, capped. ok is false when there is none.
func readFailed(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 2048))
	return string(b), true
}
