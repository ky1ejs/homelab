package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type rig struct {
	t      *testing.T
	p      *publisher
	sp     *fakeSpotify
	mail   *fakeMailer
	now    time.Time
	root   string
	pinged []string
}

const today = "2026-11-28"

func newRig(t *testing.T, podcasts bool) *rig {
	root := t.TempDir()
	r := &rig{t: t, sp: &fakeSpotify{}, mail: &fakeMailer{}, root: root,
		now: time.Date(2026, 11, 28, 6, 0, 0, 0, time.Local)}
	cfg := &config{
		issuesDir: filepath.Join(root, "issues"), audioDir: filepath.Join(root, "audio"), stateDir: filepath.Join(root, "state"),
		account: "you@example.com", from: "you@example.com", to: "you@example.com",
		podcastWait: 4 * time.Hour, retentionDays: 21, lookbackDays: 2,
	}
	r.p = &publisher{cfg: cfg, mailer: r.mail, now: func() time.Time { return r.now }, log: log.New(io.Discard, "", 0)}
	if podcasts {
		cfg.showID = "SHOWID1234"
		r.p.spotify = r.sp
	}
	return r
}

func (r *rig) publishIssue(date string, at time.Time) {
	path := filepath.Join(r.root, "issues", "published", date, "script.json")
	writeFile(r.t, path, issueJSON(r.t, sampleIssue(date)))
	if err := os.Chtimes(path, at, at); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) renderAudio(date string) {
	writeFile(r.t, filepath.Join(r.root, "audio", date, "episode.mp3"), []byte("ID3fake"))
	b, _ := json.Marshal(sampleTimings())
	writeFile(r.t, filepath.Join(r.root, "audio", date, "timings.json"), b)
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
	if len(r.mail.sent) != 0 || len(r.sp.uploads) != 0 {
		t.Fatal("acted with no issue")
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
}

func TestFullDay(t *testing.T) {
	r := newRig(t, true)
	r.publishIssue(today, r.now)
	r.tick() // no audio yet: waits
	if len(r.mail.sent) != 0 {
		t.Fatal("sent before the podcast was ready or the wait was up")
	}
	r.renderAudio(today)
	r.now = r.now.Add(time.Hour)
	r.tick()
	r.tick()

	if len(r.sp.uploads) != 1 || r.sp.uploads[0] != "Sat 28 Nov: Rates hold and a quiet Friday" {
		t.Fatalf("uploads %q", r.sp.uploads)
	}
	if len(r.sp.timelines) != 1 {
		t.Fatalf("%d timelines", len(r.sp.timelines))
	}
	var tl Timeline
	if err := json.Unmarshal([]byte(r.sp.timelines[0]), &tl); err != nil || len(tl.Items) != 6 {
		t.Fatalf("timeline %s (%v)", r.sp.timelines[0], err)
	}
	msg := r.mail.only(t)
	mustContain(t, msg, "https://open.spotify.com/episode/EPISODE0000001")
	st := r.state(today)
	if !st.Ready || !st.TimelineSet || !st.Emailed || !st.done(true) {
		t.Fatalf("%+v", st)
	}
	if entries, _ := filepath.Glob(filepath.Join(r.root, "state", ".timeline-*")); len(entries) != 0 {
		t.Fatalf("left temp files %v", entries)
	}
}

func TestPodcastWaitRunsOut(t *testing.T) {
	r := newRig(t, true)
	r.publishIssue(today, r.now.Add(-5*time.Hour))
	r.tick()
	mustContain(t, r.mail.only(t), "voice render is still running")

	// The audio arrives after the email: it is still uploaded, and no second
	// email goes out.
	r.renderAudio(today)
	r.tick()
	r.tick()
	if len(r.sp.uploads) != 1 || len(r.mail.sent) != 1 {
		t.Fatalf("uploads %d, mails %d", len(r.sp.uploads), len(r.mail.sent))
	}
}

func TestVoiceFailed(t *testing.T) {
	r := newRig(t, true)
	r.publishIssue(today, r.now)
	writeFile(t, filepath.Join(r.root, "audio", today, "failed.txt"), []byte("out of memory"))
	r.tick()
	mustContain(t, r.mail.only(t), "the voice render failed")
	if len(r.sp.uploads) != 0 {
		t.Fatal("uploaded after a failed render")
	}
}

func TestUploadFailsThreeTimes(t *testing.T) {
	r := newRig(t, true)
	r.sp.uploadErr = errFake
	r.publishIssue(today, r.now)
	r.renderAudio(today)
	for i := 0; i < 5; i++ {
		r.tick()
	}
	if st := r.state(today); st.UploadAttempts != maxUploadAttempts || !st.GaveUpPodcast {
		t.Fatalf("%+v", st)
	}
	mustContain(t, r.mail.only(t), "the upload to Spotify failed")
}

func TestNotReadyThenReady(t *testing.T) {
	r := newRig(t, true)
	r.sp.readyErr = errFake
	r.publishIssue(today, r.now)
	r.renderAudio(today)
	r.tick()
	r.tick()
	if len(r.sp.uploads) != 1 || len(r.mail.sent) != 0 {
		t.Fatalf("uploads %d (want 1, no re-upload), mails %d", len(r.sp.uploads), len(r.mail.sent))
	}
	r.sp.readyErr = nil
	r.tick()
	mustContain(t, r.mail.only(t), "open.spotify.com/episode/")
}

func TestTimelineFailureDoesNotBlock(t *testing.T) {
	r := newRig(t, true)
	r.sp.timeErr = errFake
	r.publishIssue(today, r.now)
	r.renderAudio(today)
	r.tick()
	r.tick()
	mustContain(t, r.mail.only(t), "open.spotify.com/episode/")
	if st := r.state(today); st.TimelineError == "" || !st.done(true) {
		t.Fatalf("%+v", st)
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

func TestSendFailureRetries(t *testing.T) {
	r := newRig(t, false)
	r.mail.err = errFake
	r.publishIssue(today, r.now)
	if err := r.p.Tick(context.Background()); err == nil {
		t.Fatal("want the send error reported")
	}
	r.mail.err = nil
	r.tick()
	r.tick()
	r.mail.only(t)
}

func TestYesterdayCatchesUp(t *testing.T) {
	r := newRig(t, false)
	r.publishIssue("2026-11-27", r.now.Add(-24*time.Hour))
	r.publishIssue("2026-11-26", r.now.Add(-48*time.Hour)) // outside LOOKBACK_DAYS
	r.tick()
	mustContain(t, r.mail.only(t), "Fri 27 Nov")
}

func TestRetentionDeletesOnlyRecordedEpisodes(t *testing.T) {
	r := newRig(t, true)
	old := &dayState{Date: "2026-10-01", EpisodeURI: "spotify:episode:OLDEPISODE1", Emailed: true}
	recent := &dayState{Date: "2026-11-20", EpisodeURI: "spotify:episode:NEWEPISODE1", Emailed: true}
	forged := &dayState{Date: "2026-10-02", EpisodeURI: "spotify:show:NOTANEPISODE", Emailed: true}
	for _, st := range []*dayState{old, recent, forged} {
		if err := r.p.saveState(st); err != nil {
			t.Fatal(err)
		}
	}
	r.tick()
	r.tick()
	if strings.Join(r.sp.deleted, ",") != "spotify:episode:OLDEPISODE1" {
		t.Fatalf("deleted %q", r.sp.deleted)
	}
	if !r.state("2026-10-01").Deleted {
		t.Fatal("deletion not recorded")
	}
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
