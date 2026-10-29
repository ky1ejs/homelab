package main

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	good := map[string]schedule{
		"Mon 08:00":      {time.Monday, 8, 0},
		"monday 8:05":    {time.Monday, 8, 5},
		"  SUN   23:59 ": {time.Sunday, 23, 59},
	}
	for in, want := range good {
		got, err := parseSchedule(in)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Mon", "08:00", "Mo 08:00", "Mon 24:00", "Mon 8", "Mon 08:60", "Funday 08:00", "0 8 * * 1"} {
		if _, err := parseSchedule(in); err == nil {
			t.Errorf("%q: want an error", in)
		}
	}
}

func TestNextIsStrictlyAfter(t *testing.T) {
	s := schedule{time.Monday, 8, 0}
	mon8 := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC) // a Monday
	if got := s.next(mon8); !got.Equal(mon8.AddDate(0, 0, 7)) {
		t.Fatalf("at the scheduled instant, next should be a week later: %v", got)
	}
	if got := s.next(mon8.Add(-time.Minute)); !got.Equal(mon8) {
		t.Fatalf("a minute before: %v", got)
	}
	if got := s.next(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)); !got.Equal(mon8) {
		t.Fatalf("from a Thursday: %v", got)
	}
}

// British Summer Time ended at 01:00 UTC on Sunday 25 October 2026. A report
// due on Monday 26 October must still be at 08:00 local, which is 08:00 UTC,
// not 07:00 UTC as "a week after the last one" would give.
func TestNextKeepsWallClockAcrossDST(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	s := schedule{time.Monday, 8, 0}
	before := time.Date(2026, 10, 19, 8, 0, 1, 0, london) // just after the BST report
	got := s.next(before)
	if got.Hour() != 8 || got.Day() != 26 || got.UTC().Hour() != 8 {
		t.Fatalf("got %v (%v UTC)", got, got.UTC())
	}
}
