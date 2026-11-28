package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Timings is what voice.py writes beside each episode: where every section and
// every item starts and ends in the audio, in milliseconds. It is computed from
// sample counts, not estimated, which is what Spotify's timeline rules ask for:
// a chapter or a link that starts past the end of the audio is refused.
//
// It comes from the voice container, which runs third-party ML code over
// attacker-influenced text, so it is checked here like any other input: the
// shape must match the issue's, and every span must sit inside the audio and
// after the one before it.
type Timings struct {
	DurationMS int64           `json:"duration_ms"`
	Sections   []SectionTiming `json:"sections"`
}

type SectionTiming struct {
	StartMS int64        `json:"start_ms"`
	Items   []ItemTiming `json:"items"`
}

type ItemTiming struct {
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

// maxEpisodeMS is three hours. Nothing maxSpoken allows comes near it; a
// longer duration means the file is wrong.
const maxEpisodeMS = 3 * 60 * 60 * 1000

func LoadTimings(path string, is *Issue) (*Timings, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var t Timings
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("timings: %v", jsonProblem(err))
	}
	if err := t.Check(is); err != nil {
		return nil, err
	}
	return &t, nil
}

func (t *Timings) Check(is *Issue) error {
	if t.DurationMS <= 0 || t.DurationMS > maxEpisodeMS {
		return errors.New("timings: duration out of range")
	}
	if len(t.Sections) != len(is.Sections) {
		return fmt.Errorf("timings: %d sections for an issue with %d", len(t.Sections), len(is.Sections))
	}
	var last int64
	for i, s := range t.Sections {
		if len(s.Items) != len(is.Sections[i].Items) {
			return fmt.Errorf("timings: section %d has %d items, the issue %d", i, len(s.Items), len(is.Sections[i].Items))
		}
		if s.StartMS < last {
			return fmt.Errorf("timings: section %d starts before the end of what precedes it", i)
		}
		last = s.StartMS
		for j, it := range s.Items {
			if it.StartMS < last || it.EndMS <= it.StartMS || it.EndMS > t.DurationMS {
				return fmt.Errorf("timings: section %d item %d is out of order or outside the audio", i, j)
			}
			last = it.EndMS
		}
	}
	return nil
}

// Timeline is save-to-spotify's `timeline set --from-file` format, and only the
// two kinds of entry used here.
type Timeline struct {
	Items []TimelineItem `json:"items"`
}

type TimelineItem struct {
	Chapter *Chapter `json:"chapter,omitempty"`
	Link    *Link    `json:"link,omitempty"`
}

type Chapter struct {
	Title       string `json:"title"`
	StartTimeMS int64  `json:"start_time_ms"`
}

type Link struct {
	StartTimeMS int64  `json:"start_time_ms"`
	DurationMS  int64  `json:"duration_ms"`
	URL         string `json:"url"`
}

// minChapterGapMS is Spotify's rule: consecutive chapter starts at least five
// seconds apart.
const minChapterGapMS = 5000

// BuildTimeline makes a chapter per section, after an opening one at 0 for the
// greeting and the summary, and an in-player link per item to its source for
// as long as that item is being read. Spotify's rules, from the CLI's own
// reference: chapters are optional but number at least two, the first at 0,
// strictly increasing and at least five seconds apart, each before the end of
// the audio; links have a positive duration, an http(s) URL, and do not
// overlap. Items' spans are disjoint by Check, so their links cannot overlap.
//
// A section that would start under five seconds after the previous chapter
// gets no chapter of its own rather than failing the whole timeline.
func BuildTimeline(is *Issue, t *Timings) Timeline {
	var tl Timeline
	chapters := []Chapter{{Title: "Introduction", StartTimeMS: 0}}
	for i, s := range t.Sections {
		prev := chapters[len(chapters)-1].StartTimeMS
		if s.StartMS-prev >= minChapterGapMS && s.StartMS < t.DurationMS {
			chapters = append(chapters, Chapter{Title: is.Sections[i].Title, StartTimeMS: s.StartMS})
		}
	}
	if len(chapters) >= 2 {
		for i := range chapters {
			tl.Items = append(tl.Items, TimelineItem{Chapter: &chapters[i]})
		}
	}
	for i, s := range t.Sections {
		for j, it := range s.Items {
			tl.Items = append(tl.Items, TimelineItem{Link: &Link{
				StartTimeMS: it.StartMS,
				DurationMS:  it.EndMS - it.StartMS,
				URL:         is.Sections[i].Items[j].URL,
			}})
		}
	}
	return tl
}
