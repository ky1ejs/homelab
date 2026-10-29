package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	testTo   = time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	testFrom = testTo.Add(-7 * 24 * time.Hour)
)

func testSystem() systemRecord {
	var s systemRecord
	s.ID, s.Name = "abc", "kyles-nas"
	s.Info.Threads, s.Info.CPUModel = 4, "Intel(R) Celeron(R) J4125"
	return s
}

// week builds n two-hour samples ending at testTo, each a copy of st.
func week(n int, st sysStats) []statsRecord {
	out := make([]statsRecord, n)
	for i := range out {
		out[i] = statsRecord{Created: pbTime{testTo.Add(-time.Duration(n-i) * sampleWindow)}, Stats: st}
	}
	return out
}

func calm() sysStats {
	return sysStats{
		CPU: 10, MaxCPU: 60, MemTotal: 16, MemUsed: 8, MaxMem: 10, MemPct: 50,
		Swap: 4, SwapUsed: 0.1, DiskSize: 100, DiskUsed: 40,
		Temps: map[string]float64{"cpu": 50}, LoadAvg: [3]float64{0.5, 0.6, 0.7},
		CPUBreak: []float64{6, 2, 1, 0, 91},
		ExtraFs:  map[string]fsStats{"data": {Size: 3600, Used: 1800}},
	}
}

func areas(fs []finding) map[string]level {
	m := map[string]level{}
	for _, f := range fs {
		if f.Level > m[f.Area] {
			m[f.Area] = f.Level
		}
	}
	return m
}

func TestCalmWeekIsAllClear(t *testing.T) {
	s := summarize(testSystem(), week(84, calm()), nil, nil, testFrom, testTo)
	if fs := evaluate(s); len(fs) != 0 {
		t.Fatalf("want no findings, got %+v", fs)
	}
	subject, _ := render(s, nil, "", time.UTC)
	if subject != "[homelab] weekly: all clear" {
		t.Fatalf("subject %q", subject)
	}
}

func TestMemoryThresholds(t *testing.T) {
	cases := []struct {
		avg, peakGB float64
		want        level
	}{
		{70, 12, levelOK},      // 75% peak
		{76, 12, levelWatch},   // average over 75
		{70, 14.6, levelWatch}, // peak 91%
		{86, 12, levelAct},     // average over 85
		{70, 15.4, levelAct},   // peak 96%
	}
	for _, c := range cases {
		st := calm()
		st.MemPct, st.MaxMem = c.avg, c.peakGB
		got := areas(evaluate(summarize(testSystem(), week(84, st), nil, nil, testFrom, testTo)))["memory"]
		if got != c.want {
			t.Errorf("avg %.0f peak %.1f GB: got %v, want %v", c.avg, c.peakGB, got, c.want)
		}
	}
}

func TestLoadIsJudgedPerThread(t *testing.T) {
	st := calm()
	st.LoadAvg = [3]float64{0, 0, 3.0} // 0.75 per thread on 4 threads
	if got := areas(evaluate(summarize(testSystem(), week(84, st), nil, nil, testFrom, testTo)))["load"]; got != levelWatch {
		t.Fatalf("load 3.0 on 4 threads: got %v, want WATCH", got)
	}
	big := testSystem()
	big.Info.Threads = 16
	if got := areas(evaluate(summarize(big, week(84, st), nil, nil, testFrom, testTo)))["load"]; got != levelOK {
		t.Fatalf("load 3.0 on 16 threads: got %v, want OK", got)
	}
}

func TestDiskProjectionUsesTheMonth(t *testing.T) {
	// 30 days ago the data volume had 600 GB free; now it has 300: 10 GB/day,
	// so about 30 days to full.
	month := []statsRecord{{Created: pbTime{testTo.Add(-30 * 24 * time.Hour)}, Stats: calm()}}
	month[0].Stats.ExtraFs = map[string]fsStats{"data": {Size: 3600, Used: 3000}}
	st := calm()
	st.ExtraFs = map[string]fsStats{"data": {Size: 3600, Used: 3300}}
	s := summarize(testSystem(), week(84, st), month, nil, testFrom, testTo)

	var data diskSummary
	for _, d := range s.Disks {
		if d.Name == "data" {
			data = d
		}
	}
	if !data.Measured || data.GrowthGBPD < 9.9 || data.GrowthGBPD > 10.1 || data.DaysToFull < 29 || data.DaysToFull > 31 {
		t.Fatalf("projection wrong: %+v", data)
	}
	if got := areas(evaluate(s))["disk data"]; got != levelAct {
		t.Fatalf("30 days to full: got %v, want ACT", got)
	}
}

