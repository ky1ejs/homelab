package main

import (
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/sample-*.{txt,html} from the current renderer")

// A fresh install starts inside the window. That week is partial, not broken:
// no finding, and the email says when monitoring began.
func TestFreshInstallIsAPartialWeekNotAGap(t *testing.T) {
	s := summarize(testSystem(), week(34, calm()), nil, nil, testFrom, testTo)
	if want := testTo.Add(-70 * time.Hour); !s.HistoryStart.Equal(want) {
		t.Fatalf("history start %v, want %v", s.HistoryStart, want)
	}
	if fs := evaluate(s, time.UTC); len(fs) != 0 {
		t.Fatalf("a fresh install should be all clear, got %+v", fs)
	}
	_, text, html := render(s, nil, "", time.UTC)
	for _, body := range []string{text, html} {
		if !strings.Contains(body, "Monitoring started at") {
			t.Errorf("no partial-week note:\n%s", body)
		}
	}
	// The unmonitored days are blank in the graphs, not drawn as a gap.
	if strings.Contains(html, barGap) {
		t.Errorf("a fresh install's graphs show a gap")
	}
}

// Gaps are drawn full height in the gap colour, so they cannot be read as
// a quiet stretch.
func TestChartsShowGaps(t *testing.T) {
	month := []statsRecord{{Created: pbTime{testFrom.Add(-24 * time.Hour)}, Stats: calm()}}
	all := week(84, calm())
	s := summarize(testSystem(), append(append([]statsRecord{}, all[:30]...), all[40:]...), month, nil, testFrom, testTo)
	v := newView(s, evaluate(s, time.UTC), "", time.UTC)
	if len(v.Charts) != 2 {
		t.Fatalf("want two charts, got %d", len(v.Charts))
	}
	var gaps int
	for _, b := range v.Charts[0].Bars {
		if b.Color == barGap {
			gaps++
		}
	}
	if gaps != 10 {
		t.Fatalf("want 10 gap bars, got %d", gaps)
	}
	if !strings.Contains(v.Charts[0].Note, "no data") {
		t.Errorf("note should explain the gap colour: %q", v.Charts[0].Note)
	}
}

// The coverage finding has to say whether the NAS was off or only the
// monitoring was, because the fixes differ. The agent's uptime decides it.
func TestGapCauseFromUptime(t *testing.T) {
	month := []statsRecord{{Created: pbTime{testFrom.Add(-24 * time.Hour)}, Stats: calm()}}
	// 84 windows with 20 hours missing from the middle.
	all := week(84, calm())
	samples := append(append([]statsRecord{}, all[:30]...), all[40:]...)
	gapEnd := all[40].Created.Add(-sampleWindow)

	cases := []struct {
		name string
		boot time.Time
		want string
	}{
		{"rebooted in the gap", gapEnd.Add(-10 * time.Minute), "The NAS booted at"},
		{"up throughout", testFrom.Add(-40 * 24 * time.Hour), "it stayed on and the monitoring stack stopped"},
		{"no uptime", time.Time{}, "cannot tell"},
	}
	for _, c := range cases {
		sys := testSystem()
		if !c.boot.IsZero() {
			sys.Updated = pbTime{testTo}
			sys.Info.Uptime = uint64(testTo.Sub(c.boot).Seconds())
		}
		s := summarize(sys, samples, month, nil, testFrom, testTo)
		var f *finding
		for _, x := range evaluate(s, time.UTC) {
			if x.Area == "monitoring gaps" {
				f = &x
			}
		}
		if f == nil {
			t.Errorf("%s: no coverage finding", c.name)
			continue
		}
		if !strings.Contains(f.What, c.want) {
			t.Errorf("%s: want %q in %q", c.name, c.want, f.What)
		}
		if !strings.Contains(f.Title, "20 hours") {
			t.Errorf("%s: title %q should give the gap's length", c.name, f.Title)
		}
	}
}

func TestAgentDownIsAct(t *testing.T) {
	sys := testSystem()
	sys.Status = "down"
	fs := evaluate(summarize(sys, week(84, calm()), nil, nil, testFrom, testTo), time.UTC)
	if got := areas(fs)["agent"]; got != levelAct {
		t.Fatalf("agent down: got %v, want ACT", got)
	}
}

func TestEveryFindingSaysWhatWhyAndDo(t *testing.T) {
	st := calm()
	st.MemPct, st.MaxMem, st.CPU, st.SwapUsed = 90, 15.8, 80, 3.9
	st.LoadAvg = [3]float64{0, 0, 5}
	st.CPUBreak = []float64{40, 10, 25, 0, 25}
	st.Temps = map[string]float64{"coretemp_package_id_0": 85}
	st.DiskUsed = 95
	sys := testSystem()
	sys.Status = "down"
	ctrs := []containerRecord{{Stats: []containerSample{{"vault-claude", 30, 6000}}}}
	s := summarize(sys, week(50, st), []statsRecord{{Created: pbTime{testFrom.Add(-time.Hour)}, Stats: calm()}}, ctrs, testFrom, testTo)
	fs := evaluate(s, time.UTC)
	if len(fs) < 8 {
		t.Fatalf("want a finding per area, got %d: %+v", len(fs), fs)
	}
	for _, f := range fs {
		if f.Area == "" || f.Title == "" || f.What == "" || f.Why == "" || f.Do == "" {
			t.Errorf("incomplete finding: %+v", f)
		}
	}
}

// The samples in testdata are what the email looks like: open the .html in a
// browser to review a layout change. They are also golden files, so a change
// to the wording or layout shows up in review as a diff to them. Rewrite them
// with `go test -run TestSamples -update`.
func TestSamples(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	for name, s := range map[string]summary{
		"partial-week": samplePartialWeek(ny),
		"rough-week":   sampleRoughWeek(ny),
	} {
		subject, text, html := render(s, evaluate(s, ny), "https://kyles-nas.example.ts.net:8443/", ny)
		files := map[string]string{
			"sample-" + name + ".txt":  "Subject: " + subject + "\n\n" + text,
			"sample-" + name + ".html": html,
		}
		for file, got := range files {
			path := filepath.Join("testdata", file)
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -run TestSamples -update)", err)
			}
			if string(want) != got {
				t.Errorf("%s differs from the renderer's output; run go test -run TestSamples -update and review the diff", path)
			}
		}
	}
}

