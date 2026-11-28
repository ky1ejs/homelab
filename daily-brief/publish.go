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
	"sort"
	"strings"
	"syscall"
	"time"
)

// `daily-brief publish`: one tick of the email-and-upload job, run by
// supercronic every few minutes in the brief-publisher container.
//
// For each recent day it moves that day through, at most once each:
//
//  1. a published issue exists (written by brief-agent);
//  2. its audio exists (written by brief-voice), or the voice job gave up;
//  3. the episode is uploaded to the show and has finished processing, and
//     its chapters and links are set;
//  4. the email is sent, linking the episode when there is one.
//
// The email waits for the podcast up to PODCAST_WAIT after the issue was
// published, then goes without it and says why. An episode that turns up
// after the email went out is still uploaded, so the show stays complete.
//
// Every step's outcome is written to /state/issues/<date>.json before the next
// step starts, so a crash, a restart or a deploy mid-tick repeats nothing that
// reaches the outside: no second email, no second upload. The cost of that
// rule is that a crash between Gmail accepting a send and the state file
// recording it would skip that day's email rather than send it twice.

type publisher struct {
	cfg     *config
	spotify Spotify // nil when no show is configured
	mailer  Mailer
	http    *http.Client
	now     func() time.Time
	log     *log.Logger
}

// dayState is one day's progress. Fields are only ever set, never cleared,
// apart from the counters.
type dayState struct {
	Date string `json:"date"`

	EpisodeURI     string    `json:"episode_uri,omitempty"`
	UploadedAt     time.Time `json:"uploaded_at,omitzero"`
	UploadAttempts int       `json:"upload_attempts,omitempty"`
	UploadError    string    `json:"upload_error,omitempty"`
	Ready          bool      `json:"ready,omitempty"`
	ReadyChecks    int       `json:"ready_checks,omitempty"`
	TimelineSet    bool      `json:"timeline_set,omitempty"`
	TimelineError  string    `json:"timeline_error,omitempty"`
	GaveUpPodcast  bool      `json:"gave_up_podcast,omitempty"`

	Emailed   bool      `json:"emailed,omitempty"`
	EmailedAt time.Time `json:"emailed_at,omitzero"`
	Kind      string    `json:"kind,omitempty"` // "brief" or "notice"
	MessageID string    `json:"message_id,omitempty"`

	Deleted bool `json:"deleted,omitempty"`
}

// done reports that nothing is left to do for the day.
func (st *dayState) done(podcasts bool) bool {
	switch {
	case !st.Emailed:
		return false
	case st.Kind == "notice", !podcasts, st.GaveUpPodcast:
		return true
	case st.EpisodeURI == "":
		return false // the email went without it; upload it when it comes
	case !st.Ready:
		return st.ReadyChecks >= maxReadyChecks
	default:
		return st.TimelineSet || st.TimelineError != ""
	}
}

const (
	maxUploadAttempts = 3
	// Each check blocks for readyWait; together they cover processing that
	// Spotify's docs put at "a few minutes" with plenty to spare.
	maxReadyChecks = 6
	readyWait      = 10 * time.Minute
)

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
	return writeFileAtomic(p.statePath(st.Date), append(body, '\n'), 0o600)
}

// Tick runs one pass. It holds an exclusive lock on /state for its whole run,
// so a slow tick (a readiness wait can take ten minutes) and the next one never
// both act on the same day.
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
	if err := p.retention(ctx); err != nil {
		errs = append(errs, fmt.Errorf("retention: %w", err))
	}
	return errors.Join(errs...)
}

