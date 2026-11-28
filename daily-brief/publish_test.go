package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type rig struct {
	t     *testing.T
	p     *publisher
	mail  *fakeMailer
	now   time.Time
	root  string
	pings atomic.Int32
}

const today = "2026-11-28"

func newRig(t *testing.T, podcasts bool) *rig {
	root := t.TempDir()
	r := &rig{t: t, mail: &fakeMailer{}, root: root,
		now: time.Date(2026, 11, 28, 6, 0, 0, 0, time.Local)}
	hc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { r.pings.Add(1) }))
	t.Cleanup(hc.Close)
	cfg := &config{
		issuesDir: filepath.Join(root, "issues"), stateDir: filepath.Join(root, "state"),
		podcastDays: filepath.Join(root, "podcast", "days"),
		account:     "you@example.com", from: "you@example.com", to: "you@example.com",
		podcasts: podcasts, podcastWait: 4 * time.Hour, lookbackDays: 2,
		hcPingURL: hc.URL,
	}
	if err := os.MkdirAll(cfg.podcastDays, 0o700); err != nil {
		t.Fatal(err)
	}
	r.p = &publisher{cfg: cfg, mailer: r.mail, http: hc.Client(), now: func() time.Time { return r.now }, log: log.New(io.Discard, "", 0)}
	return r
}

func (r *rig) publishIssue(date string, at time.Time) {
	path := filepath.Join(r.root, "issues", "published", date, "script.json")
	writeFile(r.t, path, issueJSON(r.t, sampleIssue(date)))
	if err := os.Chtimes(path, at, at); err != nil {
		r.t.Fatal(err)
	}
}

// podcast writes what the podcast job would have written for date.
func (r *rig) podcast(pd *podDay) {
	b, err := json.Marshal(pd)
	if err != nil {
		r.t.Fatal(err)
	}
	writeFile(r.t, filepath.Join(r.p.cfg.podcastDays, pd.Date+".json"), b)
}

func (r *rig) tick() {
	r.t.Helper()
	if err := r.p.Tick(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) state(date string) *dayState {
	st, err := r.p.loadState(date)
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func TestNothingPublishedNothingSent(t *testing.T) {
	r := newRig(t, true)
	r.tick()
	if len(r.mail.sent) != 0 {
		t.Fatal("sent with no issue")
	}
}

func TestEmailOnlyWithoutShow(t *testing.T) {
	r := newRig(t, false)
	r.publishIssue(today, r.now)
	r.tick()
	r.tick()
	msg := r.mail.only(t)
	if strings.Contains(msg, "Spotify") || strings.Contains(msg, "podcast") {
		t.Fatal("mentions a podcast with no show configured")
	}
	if st := r.state(today); !st.Emailed || st.Kind != "brief" || st.MessageID != "msg1" {
		t.Fatalf("%+v", st)
	}
	if r.pings.Load() != 1 {
		t.Fatalf("%d pings", r.pings.Load())
	}
}

func TestBriefWaitsForTheEpisode(t *testing.T) {
	r := newRig(t, true)
	r.publishIssue(today, r.now)
	r.tick()
	r.podcast(&podDay{Date: today, UploadAttempts: 1, EpisodeURI: "spotify:episode:EPISODE0000001"})
	r.tick() // uploaded, not ready: still waits
	if len(r.mail.sent) != 0 {
		t.Fatal("sent before the episode was ready or the wait was up")
	}
	r.podcast(&podDay{Date: today, UploadAttempts: 1, EpisodeURI: "spotify:episode:EPISODE0000001", Ready: true, ReadyChecks: 1})
	r.tick()
	r.tick()
	mustContain(t, r.mail.only(t), "https://open.spotify.com/episode/EPISODE0000001")
}

func TestPodcastNotes(t *testing.T) {
	for name, c := range map[string]struct {
		pd   *podDay
		want string
	}{
		"voice":     {&podDay{Date: today, GaveUp: gaveUpVoice}, "the voice render failed"},
		"upload":    {&podDay{Date: today, GaveUp: gaveUpUpload, UploadAttempts: 3}, "the upload to Spotify failed"},
		"not ready": {&podDay{Date: today, EpisodeURI: "spotify:episode:EPISODE0000001", ReadyChecks: maxReadyChecks}, "open.spotify.com/episode/EPISODE0000001"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, true)
			r.publishIssue(today, r.now)
			r.podcast(c.pd)
			r.tick()
			mustContain(t, r.mail.only(t), c.want)
		})
	}
}

func TestPodcastWaitRunsOut(t *testing.T) {
	r := newRig(t, true)
	r.publishIssue(today, r.now.Add(-5*time.Hour))
	r.tick()
	mustContain(t, r.mail.only(t), "it was not ready in time")
}

