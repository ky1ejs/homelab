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
	// The NAS: what every test judges by unless it says otherwise.
	testHost = nasHost("kyles-nas")
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
	if fs := evaluate(s, testHost, time.UTC); len(fs) != 0 {
		t.Fatalf("want no findings, got %+v", fs)
	}
	subject, _, _ := mustRender(t, s, nil, "", time.UTC)
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
		got := areas(evaluate(summarize(testSystem(), week(84, st), nil, nil, testFrom, testTo), testHost, time.UTC))["memory"]
		if got != c.want {
			t.Errorf("avg %.0f peak %.1f GB: got %v, want %v", c.avg, c.peakGB, got, c.want)
		}
	}
}

func TestLoadIsJudgedPerThread(t *testing.T) {
	st := calm()
	st.LoadAvg = [3]float64{0, 0, 3.0} // 0.75 per thread on 4 threads
	if got := areas(evaluate(summarize(testSystem(), week(84, st), nil, nil, testFrom, testTo), testHost, time.UTC))["load"]; got != levelWatch {
		t.Fatalf("load 3.0 on 4 threads: got %v, want WATCH", got)
	}
	big := testSystem()
	big.Info.Threads = 16
	if got := areas(evaluate(summarize(big, week(84, st), nil, nil, testFrom, testTo), testHost, time.UTC))["load"]; got != levelOK {
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
	if got := areas(evaluate(s, testHost, time.UTC))["disk data"]; got != levelAct {
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
	_, body, _ := mustRender(t, s, evaluate(s, testHost, time.UTC), "", time.UTC)
	if !strings.Contains(body, "growth needs 3 days of history") {
		t.Fatalf("body should say growth is unknown:\n%s", body)
	}
}

func TestMissingSamplesAreReported(t *testing.T) {
	// A record from before the week: the hub existed throughout, so the
	// missing first two days are an outage, not a fresh install.
	month := []statsRecord{{Created: pbTime{testFrom.Add(-24 * time.Hour)}, Stats: calm()}}
	s := summarize(testSystem(), week(60, calm()), month, nil, testFrom, testTo)
	if got := areas(evaluate(s, testHost, time.UTC))["monitoring gaps"]; got != levelWatch {
		t.Fatalf("60 of 84 samples: got %v, want WATCH", got)
	}
	if len(s.Gaps) != 1 || !s.Gaps[0].From.Equal(testFrom) || !s.Gaps[0].To.Equal(testTo.Add(-122*time.Hour)) {
		t.Fatalf("gaps: %+v", s.Gaps)
	}
	empty := summarize(testSystem(), nil, nil, nil, testFrom, testTo)
	if got := evaluate(empty, testHost, time.UTC); len(got) != 1 || got[0].Level != levelAct {
		t.Fatalf("no samples at all should be one ACT, got %+v", got)
	}
	if _, body, _ := mustRender(t, empty, evaluate(empty, testHost, time.UTC), "", time.UTC); strings.Contains(body, "CPU:") {
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
	if got := areas(evaluate(s, testHost, time.UTC))["memory trend"]; got != levelWatch {
		t.Fatalf("12-point rise: got %v, want WATCH", got)
	}
}

func TestContainersRankByMemory(t *testing.T) {
	recs := []containerRecord{
		{Stats: []containerSample{{"vault-claude", 4, 2000}, {"beszel", 1, 30}, {"home-assistant", 2, 600}}},
		{Stats: []containerSample{{"vault-claude", 6, 3000}, {"home-assistant", 2, 700}}},
	}
	got, more, top := summarizeContainers(recs, 2)
	if more != 1 || len(got) != 2 || got[0].Name != "vault-claude" || got[1].Name != "home-assistant" {
		t.Fatalf("ranking wrong: %+v, %d more", got, more)
	}
	if top.Name != "vault-claude" || top.CPUAvg != 5 {
		t.Fatalf("busiest by CPU: %+v", top)
	}
	if got[0].MemAvgMB != 2500 || got[0].MemBusiestMB != 3000 || got[0].CPUAvg != 5 {
		t.Fatalf("averages wrong: %+v", got[0])
	}
}

func TestActSortsBeforeWatch(t *testing.T) {
	st := calm()
	st.CPU = 55    // WATCH
	st.MemPct = 90 // ACT
	fs := evaluate(summarize(testSystem(), week(84, st), nil, nil, testFrom, testTo), testHost, time.UTC)
	if len(fs) < 2 || fs[0].Level != levelAct {
		t.Fatalf("ACT should lead: %+v", fs)
	}
	if got := subject(testHost, fs); got != "[homelab] weekly: act on memory; watch cpu" {
		t.Fatalf("subject %q", got)
	}
	if got := headline(fs); got != "1 thing to act on and 1 to keep an eye on." {
		t.Fatalf("headline %q", got)
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
	subject, body, html := mustRender(t, s, evaluate(s, testHost, london), "https://kyles-nas.tail3df177.ts.net:8443/", london)
	failure, fbody := failureReport(testHost, errors.New("dial tcp: connection refused"), "x")
	for _, text := range []string{subject, body, html, failure, fbody} {
		for i, r := range text {
			if r > 127 {
				t.Fatalf("non-ASCII %q at byte %d in:\n%s", r, i, text)
			}
		}
		// 7bit allows 998 octets a line; leave room for the CRLF.
		for _, line := range strings.Split(text, "\n") {
			if len(line) > 900 {
				t.Fatalf("line of %d bytes: %.80s...", len(line), line)
			}
		}
	}
	if html == "" || !strings.Contains(html, "<table") {
		t.Fatalf("no HTML part:\n%s", html)
	}
	for _, want := range []string{"ACT", "Memory", "a-very-long-container-name-indeed", "Dashboard: https://"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// A filesystem the agent stops reporting is dropped from its samples, so a
// report that listed only what is present would read "all clear" with the data
// volume gone.
func TestVanishedDiskIsAct(t *testing.T) {
	month := []statsRecord{{Created: pbTime{testTo.Add(-20 * 24 * time.Hour)}, Stats: calm()}}
	st := calm()
	st.ExtraFs = nil
	s := summarize(testSystem(), week(84, st), month, nil, testFrom, testTo)
	if len(s.MissingDisks) != 1 || s.MissingDisks[0].Name != "data" {
		t.Fatalf("missing disks: %+v", s.MissingDisks)
	}
	if got := areas(evaluate(s, testHost, time.UTC))["disk data"]; got != levelAct {
		t.Fatalf("vanished data volume: got %v, want ACT", got)
	}
	subject, body, _ := mustRender(t, s, evaluate(s, testHost, time.UTC), "", time.UTC)
	if strings.Contains(subject, "all clear") || !strings.Contains(body, "not reported since") {
		t.Fatalf("subject %q, body:\n%s", subject, body)
	}
}

// The hub averages a filesystem over every sample in a rollup window, present
// or not, so a window it was half-present for reports half its size and half
// its usage. That record must not be the growth baseline: here it would read
// as 32 GB/day of growth against a true 1 GB/day.
func TestDeflatedRollupIsNotAGrowthBaseline(t *testing.T) {
	half := calm()
	half.ExtraFs = map[string]fsStats{"data": {Size: 1800, Used: 900}}
	whole := calm()
	whole.ExtraFs = map[string]fsStats{"data": {Size: 3600, Used: 1800}}
	month := []statsRecord{
		{Created: pbTime{testTo.Add(-30 * 24 * time.Hour)}, Stats: half},
		{Created: pbTime{testTo.Add(-29 * 24 * time.Hour)}, Stats: whole},
	}
	st := calm()
	st.ExtraFs = map[string]fsStats{"data": {Size: 3600, Used: 1829}}
	s := summarize(testSystem(), week(84, st), month, nil, testFrom, testTo)
	for _, d := range s.Disks {
		if d.Name == "data" && (d.GrowthGBPD > 1.1 || d.GrowthGBPD < 0.9) {
			t.Fatalf("growth %.1f GB/day from a deflated baseline, want about 1.0", d.GrowthGBPD)
		}
	}
	if got := areas(evaluate(s, testHost, time.UTC))["disk data"]; got != levelOK {
		t.Fatalf("1 GB/day on a half-empty volume: got %v, want OK", got)
	}
}

// In a DST week the two ends of the window are in different zones, and the
// header must say so rather than label both with the zone of the end.
func TestHeaderZonePerEnd(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	to := time.Date(2026, 10, 26, 8, 0, 0, 0, london)
	from := to.AddDate(0, 0, -7)
	_, body, _ := mustRender(t, summarize(testSystem(), nil, nil, nil, from, to), nil, "", london)
	if !strings.Contains(body, "Mon 19 Oct 08:00 BST to Mon 26 Oct 08:00 GMT") {
		t.Fatalf("header wrong:\n%s", body)
	}
}

func macSystem() systemRecord {
	var s systemRecord
	s.ID, s.Name, s.Status = "mac", "mac-studio", "up"
	s.Info.Threads, s.Info.CPUModel = 18, "Apple M5 Max"
	return s
}

func macCalm() sysStats {
	return sysStats{
		CPU: 10, MaxCPU: 60, MemTotal: 64, MemUsed: 40, MaxMem: 50, MemPct: 62,
		Swap: 2, SwapUsed: 0.2, DiskSize: 926, DiskUsed: 400,
		Temps: map[string]float64{"pacc_mtr_temp_sensor0": 70}, LoadAvg: [3]float64{2, 2, 2},
		CPUBreak: []float64{6, 3, 0, 0, 91},
	}
}

func macFindings(st sysStats) map[string]level {
	sys := macSystem()
	sys.Updated = pbTime{testTo}
	s := summarize(sys, week(84, st), nil, nil, testFrom, testTo)
	return areas(evaluate(s, macStudioHost, time.UTC))
}

// macOS keeps memory full of cache, so a percent that would be ACT on the NAS
// says nothing on the mac studio.
func TestMacStudioMemoryPercentIsNotJudged(t *testing.T) {
	st := macCalm()
	st.MemPct, st.MaxMem = 97, 63.9
	if got := macFindings(st); len(got) != 0 {
		t.Errorf("findings %v, want none", got)
	}
}

// Swap is judged in GB on the mac studio: macOS grows its swap files, so
// 3 GB of a 4 GB file is calm and 9 GB is not, whatever the percent.
func TestMacStudioSwapIsJudgedInGB(t *testing.T) {
	for _, tc := range []struct {
		total, used float64
		want        level
	}{
		{4, 3, levelOK},
		{6, 5, levelWatch},
		{10, 9, levelAct},
	} {
		st := macCalm()
		st.Swap, st.SwapUsed = tc.total, tc.used
		if got := macFindings(st)["swap"]; got != tc.want {
			t.Errorf("%g of %g GB: %v, want %v", tc.used, tc.total, got, tc.want)
		}
	}
}

// Apple silicon runs its dies near 100 C by design, and macOS has no iowait.
func TestMacStudioIgnoresTemperatureAndIOWait(t *testing.T) {
	st := macCalm()
	st.Temps = map[string]float64{"pacc_mtr_temp_sensor0": 104}
	st.CPUBreak = []float64{6, 3, 40, 0, 51}
	if got := macFindings(st); len(got) != 0 {
		t.Errorf("findings %v, want none", got)
	}
}

// What the mac studio does share with the NAS: CPU, load and disk lines.
func TestMacStudioJudgesCPULoadAndDisk(t *testing.T) {
	st := macCalm()
	st.CPU, st.LoadAvg, st.DiskUsed = 75, [3]float64{20, 20, 20}, 850
	got := macFindings(st)
	for _, area := range []string{"cpu", "load", "disk root"} {
		if got[area] != levelAct {
			t.Errorf("%s: %v, want act (all findings %v)", area, got[area], got)
		}
	}
}