func (p *publisher) day(ctx context.Context, date string) error {
	st, err := p.loadState(date)
	if err != nil {
		return err
	}
	if st.done(p.spotify != nil) {
		return nil
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
	publishedAt := fi.ModTime()

	if p.spotify != nil {
		p.podcast(ctx, st, is)
	}
	if st.Emailed {
		return nil
	}

	pod, wait := p.podcastLine(st, publishedAt)
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

// podcast advances the upload as far as it can this tick. Its failures are
// recorded on the state, not returned: the email must not wait on Spotify
// beyond PODCAST_WAIT.
func (p *publisher) podcast(ctx context.Context, st *dayState, is *Issue) {
	audioDir := filepath.Join(p.cfg.audioDir, is.Date)
	mp3 := filepath.Join(audioDir, "episode.mp3")

	if st.EpisodeURI == "" && !st.GaveUpPodcast {
		if _, err := os.Stat(filepath.Join(audioDir, "failed.txt")); err == nil {
			st.GaveUpPodcast = true
			p.save(st)
			return
		}
		if fi, err := os.Stat(mp3); err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
			return // not rendered yet
		}
		when, _ := time.ParseInLocation(time.DateOnly, is.Date, time.Local)
		title := truncate(when.Format("Mon 2 Jan")+": "+is.Title, 200)
		uctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		uri, err := p.spotify.Upload(uctx, mp3, title, is.Summary, p.cfg.showID)
		cancel()
		if err != nil {
			st.UploadAttempts++
			st.UploadError = truncate(err.Error(), 300)
			if st.UploadAttempts >= maxUploadAttempts {
				st.GaveUpPodcast = true
			}
			p.log.Printf("%s: upload failed (attempt %d): %v", is.Date, st.UploadAttempts, err)
			p.save(st)
			return
		}
		st.EpisodeURI, st.UploadedAt, st.UploadError = uri, p.now(), ""
		if err := p.saveState(st); err != nil {
			// The one save that matters: without it the next tick uploads
			// again. Stop here and say so loudly.
			p.log.Printf("%s: uploaded %s but could not record it, so the next tick may upload a second copy: %v", is.Date, uri, err)
			return
		}
	}

	if st.EpisodeURI != "" && !st.Ready && st.ReadyChecks < maxReadyChecks {
		wctx, cancel := context.WithTimeout(ctx, readyWait+time.Minute)
		err := p.spotify.WaitReady(wctx, st.EpisodeURI, readyWait)
		cancel()
		st.ReadyChecks++
		if err != nil {
			p.log.Printf("%s: episode not ready yet: %v", is.Date, err)
			p.save(st)
			return
		}
		st.Ready = true
		p.save(st)
	}

	if st.Ready && !st.TimelineSet && st.TimelineError == "" {
		// Chapters and links are a nicety. One try; a failure is logged and
		// recorded, and the episode stands without them.
		if err := p.timeline(ctx, st, is); err != nil {
			st.TimelineError = truncate(err.Error(), 300)
			p.log.Printf("%s: timeline not set: %v", is.Date, err)
		} else {
			st.TimelineSet = true
		}
		p.save(st)
	}
}

func (p *publisher) timeline(ctx context.Context, st *dayState, is *Issue) error {
	t, err := LoadTimings(filepath.Join(p.cfg.audioDir, is.Date, "timings.json"), is)
	if err != nil {
		return err
	}
	tl := BuildTimeline(is, t)
	if len(tl.Items) == 0 {
		return nil
	}
	body, err := json.Marshal(tl)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(p.cfg.stateDir, ".timeline-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return p.spotify.SetTimeline(tctx, st.EpisodeURI, f.Name())
}

// podcastLine says what the email says about the episode, or that the email
// should wait for it.
func (p *publisher) podcastLine(st *dayState, publishedAt time.Time) (Podcast, bool) {
	if p.spotify == nil {
		return Podcast{}, false
	}
	link := "https://open.spotify.com/episode/" + episodeID(st.EpisodeURI)
	switch {
	case st.Ready:
		return Podcast{URL: link}, false
	case st.GaveUpPodcast && st.EpisodeURI == "" && st.UploadAttempts >= maxUploadAttempts:
		return Podcast{Note: "No podcast today: the upload to Spotify failed."}, false
	case st.GaveUpPodcast && st.EpisodeURI == "":
		return Podcast{Note: "No podcast today: the voice render failed."}, false
	case st.EpisodeURI != "" && st.ReadyChecks >= maxReadyChecks:
		return Podcast{URL: link}, false
	case p.now().Sub(publishedAt) < p.cfg.podcastWait:
		return Podcast{}, true
	case st.EpisodeURI != "":
		return Podcast{URL: link}, false
	default:
		return Podcast{Note: "No podcast yet: the voice render is still running, and it will be on the show when it finishes."}, false
	}
}

func (p *publisher) notice(ctx context.Context, st *dayState, reason string) error {
	if st.Emailed {
		return nil
	}
	raw, err := buildNotice(p.cfg.from, p.cfg.to, st.Date, reason, p.now())
	if err != nil {
		return err
	}
	return p.send(ctx, st, raw, "notice")
}

func (p *publisher) send(ctx context.Context, st *dayState, raw []byte, kind string) error {
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	id, err := p.mailer.Send(sctx, raw)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	st.Emailed, st.EmailedAt, st.Kind, st.MessageID = true, p.now(), kind, id
	if err := p.saveState(st); err != nil {
		return fmt.Errorf("sent message %s but could not record it: %w", id, err)
	}
	p.log.Printf("%s: sent the %s (message %s)", st.Date, kind, id)
	p.ping(ctx, kind == "notice")
	return nil
}

// save records progress mid-podcast. A failure is logged rather than
// returned: the step it records has already happened, and losing the record
// only means the next tick repeats a readiness check or a timeline set, which
// is harmless. The upload is recorded with saveState instead.
func (p *publisher) save(st *dayState) {
	if err := p.saveState(st); err != nil {
		p.log.Printf("%s: could not save state: %v", st.Date, err)
	}
}

// ping tells a dead-man's-switch monitor that a day's email went out, so a
// day with no email at all (the NAS down, the stack stopped) raises an alert
// somewhere other than the inbox it would have arrived in.
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

// retention deletes episodes this publisher uploaded once they are older than
// RETENTION_DAYS. It only ever deletes an episode id it recorded itself, never
// one found by listing the show, so anything else on the show is left alone.
func (p *publisher) retention(ctx context.Context) error {
	if p.spotify == nil || p.cfg.retentionDays <= 0 {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(p.cfg.stateDir, "issues"))
	if err != nil {
		return err
	}
	cutoff := p.now().AddDate(0, 0, -p.cfg.retentionDays).Format(time.DateOnly)
	var dates []string
	for _, e := range entries {
		date, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && validDate(date) && date < cutoff {
			dates = append(dates, date)
		}
	}
	sort.Strings(dates)
	var errs []error
	for _, date := range dates {
		st, err := p.loadState(date)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if st.EpisodeURI == "" || st.Deleted || !episodeURIRE.MatchString(st.EpisodeURI) {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = p.spotify.Delete(dctx, st.EpisodeURI)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", date, err))
			continue
		}
		st.Deleted = true
		if err := p.saveState(st); err != nil {
			errs = append(errs, err)
		}
		p.log.Printf("%s: deleted the episode (older than %d days)", date, p.cfg.retentionDays)
	}
	return errors.Join(errs...)
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
