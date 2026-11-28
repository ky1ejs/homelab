package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// `daily-brief podcast`: one tick of the upload job, run by supercronic every
// few minutes in the brief-podcast container.
//
// For each recent day it moves that day through, once each:
//
//  1. a published issue exists, and its audio, or the voice job gave up;
//  2. the episode is uploaded to the show;
//  3. Spotify has finished processing it;
//  4. its chapters and in-player links are set.
//
// It writes each day's progress to /podcast/days/<date>.json. The publisher
// reads that file, and nothing else of this container's, to decide what the
// email says about the podcast.
//
// AT MOST ONE EPISODE A DAY. An attempt is counted on disk before the upload
// starts, and an upload does not start if that cannot be written. An attempt
// whose episode id never reached disk is not simply retried: Spotify can
// create the episode and still time out on the reply, a deploy can stop the
// container mid-upload, and a full disk can lose the id. So the next tick lists
// the show first, adopts the episode with that day's exact title if there is
// one, and uploads again only if there is not.
//
// The audio comes from brief-voice, which runs third-party ML code over text
// the web shaped. It is read through os.Root, so a link cannot reach outside
// the audio volume, and the MP3 is copied before the CLI sees it, so the file
// that was checked is the file that is uploaded.

type podConfig struct {
	issuesDir, audioDir, podcastDir string
	showID                          string
	retentionDays, lookbackDays     int
	spotifyBin                      string
}

type podcaster struct {
	cfg     *podConfig
	spotify Spotify
	now     func() time.Time
	log     *log.Logger
}

// podDay is one day's progress. The publisher reads it with readPodDay, which
// checks every field it uses.
type podDay struct {
	Date string `json:"date"`

	UploadAttempts  int       `json:"upload_attempts,omitempty"`
	UploadStartedAt time.Time `json:"upload_started_at,omitzero"`
	UploadError     string    `json:"upload_error,omitempty"`
	EpisodeURI      string    `json:"episode_uri,omitempty"`
	UploadedAt      time.Time `json:"uploaded_at,omitzero"`
	Ready           bool      `json:"ready,omitempty"`
	ReadyChecks     int       `json:"ready_checks,omitempty"`
	TimelineSet     bool      `json:"timeline_set,omitempty"`
	TimelineError   string    `json:"timeline_error,omitempty"`
	// GaveUp says why there is no episode: gaveUpVoice, gaveUpUpload or
	// gaveUpIssue.
	GaveUp string `json:"gave_up,omitempty"`

	Deleted        bool   `json:"deleted,omitempty"`
	DeleteAttempts int    `json:"delete_attempts,omitempty"`
	DeleteError    string `json:"delete_error,omitempty"`
}

const (
	gaveUpVoice  = "voice"  // the voice job wrote failed.txt
	gaveUpUpload = "upload" // maxUploadAttempts uploads failed
	gaveUpIssue  = "issue"  // the issue did not pass the validator

	maxUploadAttempts = 3
	// Each check blocks for readyWait; together they cover processing that
	// Spotify's docs put at "a few minutes" with plenty to spare.
	maxReadyChecks = 6
	readyWait      = 10 * time.Minute
	// A delete that keeps failing (the login expired, say) is retried a few
	// times and then left, rather than retried every tick for ever.
	maxDeleteAttempts = 5

	// Three hours at 128 kbit/s is about 170 MB. Nothing maxSpoken allows
	// comes near it.
	maxMP3Bytes = 256 << 20
)

// done reports that nothing is left to do for the day before retention.
func (st *podDay) done() bool {
	switch {
	case st.GaveUp != "":
		return true
	case st.EpisodeURI == "":
		return false
	case !st.Ready:
		return st.ReadyChecks >= maxReadyChecks
	default:
		return st.TimelineSet || st.TimelineError != ""
	}
}

func (p *podcaster) daysDir() string { return filepath.Join(p.cfg.podcastDir, "days") }

