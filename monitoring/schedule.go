package main

// When the report goes out: one weekday and a wall-clock time, in the host's
// timezone (docker-compose.yml mounts /etc/localtime).
//
// Deliberately not a cron expression. The only schedule this needs is "once a
// week", and a parser for five fields would be more code than the report.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type schedule struct {
	Weekday time.Weekday
	Hour    int
	Minute  int
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// parseSchedule reads "Mon 08:00". The weekday is the first three letters of
// any spelling, case-insensitive, so "monday 8:00" works too.
func parseSchedule(s string) (schedule, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 || len(fields[0]) < 3 {
		return schedule{}, fmt.Errorf("REPORT_SCHEDULE %q: want \"<weekday> HH:MM\", e.g. \"Mon 08:00\"", s)
	}
	wd, ok := weekdays[strings.ToLower(fields[0][:3])]
	if !ok {
		return schedule{}, fmt.Errorf("REPORT_SCHEDULE %q: unknown weekday %q", s, fields[0])
	}
	hm := strings.SplitN(fields[1], ":", 2)
	if len(hm) != 2 {
		return schedule{}, fmt.Errorf("REPORT_SCHEDULE %q: time must be HH:MM", s)
	}
	h, err1 := strconv.Atoi(hm[0])
	m, err2 := strconv.Atoi(hm[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return schedule{}, fmt.Errorf("REPORT_SCHEDULE %q: time must be HH:MM, 00:00 to 23:59", s)
	}
	return schedule{wd, h, m}, nil
}

// next returns the first scheduled time strictly after now, in now's location.
//
// Built with time.Date rather than by adding 24h repeatedly, so a DST change
// in the week moves the instant and keeps the wall-clock time: "Mon 08:00"
// stays 08:00 local on both sides of the clocks changing.
func (s schedule) next(now time.Time) time.Time {
	y, mo, d := now.Date()
	for i := 0; i <= 7; i++ {
		t := time.Date(y, mo, d+i, s.Hour, s.Minute, 0, 0, now.Location())
		if t.Weekday() == s.Weekday && t.After(now) {
			return t
		}
	}
	// Unreachable: some day in the next eight is the right weekday and after now.
	panic("schedule.next: no candidate in eight days")
}
