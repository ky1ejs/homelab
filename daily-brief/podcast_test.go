package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type podRig struct {
	t    *testing.T
	p    *podcaster
	sp   *fakeSpotify
	root string
}

func newPodRig(t *testing.T) *podRig {
	root := t.TempDir()
	r := &podRig{t: t, sp: &fakeSpotify{}, root: root}
	cfg := &podConfig{
		issuesDir: filepath.Join(root, "issues"), audioDir: filepath.Join(root, "audio"),
		podcastDir: filepath.Join(root, "podcast"), showID: "SHOWID1234",
		retentionDays: 21, lookbackDays: 2,
	}
	for _, d := range []string{cfg.audioDir, cfg.podcastDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 28, 6, 0, 0, 0, time.Local)
	r.p = &podcaster{cfg: cfg, spotify: r.sp, now: func() time.Time { return now }, log: log.New(io.Discard, "", 0)}
	return r
}

func (r *podRig) publish(date string) {
	writeFile(r.t, filepath.Join(r.p.cfg.issuesDir, "published", date, "script.json"), issueJSON(r.t, sampleIssue(date)))
}

func (r *podRig) render(date string) {
	dir := filepath.Join(r.p.cfg.audioDir, date)
	writeFile(r.t, filepath.Join(dir, "episode.mp3"), []byte("MP3DATA"))
	b, err := json.Marshal(sampleTimings())
	if err != nil {
		r.t.Fatal(err)
	}
	writeFile(r.t, filepath.Join(dir, "timings.json"), b)
}

func (r *podRig) tick() error { return r.p.Tick(context.Background()) }

func (r *podRig) mustTick() {
	r.t.Helper()
	if err := r.tick(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *podRig) state(date string) *podDay {
	r.t.Helper()
	st, err := r.p.loadState(date)
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func TestPodcastFullDay(t *testing.T) {
	r := newPodRig(t)
	r.publish(today)
	r.render(today)
	r.mustTick()
	r.mustTick()

	if r.sp.uploadCalls != 1 || r.sp.uploaded[0] != "MP3DATA" || r.sp.readyCalls != 1 || len(r.sp.timelines) != 1 {
		t.Fatalf("%+v", r.sp)
	}
	// The CLI was handed a copy in the podcast job's own directory, and the
	// copy is gone.
	up := r.sp.uploadPaths[0]
	if filepath.Dir(up) != r.p.cfg.podcastDir || !strings.HasSuffix(up, ".mp3") {
		t.Fatalf("uploaded %s, want a copy under %s", up, r.p.cfg.podcastDir)
	}
	if _, err := os.Stat(up); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the upload copy was left behind: %v", err)
	}
	if r.sp.show[0].Title != "Sat 28 Nov: Rates hold and a quiet Friday" {
		t.Fatalf("title %q", r.sp.show[0].Title)
	}
	st := r.state(today)
	if st.EpisodeURI != "spotify:episode:EPISODE0000001" || !st.Ready || !st.TimelineSet || st.UploadAttempts != 1 {
		t.Fatalf("%+v", st)
	}

	// The publisher can read what this wrote.
	fi, err := os.Stat(r.p.statePath(today))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("days/%s.json: %v %v", today, fi.Mode(), err)
	}
	days, err := os.OpenRoot(r.p.daysDir())
	if err != nil {
		t.Fatal(err)
	}
	defer days.Close()
	if pd, err := readPodDay(days, today); err != nil || !pd.Ready {
		t.Fatalf("readPodDay = %+v, %v", pd, err)
	}
}

func TestPodcastRemovesStaleCopies(t *testing.T) {
	r := newPodRig(t)
	stale := filepath.Join(r.p.cfg.podcastDir, ".upload-123.mp3")
	writeFile(t, stale, []byte("MP3DATA"))
	r.mustTick()
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a killed tick's copy was left: %v", err)
	}
}

