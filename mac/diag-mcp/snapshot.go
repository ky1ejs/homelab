package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// The collector's file (./mac-diag). Root writes it whole, with a
// rename, so a read sees one run's data or the one before. This server reads
// it on every call and keeps nothing: it is small, and a cache would only
// make answers older.

const (
	snapshotSchema   = 1
	maxSnapshotBytes = 16 << 20

	// When an answer says its data is old: the collector's own intervals,
	// twice over, so one missed run does not warn.
	fastStale  = 15 * time.Minute
	diskStale  = 13 * time.Hour
	usageStale = 3 * time.Hour
)

var errNoSnapshot = errors.New("no snapshot yet: the collector (the mac-diag-collect daemon) has not written one")

type snapshot map[string]any

type snapshotFile struct {
	path string
	now  func() time.Time
}

func (f *snapshotFile) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

func (f *snapshotFile) load() (snapshot, error) {
	fh, err := os.Open(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoSnapshot
	}
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSnapshotBytes {
		return nil, errors.New("the snapshot is larger than this server reads")
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("the snapshot is not JSON: %w", err)
	}
	if v, _ := s["schema"].(float64); v != snapshotSchema {
		return nil, fmt.Errorf("the snapshot's schema is %v; this server reads %d", s["schema"], snapshotSchema)
	}
	return s, nil
}

// section is one top-level object of the snapshot, or nil.
func (s snapshot) section(name string) map[string]any {
	m, _ := s[name].(map[string]any)
	return m
}

func timeOf(m map[string]any, key string) (time.Time, bool) {
	v, _ := m[key].(string)
	t, err := time.Parse(time.RFC3339, v)
	return t, err == nil
}

// asOf says when each part was collected and how long ago, and warns about
// any part older than its collector's interval allows.
func (s snapshot) asOf(now time.Time) (map[string]any, []string) {
	out := map[string]any{}
	var warn []string
	for _, p := range []struct {
		name  string
		stale time.Duration
		what  string
	}{
		{"fast", fastStale, "host, CI and service figures"},
		{"disk", diskStale, "disk scan"},
		{"usage", usageStale, "usage trend"},
	} {
		t, ok := timeOf(s.section(p.name), "collected")
		if !ok {
			warn = append(warn, fmt.Sprintf("no %s yet", p.what))
			continue
		}
		age := now.Sub(t)
		out[p.name] = map[string]any{"collected": t.UTC().Format(time.RFC3339), "age_minutes": int(age.Minutes())}
		if age > p.stale {
			warn = append(warn, fmt.Sprintf("the %s is %s old: the collector may have stopped (hl status, hl logs daemon-mac-diag-collect)", p.what, age.Round(time.Minute)))
		}
	}
	if t, ok := timeOf(s.section("disk_scan"), "running_since"); ok {
		out["disk_scan_running_since"] = t.UTC().Format(time.RFC3339)
	}
	return out, warn
}

// pick copies the named keys of m that are present.
func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}
