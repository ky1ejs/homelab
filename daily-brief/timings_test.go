package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileString(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

func TestTimingsCheck(t *testing.T) {
	is := sampleIssue("2026-11-28")
	if err := sampleTimings().Check(is); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Timings){
		"no duration":       func(tm *Timings) { tm.DurationMS = 0 },
		"too long":          func(tm *Timings) { tm.DurationMS = maxEpisodeMS + 1 },
		"missing section":   func(tm *Timings) { tm.Sections = tm.Sections[:1] },
		"missing item":      func(tm *Timings) { tm.Sections[0].Items = tm.Sections[0].Items[:1] },
		"overlap":           func(tm *Timings) { tm.Sections[0].Items[1].StartMS = 30_000 },
		"empty item":        func(tm *Timings) { tm.Sections[1].Items[0].EndMS = 64_000 },
		"past end":          func(tm *Timings) { tm.Sections[1].Items[0].EndMS = 130_000 },
		"section goes back": func(tm *Timings) { tm.Sections[1].StartMS = 50_000 },
		"negative":          func(tm *Timings) { tm.Sections[0].StartMS = -1; tm.Sections[0].Items[0].StartMS = -1 },
	}
	for name, mutate := range cases {
		tm := sampleTimings()
		mutate(tm)
		if err := tm.Check(is); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadTimingsRefusesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timings.json")
	writeFile(t, path, []byte(`{"duration_ms":1000,"sections":[],"x":1}`))
	if _, err := LoadTimings(path, &Issue{}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("got %v", err)
	}
}

func TestBuildTimeline(t *testing.T) {
	is := sampleIssue("2026-11-28")
	tl := BuildTimeline(is, sampleTimings())
	var chapters []Chapter
	var links []Link
	for _, it := range tl.Items {
		if it.Chapter != nil {
			chapters = append(chapters, *it.Chapter)
		}
		if it.Link != nil {
			links = append(links, *it.Link)
		}
	}
	want := []Chapter{{"Introduction", 0}, {"Business and markets", 20_000}, {"AI and software", 62_000}}
	if len(chapters) != len(want) {
		t.Fatalf("chapters = %+v", chapters)
	}
	for i := range want {
		if chapters[i] != want[i] {
			t.Fatalf("chapter %d = %+v, want %+v", i, chapters[i], want[i])
		}
	}
	if len(links) != 3 || links[2] != (Link{64_000, 36_000, "https://www.theverge.com/ai/1"}) {
		t.Fatalf("links = %+v", links)
	}
	b, _ := json.Marshal(tl)
	if strings.Contains(string(b), `"link":null`) || strings.Contains(string(b), `"chapter":null`) {
		t.Fatalf("null entries in %s", b)
	}
}

func TestBuildTimelineSkipsCloseChapters(t *testing.T) {
	is := sampleIssue("2026-11-28")
	tm := sampleTimings()
	tm.Sections[0].StartMS = 3_000 // under 5s after the opening chapter
	tm.Sections[0].Items[0].StartMS = 3_500
	tl := BuildTimeline(is, tm)
	n := 0
	for _, it := range tl.Items {
		if it.Chapter != nil {
			n++
			if it.Chapter.Title == "Business and markets" {
				t.Fatal("kept a chapter under five seconds after the previous one")
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d chapters, want 2", n)
	}
}

func TestBuildTimelineDropsLoneChapter(t *testing.T) {
	is := sampleIssue("2026-11-28")
	is.Sections = is.Sections[:1]
	tm := &Timings{DurationMS: 30_000, Sections: []SectionTiming{{StartMS: 2_000, Items: []ItemTiming{{3_000, 10_000}, {11_000, 20_000}}}}}
	for _, it := range BuildTimeline(is, tm).Items {
		if it.Chapter != nil {
			t.Fatal("one chapter is below Spotify's minimum of two; want none")
		}
	}
}