func TestPodcastWaitsForAudio(t *testing.T) {
	r := newPodRig(t)
	r.publish(today)
	r.mustTick()
	if r.sp.uploadCalls != 0 || r.state(today).UploadAttempts != 0 {
		t.Fatal("uploaded before the audio existed")
	}
	writeFile(t, filepath.Join(r.p.cfg.audioDir, today, "episode.mp3"), nil)
	r.mustTick()
	if r.sp.uploadCalls != 0 {
		t.Fatal("uploaded an empty file")
	}
}

func TestPodcastNothingWithoutAnIssue(t *testing.T) {
	r := newPodRig(t)
	writeFile(t, filepath.Join(r.p.cfg.issuesDir, "published", today, "failed.txt"), []byte("no token"))
	r.render(today)
	r.mustTick()
	if r.sp.uploadCalls != 0 {
		t.Fatal("uploaded a day the writer gave up on")
	}
}

func TestPodcastVoiceFailed(t *testing.T) {
	r := newPodRig(t)
	r.publish(today)
	writeFile(t, filepath.Join(r.p.cfg.audioDir, today, "failed.txt"), []byte("render failed"))
	r.mustTick()
	if st := r.state(today); st.GaveUp != gaveUpVoice || r.sp.uploadCalls != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestPodcastRefusedIssue(t *testing.T) {
	r := newPodRig(t)
	is := sampleIssue(today)
	is.Title = ""
	writeFile(t, filepath.Join(r.p.cfg.issuesDir, "published", today, "script.json"), issueJSON(t, is))
	r.render(today)
	r.mustTick()
	if st := r.state(today); st.GaveUp != gaveUpIssue || r.sp.uploadCalls != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestPodcastUploadGivesUpAfterThree(t *testing.T) {
	r := newPodRig(t)
	r.sp.uploadErr = errors.New("save-to-spotify upload: not logged in")
	r.publish(today)
	r.render(today)
	for i := 0; i < 6; i++ {
		r.mustTick()
	}
	st := r.state(today)
	if r.sp.uploadCalls != maxUploadAttempts || st.GaveUp != gaveUpUpload || !strings.Contains(st.UploadError, "not logged in") {
		t.Fatalf("%d uploads, %+v", r.sp.uploadCalls, st)
	}
	// Each retry looked for an earlier upload first.
	if r.sp.listCalls != maxUploadAttempts {
		t.Fatalf("%d lists", r.sp.listCalls)
	}
}

// THE TEST THAT MATTERS for the podcast: Spotify creates the episode and the
// reply is lost. The next tick finds it on the show instead of uploading a
// second one.
func TestPodcastAdoptsAnUploadWhoseReplyWasLost(t *testing.T) {
	r := newPodRig(t)
	r.sp.createdThenErr = true
	r.publish(today)
	r.render(today)
	r.mustTick()
	if st := r.state(today); st.EpisodeURI != "" || st.UploadAttempts != 1 {
		t.Fatalf("%+v", st)
	}
	r.mustTick()
	st := r.state(today)
	if r.sp.uploadCalls != 1 || len(r.sp.show) != 1 || st.EpisodeURI != r.sp.show[0].URI || !st.Ready {
		t.Fatalf("%d uploads, %d on the show, %+v", r.sp.uploadCalls, len(r.sp.show), st)
	}
}

// The review's reproduction: a disk too full to record the episode id.
func TestPodcastAdoptsAnUploadItCouldNotRecord(t *testing.T) {
	r := newPodRig(t)
	r.publish(today)
	r.render(today)
	failSaves(t, 2) // the attempt is recorded, the episode id is not
	if err := r.tick(); err == nil || !strings.Contains(err.Error(), "could not record") {
		t.Fatalf("got %v", err)
	}
	writeState = writeFileAtomic
	r.mustTick()
	r.mustTick()
	if r.sp.uploadCalls != 1 || r.state(today).EpisodeURI != "spotify:episode:EPISODE0000001" {
		t.Fatalf("%d uploads, %+v", r.sp.uploadCalls, r.state(today))
	}
}

func TestPodcastAttemptUnwritableUploadsNothing(t *testing.T) {
	r := newPodRig(t)
	r.publish(today)
	r.render(today)
	failSaves(t, 1)
	for i := 0; i < 3; i++ {
		if err := r.tick(); err == nil {
			t.Fatal("want the failed save reported")
		}
	}
	if r.sp.uploadCalls != 0 {
		t.Fatalf("%d uploads with nowhere to count them", r.sp.uploadCalls)
	}
}

func TestPodcastListErrorUploadsNothing(t *testing.T) {
	r := newPodRig(t)
	r.sp.uploadErr = errors.New("timeout")
	r.publish(today)
	r.render(today)
	r.mustTick()
	r.sp.uploadErr = nil
	r.sp.listErr = errors.New("save-to-spotify episodes: 503")
	r.mustTick()
	r.mustTick()
	if st := r.state(today); r.sp.uploadCalls != 1 || !strings.Contains(st.UploadError, "503") {
		t.Fatalf("%d uploads, %+v", r.sp.uploadCalls, st)
	}
	r.sp.listErr = nil
	r.mustTick()
	if r.sp.uploadCalls != 2 || r.state(today).EpisodeURI == "" {
		t.Fatalf("%d uploads after the list came back", r.sp.uploadCalls)
	}
}

func TestPodcastReadiness(t *testing.T) {
	r := newPodRig(t)
	r.sp.readyErr = errors.New("episode not ready (\"PROCESSING\")")
	r.publish(today)
	r.render(today)
	r.mustTick()
	if st := r.state(today); st.Ready || st.ReadyChecks != 1 || len(r.sp.timelines) != 0 {
		t.Fatalf("%+v", st)
	}
	r.sp.readyErr = nil
	r.mustTick()
	if st := r.state(today); !st.Ready || !st.TimelineSet {
		t.Fatalf("%+v", st)
	}
}

func TestPodcastStopsCheckingReadiness(t *testing.T) {
	r := newPodRig(t)
	r.sp.readyErr = errors.New("not ready")
	r.publish(today)
	r.render(today)
	for i := 0; i < maxReadyChecks+3; i++ {
		r.mustTick()
	}
	if r.sp.readyCalls != maxReadyChecks || !r.state(today).done() {
		t.Fatalf("%d checks", r.sp.readyCalls)
	}
}

func TestPodcastTimelineFailureIsTriedOnce(t *testing.T) {
	r := newPodRig(t)
	r.sp.timeErr = errors.New("400 bad timeline")
	r.publish(today)
	r.render(today)
	r.mustTick()
	r.sp.timeErr = nil
	r.mustTick()
	if st := r.state(today); st.TimelineSet || !strings.Contains(st.TimelineError, "bad timeline") || len(r.sp.timelines) != 0 {
		t.Fatalf("%+v", st)
	}
}

// brief-voice runs third-party ML code and could leave a link where its
// output should be. Nothing outside the audio volume is uploaded or read.
func TestPodcastRefusesLinksOutOfTheAudioVolume(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "episode.mp3"), []byte("SECRET"))
	tm, err := json.Marshal(sampleTimings())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "timings.json"), tm)

	link := func(t *testing.T, target, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("mp3", func(t *testing.T) {
		r := newPodRig(t)
		r.publish(today)
		link(t, filepath.Join(outside, "episode.mp3"), filepath.Join(r.p.cfg.audioDir, today, "episode.mp3"))
		r.mustTick()
		if r.sp.uploadCalls != 0 {
			t.Fatalf("uploaded %q", r.sp.uploaded)
		}
	})
	t.Run("day directory", func(t *testing.T) {
		r := newPodRig(t)
		r.publish(today)
		link(t, outside, filepath.Join(r.p.cfg.audioDir, today))
		r.mustTick()
		if r.sp.uploadCalls != 0 {
			t.Fatalf("uploaded %q", r.sp.uploaded)
		}
	})
	t.Run("timings", func(t *testing.T) {
		r := newPodRig(t)
		r.publish(today)
		r.render(today)
		timings := filepath.Join(r.p.cfg.audioDir, today, "timings.json")
		if err := os.Remove(timings); err != nil {
			t.Fatal(err)
		}
		link(t, filepath.Join(outside, "timings.json"), timings)
		r.mustTick()
		if st := r.state(today); st.TimelineSet || st.TimelineError == "" || len(r.sp.timelines) != 0 {
			t.Fatalf("%+v", st)
		}
	})
}