// sampleContainers builds n records of the given containers, for two-hour
// windows ending at to.
func sampleContainers(n int, to time.Time, ctrs []containerSample) []containerRecord {
	out := make([]containerRecord, n)
	for i := range out {
		out[i] = containerRecord{Created: pbTime{to.Add(-time.Duration(n-i-1) * sampleWindow)}, Stats: ctrs}
	}
	return out
}

func nasSystem() systemRecord {
	s := testSystem()
	s.Status = "up"
	return s
}

// samplePartialWeek is the week of 2 November 2026, when monitoring was
// installed on the Friday: the numbers in the first email Kyle received.
func samplePartialWeek(loc *time.Location) summary {
	to := time.Date(2026, 11, 2, 8, 0, 0, 0, loc)
	from := to.AddDate(0, 0, -7)
	base := sysStats{
		CPU: 12, MaxCPU: 30, MemTotal: 15.5, MemUsed: 5.1, MaxMem: 5.2, MemPct: 33,
		Swap: 22.9, SwapUsed: 0, DiskSize: 49, DiskUsed: 12.25,
		Temps: map[string]float64{"coretemp_package_id_0": 58}, LoadAvg: [3]float64{1.8, 1.8, 1.79},
		CPUBreak: []float64{8, 3, 1.2, 0, 87.8},
		ExtraFs:  map[string]fsStats{"data": {Size: 99, Used: 5.9}},
	}
	busy := base
	busy.MaxCPU, busy.MaxMem, busy.LoadAvg = 42, 5.27, [3]float64{2.4, 2.4, 2.42}
	busy.Temps = map[string]float64{"coretemp_package_id_0": 61}
	var wk []statsRecord
	for i := 0; i < 34; i++ {
		st := base
		if i == 20 {
			st = busy
		}
		st.CPU = 12 + 6*math.Sin(float64(i)*math.Pi/6) // a daily cycle
		st.MemPct = 33 + math.Sin(float64(i)*math.Pi/6)
		wk = append(wk, statsRecord{Created: pbTime{to.Add(-time.Duration(34-i) * sampleWindow)}, Stats: st})
	}
	ctrs := []containerSample{
		{"home-assistant", 0.3, 524}, {"vault-claude", 0.2, 182}, {"matter-server", 0.0, 110},
		{"vault-research", 0.1, 69}, {"vault-sync", 0.0, 58}, {"beszel", 0.4, 41}, {"beszel-agent", 0.2, 22},
	}
	sys := nasSystem()
	sys.Updated = pbTime{to}
	sys.Info.Uptime = uint64((40 * 24 * time.Hour).Seconds())
	return summarize(sys, wk, nil, sampleContainers(34, to, ctrs), from, to)
}

