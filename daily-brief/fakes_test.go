package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type fakeSpotify struct {
	uploads    []string // titles
	uploadErr  error
	readyErr   error
	readyCalls int
	timelines  []string // file contents
	timeErr    error
	deleted    []string
	nextID     int
}

func (f *fakeSpotify) Upload(ctx context.Context, file, title, summary, showID string) (string, error) {
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	f.uploads = append(f.uploads, title)
	f.nextID++
	return fmt.Sprintf("spotify:episode:EPISODE%07d", f.nextID), nil
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
	f.deleted = append(f.deleted, uri)
	return nil
}

type fakeMailer struct {
	sent []string
	err  error
}

func (m *fakeMailer) Send(ctx context.Context, raw []byte) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	m.sent = append(m.sent, string(raw))
	return fmt.Sprintf("msg%d", len(m.sent)), nil
}

func (m *fakeMailer) only(t *testing.T) string {
	t.Helper()
	if len(m.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(m.sent))
	}
	return m.sent[0]
}

var errFake = errors.New("fake failure")

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("missing %q in:\n%s", sub, s)
	}
}