func (p *podcaster) statePath(date string) string {
	return filepath.Join(p.daysDir(), date+".json")
}

func (p *podcaster) loadState(date string) (*podDay, error) {
	raw, err := os.ReadFile(p.statePath(date))
	if errors.Is(err, os.ErrNotExist) {
		return &podDay{Date: date}, nil
	}
	if err != nil {
		return nil, err
	}
	var st podDay
	if err := json.Unmarshal(raw, &st); err != nil || st.Date != date {
		return nil, fmt.Errorf("podcast state for %s is damaged; move it aside to start that day over", date)
	}
	return &st, nil
}

func (p *podcaster) saveState(st *podDay) error {
	body, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// 0644, not 0600: the publisher reads this file through a read-only
	// mount of days/, as the same uid, and the directory above is 0700.
	return writeState(p.statePath(st.Date), append(body, '\n'), 0o644)
}

// save records progress where losing the record is harmless: the next tick
// repeats a readiness check or a timeline set. The two records that guard
// against a second episode use saveState and stop on failure.
func (p *podcaster) save(st *podDay) {
	if err := p.saveState(st); err != nil {
		p.log.Printf("%s: could not save the podcast state: %v", st.Date, err)
	}
}

// Tick runs one pass. It holds an exclusive lock for its whole run, so a slow
// tick (a readiness check can block for ten minutes) and the next never both
// act on the same day.
func (p *podcaster) Tick(ctx context.Context) error {
	if err := os.MkdirAll(p.daysDir(), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(p.cfg.podcastDir, ".podcast.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		p.log.Print("another podcast tick is still running; skipping this one")
		return nil
	}
	// Copies left by a tick that was killed. The lock means no tick is using
	// them now.
	for _, pattern := range []string{".upload-*.mp3", ".timeline-*.json"} {
		stale, _ := filepath.Glob(filepath.Join(p.cfg.podcastDir, pattern))
		for _, f := range stale {
			os.Remove(f)
		}
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

func (p *podcaster) day(ctx context.Context, date string) error {
	st, err := p.loadState(date)
	if err != nil {
		return err
	}
	if st.done() {
		return nil
	}

	dir := filepath.Join(p.cfg.issuesDir, "published", date)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil // not written yet
	}
	if _, err := os.Stat(filepath.Join(dir, "failed.txt")); err == nil {
		return nil // no issue today; the publisher sends the notice
	}
	is, err := LoadIssue(filepath.Join(dir, "script.json"), date)
	if err != nil {
		st.GaveUp = gaveUpIssue
		p.save(st)
		p.log.Printf("%s: refused the issue, so there is no episode: %v", date, err)
		return nil
	}

	audio, err := os.OpenRoot(p.cfg.audioDir)
	if err != nil {
		return err
	}
	defer audio.Close()

	if st.EpisodeURI == "" {
		if err := p.upload(ctx, st, is, audio); err != nil || st.EpisodeURI == "" {
			return err
		}
	}

	if !st.Ready && st.ReadyChecks < maxReadyChecks {
		wctx, cancel := context.WithTimeout(ctx, readyWait+time.Minute)
		err := p.spotify.WaitReady(wctx, st.EpisodeURI, readyWait)
		cancel()
		st.ReadyChecks++
		if err != nil {
			p.log.Printf("%s: episode not ready yet: %v", date, err)
			p.save(st)
			return nil
		}
		st.Ready = true
		p.save(st)
	}

	if st.Ready && !st.TimelineSet && st.TimelineError == "" {
		// Chapters and links are a nicety. One try; a failure is logged and
		// recorded, and the episode stands without them.
		if err := p.timeline(ctx, st, is, audio); err != nil {
			st.TimelineError = truncate(err.Error(), 300)
			p.log.Printf("%s: timeline not set: %v", date, err)
		} else {
			st.TimelineSet = true
		}
		p.save(st)
	}
	return nil
}

// episodeTitle is the title the episode is uploaded under, and the title a
// later tick looks for when an earlier upload's outcome was not recorded.
func episodeTitle(is *Issue) string {
	when, _ := time.ParseInLocation(time.DateOnly, is.Date, time.Local)
	return truncate(when.Format("Mon 2 Jan")+": "+is.Title, 200)
}

// upload gets the day's episode onto the show, at most once. It returns an
// error only for what the operator must see; an upload that failed is
// recorded on st and retried on a later tick.
func (p *podcaster) upload(ctx context.Context, st *podDay, is *Issue, audio *os.Root) error {
	if _, err := audio.Lstat(filepath.Join(is.Date, "failed.txt")); err == nil {
		st.GaveUp = gaveUpVoice
		p.save(st)
		return nil
	}
	mp3 := filepath.Join(is.Date, "episode.mp3")
	if fi, err := audio.Stat(mp3); err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return nil // not rendered yet
	}
	title := episodeTitle(is)

	if st.UploadAttempts > 0 {
		// An earlier attempt may have made the episode without its id
		// reaching disk. Look before uploading another.
		uri, err := p.find(ctx, title)
		if err != nil {
			st.UploadError = truncate(err.Error(), 300)
			p.save(st)
			p.log.Printf("%s: could not list the show to check for an earlier upload, so not uploading: %v", is.Date, err)
			return nil
		}
		if uri != "" {
			st.EpisodeURI, st.UploadedAt, st.UploadError = uri, p.now(), ""
			if err := p.saveState(st); err != nil {
				return fmt.Errorf("found the earlier upload %s but could not record it: %w", uri, err)
			}
			p.log.Printf("%s: found the episode an earlier attempt uploaded: %s", is.Date, uri)
			return nil
		}
		if st.UploadAttempts >= maxUploadAttempts {
			st.GaveUp = gaveUpUpload
			p.save(st)
			p.log.Printf("%s: gave up uploading after %d attempts: %s", is.Date, st.UploadAttempts, st.UploadError)
			return nil
		}
	}

	copyPath, err := p.copyMP3(audio, mp3)
	if err != nil {
		return fmt.Errorf("could not copy the episode out of the audio volume: %w", err)
	}
	defer os.Remove(copyPath)

	// Counted before it starts, and not started if the count cannot be
	// written: an uncounted upload is the one a later tick could repeat.
	st.UploadAttempts++
	st.UploadStartedAt = p.now()
	if err := p.saveState(st); err != nil {
		return fmt.Errorf("not uploading, because the attempt could not be recorded: %w", err)
	}
	uctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	uri, err := p.spotify.Upload(uctx, copyPath, title, is.Summary, p.cfg.showID)
	cancel()
	if err != nil {
		st.UploadError = truncate(err.Error(), 300)
		p.save(st)
		p.log.Printf("%s: upload failed (attempt %d): %v", is.Date, st.UploadAttempts, err)
		return nil
	}
	st.EpisodeURI, st.UploadedAt, st.UploadError = uri, p.now(), ""
	if err := p.saveState(st); err != nil {
		// The next tick finds it on the show by its title.
		return fmt.Errorf("uploaded %s but could not record it: %w", uri, err)
	}
	p.log.Printf("%s: uploaded %s", is.Date, uri)
	return nil
}

// find returns the URI of the show's episode with exactly this title, or ""
// when there is none.
func (p *podcaster) find(ctx context.Context, title string) (string, error) {
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	eps, err := p.spotify.Episodes(lctx, p.cfg.showID)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range eps {
		if strings.TrimSpace(e.Title) == title {
			found = append(found, e.URI)
		}
	}
	if len(found) > 1 {
		p.log.Printf("%d episodes on the show are titled %q; keeping %s", len(found), title, found[0])
	}
	if len(found) == 0 {
		return "", nil
	}
	return found[0], nil
}

// copyMP3 copies audio/<name> into a file of this container's own. The CLI
// opens the path it is given itself, so uploading the voice volume's file
// directly would let that file be swapped between this check and the upload.
// The name ends in .mp3 because the CLI decides the media type by extension.
func (p *podcaster) copyMP3(audio *os.Root, name string) (string, error) {
	src, err := audio.Open(name)
	if err != nil {
		return "", err
	}
	defer src.Close()
	if fi, err := src.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", errors.New("episode.mp3 is not a regular file")
	}
	dst, err := os.CreateTemp(p.cfg.podcastDir, ".upload-*.mp3")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(dst, io.LimitReader(src, maxMP3Bytes+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxMP3Bytes {
		err = fmt.Errorf("episode.mp3 is over %d bytes", maxMP3Bytes)
	}
	if err != nil {
		os.Remove(dst.Name())
		return "", err
	}
	return dst.Name(), nil
}

func (p *podcaster) timeline(ctx context.Context, st *podDay, is *Issue, audio *os.Root) error {
	raw, err := readUnder(audio, filepath.Join(is.Date, "timings.json"), 64<<10)
	if err != nil {
		return err
	}
	t, err := ParseTimings(raw, is)
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
	f, err := os.CreateTemp(p.cfg.podcastDir, ".timeline-*.json")
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

// retention deletes episodes this job uploaded once they are older than
// RETENTION_DAYS. It only ever deletes an episode id it recorded itself, never
// one found by listing the show, so anything else on the show is left alone.
func (p *podcaster) retention(ctx context.Context) error {
	if p.cfg.retentionDays <= 0 {
		return nil
	}
	entries, err := os.ReadDir(p.daysDir())
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
		if st.EpisodeURI == "" || st.Deleted || st.DeleteAttempts >= maxDeleteAttempts || !episodeURIRE.MatchString(st.EpisodeURI) {
			continue
		}
		st.DeleteAttempts++
		dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = p.spotify.Delete(dctx, st.EpisodeURI)
		cancel()
		switch {
		case err == nil, errors.Is(err, errEpisodeGone):
			st.Deleted, st.DeleteError = true, ""
			p.log.Printf("%s: deleted the episode (older than %d days)", date, p.cfg.retentionDays)
		case st.DeleteAttempts >= maxDeleteAttempts:
			st.DeleteError = truncate(err.Error(), 300)
			errs = append(errs, fmt.Errorf("delete %s: gave up after %d attempts, delete it by hand: %w", date, st.DeleteAttempts, err))
		default:
			st.DeleteError = truncate(err.Error(), 300)
			errs = append(errs, fmt.Errorf("delete %s (attempt %d): %w", date, st.DeleteAttempts, err))
		}
		if err := p.saveState(st); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// readUnder reads a regular file under root, capped at max bytes.
func readUnder(root *os.Root, name string, max int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errors.New("not a regular file")}
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over %d bytes", name, max)
	}
	return b, nil
}

// readPodDay is the publisher's reader for days/<date>.json. brief-podcast
// runs a third-party binary with network access, so what it wrote is checked
// before it reaches the email: a day missing is nil, and anything that does
// not parse strictly, names another day or holds an episode id that is not
// one is refused.
func readPodDay(days *os.Root, date string) (*podDay, error) {
	raw, err := readUnder(days, date+".json", 16<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st podDay
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, errors.New("not the expected JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the JSON object")
	}
	switch {
	case st.Date != date:
		return nil, errors.New("it is for another day")
	case st.EpisodeURI != "" && !episodeURIRE.MatchString(st.EpisodeURI):
		return nil, errors.New("its episode id is malformed")
	case st.GaveUp != "" && st.GaveUp != gaveUpVoice && st.GaveUp != gaveUpUpload && st.GaveUp != gaveUpIssue:
		return nil, errors.New("its reason for giving up is not one this publisher knows")
	case st.ReadyChecks < 0 || st.UploadAttempts < 0:
		return nil, errors.New("it holds a negative count")
	}
	return &st, nil
}