// sampleRoughWeek is invented to show every part of the layout at once: an
// ACT, a reboot inside a gap, a climbing trend and a disk filling up.
func sampleRoughWeek(loc *time.Location) summary {
	to := time.Date(2026, 11, 2, 8, 0, 0, 0, loc)
	from := to.AddDate(0, 0, -7)
	base := sysStats{
		CPU: 31, MaxCPU: 88, MemTotal: 15.5, MemUsed: 12.2, MaxMem: 14.9, MemPct: 79,
		Swap: 22.9, SwapUsed: 2.1, DiskSize: 49, DiskUsed: 21,
		Temps: map[string]float64{"coretemp_package_id_0": 66}, LoadAvg: [3]float64{2.1, 2.2, 2.3},
		CPUBreak: []float64{20, 6, 4.1, 0, 69.9},
		ExtraFs:  map[string]fsStats{"data": {Size: 3600, Used: 2700}},
	}
	var wk []statsRecord
	n := int(to.Sub(from) / sampleWindow)
	for i := 0; i < n; i++ {
		if i >= 18 && i < 28 { // no samples Tue 27 Oct 20:00 to Wed 28 Oct 16:00
			continue
		}
		st := base
		st.CPU = 31 + 18*math.Sin(float64(i)*math.Pi/6) // busy afternoons, quiet nights
		st.MemPct = 79 + 7*math.Sin(float64(i)*math.Pi/6)
		wk = append(wk, statsRecord{Created: pbTime{from.Add(time.Duration(i+1) * sampleWindow)}, Stats: st})
	}
	prior := base
	prior.CPU, prior.MemPct = 22, 58
	prior.ExtraFs = map[string]fsStats{"data": {Size: 3600, Used: 2400}}
	var month []statsRecord
	for d := 30; d >= 0; d-- {
		st := prior
		if d < 7 {
			st = base
		}
		month = append(month, statsRecord{Created: pbTime{to.Add(-time.Duration(d) * 24 * time.Hour)}, Stats: st})
	}
	ctrs := []containerSample{
		{"vault-claude", 9.4, 4710}, {"vault-research", 6.1, 2880}, {"home-assistant", 1.2, 610},
		{"vault-sync", 0.4, 240}, {"matter-server", 0.1, 118}, {"beszel", 0.4, 44}, {"beszel-agent", 0.3, 25},
	}
	sys := nasSystem()
	sys.Updated = pbTime{to}
	boot := from.Add(28*sampleWindow - 25*time.Minute)
	sys.Info.Uptime = uint64(to.Sub(boot).Seconds())
	return summarize(sys, wk, month, sampleContainers(n-10, to, ctrs), from, to)
}