// brief-podcast runs a third-party binary with network access. What it writes
// may hold up nothing beyond PODCAST_WAIT and may put no link in the email but
// a well-formed episode's, and it cannot point the publisher at another file.
func TestUnusablePodcastRecord(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	writeFile(t, outside, []byte(`{"date":"`+today+`","episode_uri":"spotify:episode:OUTSIDE00001","ready":true}`))
	for name, write := range map[string]func(path string){
		"garbage":     func(p string) { writeFile(t, p, []byte("{")) },
		"another day": func(p string) { writeFile(t, p, []byte(`{"date":"2026-11-27","ready":true}`)) },
		"bad uri": func(p string) {
			writeFile(t, p, []byte(`{"date":"`+today+`","episode_uri":"https://evil.example/x","ready":true}`))
		},
		"unknown field": func(p string) { writeFile(t, p, []byte(`{"date":"`+today+`","link":"https://evil.example/x"}`)) },
		"odd reason":    func(p string) { writeFile(t, p, []byte(`{"date":"`+today+`","gave_up":"click here"}`)) },
		"symlink out": func(p string) {
			if err := os.Symlink(outside, p); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, true)
			r.publishIssue(today, r.now)
			write(filepath.Join(r.p.cfg.podcastDays, today+".json"))
			r.tick()
			msg := r.mail.only(t)
			mustContain(t, msg, "could not use")
			for _, bad := range []string{"evil.example", "OUTSIDE", "click here"} {
				if strings.Contains(msg, bad) {
					t.Fatalf("the email carries %q", bad)
				}
			}
		})
	}
}

func TestWriterFailedSendsNotice(t *testing.T) {
	r := newRig(t, true)
	writeFile(t, filepath.Join(r.root, "issues", "published", today, "failed.txt"), []byte("timed out after 45m"))
	r.tick()
	r.tick()
	msg := r.mail.only(t)
	mustContain(t, msg, "did not go out")
	mustContain(t, msg, "timed out after 45m")
	if st := r.state(today); st.Kind != "notice" {
		t.Fatalf("%+v", st)
	}
}

func TestInvalidIssueSendsNotice(t *testing.T) {
	r := newRig(t, false)
	is := sampleIssue(today)
	is.Sections[0].Items[0].URL = "javascript:alert(1)"
	writeFile(t, filepath.Join(r.root, "issues", "published", today, "script.json"), issueJSON(t, is))
	r.tick()
	msg := r.mail.only(t)
	mustContain(t, msg, "refused the issue")
	if strings.Contains(msg, "javascript") {
		t.Fatal("notice quotes the refused input")
	}
}

func TestIssueForAnotherDayIsRefused(t *testing.T) {
	r := newRig(t, false)
	writeFile(t, filepath.Join(r.root, "issues", "published", today, "script.json"), issueJSON(t, sampleIssue("2026-11-27")))
	r.tick()
	mustContain(t, r.mail.only(t), "refused the issue")
}

// A send that failed before Gmail could have taken it is retried.
func TestSendNotSentRetries(t *testing.T) {
	r := newRig(t, false)
	r.mail.notSentErr = true
	r.publishIssue(today, r.now)
	if err := r.p.Tick(context.Background()); err == nil {
		t.Fatal("want the send error reported")
	}
	if st := r.state(today); !st.SendStartedAt.IsZero() {
		t.Fatalf("the failed send was left marked as started: %+v", st)
	}
	r.mail.notSentErr = false
	r.tick()
	r.tick()
	r.mail.only(t)
}

// THE TEST THAT MATTERS for the email: Gmail delivers it and the reply is
// lost. The next ticks must not send it again, and must not ping the check.
func TestSendOutcomeUnknownIsNotRepeated(t *testing.T) {
	r := newRig(t, false)
	r.mail.deliverThenErr = true
	r.publishIssue(today, r.now)
	for i := 0; i < 3; i++ {
		err := r.p.Tick(context.Background())
		if err == nil {
			t.Fatalf("tick %d: want the unknown send reported", i)
		}
		if i > 0 {
			mustContain(t, err.Error(), "never confirmed it")
		}
	}
	r.mail.only(t)
	if r.pings.Load() != 0 {
		t.Fatal("pinged the health check for a send nobody confirmed")
	}
}

// The review's reproduction: a disk too full to record the send.
func TestSaveFailsAfterSendIsNotRepeated(t *testing.T) {
	r := newRig(t, false)
	r.publishIssue(today, r.now)
	failSaves(t, 2) // the marker is written, the message id is not
	if err := r.p.Tick(context.Background()); err == nil {
		t.Fatal("want the failed save reported")
	}
	writeState = writeFileAtomic
	for i := 0; i < 3; i++ {
		_ = r.p.Tick(context.Background())
	}
	r.mail.only(t)
}

func TestMarkerUnwritableSendsNothing(t *testing.T) {
	r := newRig(t, false)
	r.publishIssue(today, r.now)
	failSaves(t, 1)
	for i := 0; i < 3; i++ {
		if err := r.p.Tick(context.Background()); err == nil {
			t.Fatal("want the failed save reported")
		}
	}
	if len(r.mail.sent) != 0 {
		t.Fatalf("sent %d messages with nowhere to record them", len(r.mail.sent))
	}
}

func TestYesterdayCatchesUp(t *testing.T) {
	r := newRig(t, false)
	r.publishIssue("2026-11-27", r.now.Add(-24*time.Hour))
	r.publishIssue("2026-11-26", r.now.Add(-48*time.Hour)) // outside LOOKBACK_DAYS
	r.tick()
	mustContain(t, r.mail.only(t), "Fri 27 Nov")
}

func TestDamagedStateStopsThatDay(t *testing.T) {
	r := newRig(t, false)
	r.publishIssue(today, r.now)
	writeFile(t, filepath.Join(r.root, "state", "issues", today+".json"), []byte("{"))
	if err := r.p.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("got %v", err)
	}
	if len(r.mail.sent) != 0 {
		t.Fatal("sent with unreadable state")
	}
}
