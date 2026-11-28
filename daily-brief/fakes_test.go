package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// sampleIssue is a valid issue with two sections of two and one items.
func sampleIssue(date string) *Issue {
	return &Issue{
		Date:    date,
		Title:   "Rates hold and a quiet Friday",
		Summary: "The Bank of England held rates. Markets were flat.",
		Sections: []Section{
			{Title: "Business and markets", Items: []Item{
				{Headline: "Rates held at 4%", Text: "The Bank kept rates on hold.", Source: "BBC News", URL: "https://www.bbc.co.uk/news/business-1"},
				{Headline: "FTSE 100 flat", Text: "The index ended the day unchanged.", Source: "FT", URL: "https://www.ft.com/content/abc"},
			}},
			{Title: "AI and software", Items: []Item{
				{Headline: "A new model", Text: "A lab shipped a model.", Source: "The Verge", URL: "https://www.theverge.com/ai/1"},
			}},
		},
	}
}

func issueJSON(t *testing.T, is *Issue) []byte {
	t.Helper()
	b, err := json.Marshal(is)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sampleTimings() *Timings {
	return &Timings{
		DurationMS: 120_000,
		Sections: []SectionTiming{
			{StartMS: 20_000, Items: []ItemTiming{{StartMS: 22_000, EndMS: 40_000}, {StartMS: 41_000, EndMS: 60_000}}},
			{StartMS: 62_000, Items: []ItemTiming{{StartMS: 64_000, EndMS: 100_000}}},
		},
	}
}

// fakeSpotify stands in for the CLI. show is what Spotify holds: an upload
// adds to it, and createdThenErr makes the next upload add to it and still
// fail, as a create that succeeds on the server and times out on the client.
type fakeSpotify struct {
	show           []Episode
	uploadCalls    int
	uploaded       []string // the bytes of each file handed to Upload
	uploadPaths    []string
	uploadErr      error
	createdThenErr bool
	listErr        error
	listCalls      int
	readyErr       error
	readyCalls     int
	timelines      []string // file contents
	timeErr        error
	deleted        []string
	deleteCalls    int
	deleteErr      error
	nextID         int
}

func (f *fakeSpotify) Upload(ctx context.Context, file, title, summary, showID string) (string, error) {
	f.uploadCalls++
	b, err := readFileString(file)
	if err != nil {
		return "", err
	}
	f.uploaded = append(f.uploaded, b)
	f.uploadPaths = append(f.uploadPaths, file)
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	f.nextID++
	uri := fmt.Sprintf("spotify:episode:EPISODE%07d", f.nextID)
	f.show = append(f.show, Episode{URI: uri, Title: title})
	if f.createdThenErr {
		f.createdThenErr = false
		return "", errors.New("context deadline exceeded")
	}
	return uri, nil
}

func (f *fakeSpotify) Episodes(ctx context.Context, showID string) ([]Episode, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Episode(nil), f.show...), nil
}

func (f *fakeSpotify) WaitReady(ctx context.Context, uri string, wait time.Duration) error {
	f.readyCalls++
	return f.readyErr
}

func (f *fakeSpotify) SetTimeline(ctx context.Context, uri, file string) error {
	if f.timeErr != nil {
		return f.timeErr
	}
	b, err := readFileString(file)
	if err != nil {
		return err
	}
	f.timelines = append(f.timelines, b)
	return nil
}

func (f *fakeSpotify) Delete(ctx context.Context, uri string) error {
	f.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, uri)
	return nil
}

// fakeMailer stands in for Gmail. err fails the send with an outcome
// unknown; notSentErr fails it before Gmail could have accepted it;
// deliverThenErr delivers it and still fails, as a reply lost on the way back.
type fakeMailer struct {
	sent           []string
	err            error
	notSentErr     bool
	deliverThenErr bool
}

func (m *fakeMailer) Send(ctx context.Context, raw []byte) (string, error) {
	switch {
	case m.notSentErr:
		return "", notSent(errFake)
	case m.err != nil:
		return "", m.err
	}
	m.sent = append(m.sent, string(raw))
	if m.deliverThenErr {
		return "", errors.New("read: connection reset by peer")
	}
	return fmt.Sprintf("msg%d", len(m.sent)), nil
}

func (m *fakeMailer) only(t *testing.T) string {
	t.Helper()
	if len(m.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(m.sent))
	}
	return m.sent[0]
}

// failSaves makes the nth and later state writes fail (counting from 1), for
// the rest of the test.
func failSaves(t *testing.T, from int) {
	t.Helper()
	n := 0
	writeState = func(path string, data []byte, mode os.FileMode) error {
		n++
		if n >= from {
			return errors.New("no space left on device")
		}
		return writeFileAtomic(path, data, mode)
	}
	t.Cleanup(func() { writeState = writeFileAtomic })
}

var errFake = errors.New("fake failure")

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("missing %q in:\n%s", sub, s)
	}
}