func TestDiskWithoutHistoryIsNotProjected(t *testing.T) {
	s := summarize(testSystem(), week(2, calm()), nil, nil, testFrom, testTo)
	for _, d := range s.Disks {
		if d.Measured || d.DaysToFull != -1 {
			t.Fatalf("%s projected from four hours of data: %+v", d.Name, d)
		}
	}
	_, body := render(s, evaluate(s), "", time.UTC)
	if !strings.Contains(body, "growth unknown") {
		t.Fatalf("body should say growth is unknown:\n%s", body)
	}
}

func TestMissingSamplesAreReported(t *testing.T) {
	s := summarize(testSystem(), week(60, calm()), nil, nil, testFrom, testTo)
	if got := areas(evaluate(s))["data"]; got != levelWatch {
		t.Fatalf("60 of 84 samples: got %v, want WATCH", got)
	}
	empty := summarize(testSystem(), nil, nil, nil, testFrom, testTo)
	if got := evaluate(empty); len(got) != 1 || got[0].Level != levelAct {
		t.Fatalf("no samples at all should be one ACT, got %+v", got)
	}
	if _, body := render(empty, evaluate(empty), "", time.UTC); strings.Contains(body, "cpu average") {
		t.Fatalf("an empty week should not print a table of zeros:\n%s", body)
	}
}

func TestTrendAgainstPriorWeeks(t *testing.T) {
	prior := calm()
	prior.MemPct = 50
	month := []statsRecord{
		{Created: pbTime{testFrom.Add(-21 * 24 * time.Hour)}, Stats: prior},
		{Created: pbTime{testFrom.Add(-1 * time.Hour)}, Stats: prior},
		// Inside this week: must not count as prior.
		{Created: pbTime{testFrom.Add(time.Hour)}, Stats: sysStats{MemPct: 99}},
	}
	st := calm()
	st.MemPct = 62
	s := summarize(testSystem(), week(84, st), month, nil, testFrom, testTo)
	if s.PriorSamples != 2 || s.PriorMem != 50 {
		t.Fatalf("prior window wrong: %d samples, mem %.1f", s.PriorSamples, s.PriorMem)
	}
	if got := areas(evaluate(s))["trend"]; got != levelWatch {
		t.Fatalf("12-point rise: got %v, want WATCH", got)
	}
}

func TestContainersRankByMemory(t *testing.T) {
	recs := []containerRecord{
		{Stats: []containerSample{{"vault-claude", 4, 2000}, {"beszel", 1, 30}, {"home-assistant", 2, 600}}},
		{Stats: []containerSample{{"vault-claude", 6, 3000}, {"home-assistant", 2, 700}}},
	}
	got, more := summarizeContainers(recs, 2)
	if more != 1 || len(got) != 2 || got[0].Name != "vault-claude" || got[1].Name != "home-assistant" {
		t.Fatalf("ranking wrong: %+v, %d more", got, more)
	}
	if got[0].MemAvgMB != 2500 || got[0].MemPeakMB != 3000 || got[0].CPUAvg != 5 {
		t.Fatalf("averages wrong: %+v", got[0])
	}
}

func TestActSortsBeforeWatch(t *testing.T) {
	st := calm()
	st.CPU = 55    // WATCH
	st.MemPct = 90 // ACT
	fs := evaluate(summarize(testSystem(), week(84, st), nil, nil, testFrom, testTo))
	if len(fs) < 2 || fs[0].Level != levelAct {
		t.Fatalf("ACT should lead: %+v", fs)
	}
	if v := verdict(fs); v != "1 to act on, 1 to watch" {
		t.Fatalf("verdict %q", v)
	}
}

// The message is sent as us-ascii 7bit (mail.go). A non-ASCII byte in the
// body would make that header a lie that some gateways reject.
func TestRenderIsASCII(t *testing.T) {
	st := calm()
	st.MemPct, st.CPU = 90, 80
	st.Temps = map[string]float64{"coretemp_package_id_0": 85}
	recs := []containerRecord{{Stats: []containerSample{{"a-very-long-container-name-indeed", 1, 5000}}}}
	s := summarize(testSystem(), week(50, st), nil, recs, testFrom, testTo)
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		london = time.UTC
	}
	subject, body := render(s, evaluate(s), "https://kyles-nas.tail3df177.ts.net:8443/", london)
	failure, fbody := failureReport("kyles-nas", errors.New("dial tcp: connection refused"), "x")
	for _, text := range []string{subject, body, failure, fbody} {
		for i, r := range text {
			if r > 127 {
				t.Fatalf("non-ASCII %q at byte %d in:\n%s", r, i, text)
			}
		}
	}
	for _, want := range []string{"ACT", "memory", "a-very-long-container-n~", "Dashboard: https://"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}
