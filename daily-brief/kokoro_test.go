package main

import (
	"encoding/json"
	"testing"
)

// The voice job and the publisher are written in different languages and
// meet at timings.json. testdata/kokoro-*.json are an issue and the timings
// voice.py wrote when it read that issue aloud with the real model, so this
// test fails if either side's idea of the file drifts from the other's.
func TestRealKokoroTimings(t *testing.T) {
	is, err := LoadIssue("testdata/kokoro-issue.json", "2026-11-28")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := LoadTimings("testdata/kokoro-timings.json", is)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(BuildTimeline(is, tm))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"items":[` +
		`{"chapter":{"title":"Introduction","start_time_ms":0}},` +
		`{"chapter":{"title":"Business and markets","start_time_ms":12972}},` +
		`{"chapter":{"title":"AI and software","start_time_ms":21192}},` +
		`{"link":{"start_time_ms":14916,"duration_ms":5376,"url":"https://www.bbc.co.uk/news/business-1"}},` +
		`{"link":{"start_time_ms":23072,"duration_ms":4544,"url":"https://www.theverge.com/ai/1"}}]}`
	if string(got) != want {
		t.Fatalf("timeline:\n got %s\nwant %s", got, want)
	}
}