func TestPodcastRetention(t *testing.T) {
	r := newPodRig(t)
	if err := os.MkdirAll(r.p.daysDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	put := func(st *podDay) {
		t.Helper()
		if err := r.p.saveState(st); err != nil {
			t.Fatal(err)
		}
	}
	put(&podDay{Date: "2026-11-01", EpisodeURI: "spotify:episode:OLDEPISODE01", Ready: true})
	put(&podDay{Date: "2026-11-02", EpisodeURI: "spotify:episode:../../me"}) // not an id
	put(&podDay{Date: "2026-11-03", GaveUp: gaveUpVoice})
	put(&podDay{Date: "2026-11-20", EpisodeURI: "spotify:episode:NEWEPISODE01", Ready: true})
	r.sp.show = []Episode{{URI: "spotify:episode:SOMEONEELSE1", Title: "Not ours"}}

	r.mustTick()
	r.mustTick()
	if strings.Join(r.sp.deleted, ",") != "spotify:episode:OLDEPISODE01" || r.sp.deleteCalls != 1 {
		t.Fatalf("deleted %q in %d calls", r.sp.deleted, r.sp.deleteCalls)
	}
	if st := r.state("2026-11-01"); !st.Deleted {
		t.Fatalf("%+v", st)
	}
}

func TestPodcastRetentionGoneAndGivingUp(t *testing.T) {
	r := newPodRig(t)
	if err := os.MkdirAll(r.p.daysDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.p.saveState(&podDay{Date: "2026-11-01", EpisodeURI: "spotify:episode:OLDEPISODE01"}); err != nil {
		t.Fatal(err)
	}
	r.sp.deleteErr = errEpisodeGone
	r.mustTick()
	if !r.state("2026-11-01").Deleted {
		t.Fatal("an episode already gone was not marked deleted")
	}

	if err := r.p.saveState(&podDay{Date: "2026-11-02", EpisodeURI: "spotify:episode:OLDEPISODE02"}); err != nil {
		t.Fatal(err)
	}
	r.sp.deleteErr = errors.New("save-to-spotify episodes: not logged in")
	r.sp.deleteCalls = 0
	for i := 0; i < maxDeleteAttempts+3; i++ {
		err := r.tick()
		if i < maxDeleteAttempts && err == nil {
			t.Fatalf("tick %d: want the failed delete reported", i)
		}
	}
	if r.sp.deleteCalls != maxDeleteAttempts {
		t.Fatalf("%d delete calls", r.sp.deleteCalls)
	}
}

func TestReadPodDay(t *testing.T) {
	dir := t.TempDir()
	days, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer days.Close()
	if pd, err := readPodDay(days, today); pd != nil || err != nil {
		t.Fatalf("a missing day: %+v, %v", pd, err)
	}
	for name, body := range map[string]string{
		"negative": `{"date":"` + today + `","ready_checks":-1}`,
		"trailing": `{"date":"` + today + `"} {"date":"x"}`,
		"too big":  `{"date":"` + today + `","upload_error":"` + strings.Repeat("x", 17<<10) + `"}`,
	} {
		writeFile(t, filepath.Join(dir, today+".json"), []byte(body))
		if _, err := readPodDay(days, today); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
