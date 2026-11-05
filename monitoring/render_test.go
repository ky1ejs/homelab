package main

import (
	"bytes"
	"flag"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the golden samples need America/New_York on any host
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
	_, text, html := mustRender(t, s, nil, "", time.UTC)
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

// A gap after monitoring started is drawn as a gap even in a partial week.
// This is the other half of the fresh-install test above: with no gap the
// graphs have no gap bars, with one they do.
func TestPartialWeekShowsLaterGaps(t *testing.T) {
	all := week(40, calm())
	s := summarize(testSystem(), append(append([]statsRecord{}, all[:10]...), all[20:]...), nil, nil, testFrom, testTo)
	if s.HistoryStart.IsZero() || len(s.Gaps) != 1 {
		t.Fatalf("want a partial week with one gap, got start %v gaps %v", s.HistoryStart, s.Gaps)
	}
	v := newView(s, evaluate(s, time.UTC), "", time.UTC)
	var gaps, blank int
	for _, b := range v.Charts[0].Bars {
		switch b.Color {
		case barGap:
			gaps++
		case "":
			blank++
		}
	}
	if gaps != 10 {
		t.Errorf("want 10 gap bars, got %d", gaps)
	}
	if blank != 84-40 {
		t.Errorf("want %d blank bars before monitoring started, got %d", 84-40, blank)
	}
}

// The hub writes its records on its own two-hour clock, so their phase
// against the report's window can be anything. Whatever it is, a full week
// fills every slot once, give or take the minute the hub runs late.
func TestSlotsFollowTheHubsPhase(t *testing.T) {
	month := []statsRecord{{Created: pbTime{testFrom.Add(-24 * time.Hour)}, Stats: calm()}}
	for _, phase := range []time.Duration{2 * time.Second, 30 * time.Minute, 70 * time.Minute, 119 * time.Minute, sampleWindow} {
		var wk []statsRecord
		for k := 0; ; k++ {
			at := testFrom.Add(phase + time.Duration(k)*sampleWindow)
			if !at.Before(testTo) && phase != sampleWindow || at.After(testTo) {
				break
			}
			jitter := time.Duration(k%3) * 40 * time.Second
			wk = append(wk, statsRecord{Created: pbTime{at.Add(jitter)}, Stats: calm()})
		}
		s := summarize(testSystem(), wk, month, nil, testFrom, testTo)
		if len(s.Slots) != len(wk) {
			t.Errorf("phase %v: %d slots for %d records", phase, len(s.Slots), len(wk))
			continue
		}
		for i, x := range s.Slots {
			if !x.Has {
				t.Errorf("phase %v: slot %d of %d is empty", phase, i, len(s.Slots))
			}
		}
	}
}

// The day labels follow the reader's calendar days, not 24-hour blocks from
// the report time, and their spans cover every bar.
func TestDayLabelsFollowMidnight(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	s := sampleRoughWeek(ny)
	v := newView(s, evaluate(s, ny), "", ny)
	c := v.Charts[0]
	i := 0
	for n, d := range c.Days {
		start := s.Slots[i].End.Add(-sampleWindow).In(ny)
		// Two-hour slots meet midnight exactly unless a DST change has
		// shifted them by an hour.
		if n > 0 && start.Hour() >= 2 {
			t.Errorf("day %d (%q) starts at %s, want the first slot after midnight", n, d.Label, start)
		}
		if d.Label != "" && d.Label != start.Format("Mon") {
			t.Errorf("day %d labelled %q, starts on %s", n, d.Label, start.Format("Mon"))
		}
		i += d.Span
	}
	if i != len(c.Bars) {
		t.Errorf("day spans cover %d bars of %d", i, len(c.Bars))
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
		name   string
		status string
		boot   time.Time
		want   string
	}{
		{"rebooted in the gap", "up", gapEnd.Add(-10 * time.Minute), "The NAS booted at"},
		{"up throughout", "up", testFrom.Add(-40 * 24 * time.Hour), "it stayed on and the monitoring stack stopped"},
		{"no uptime", "up", time.Time{}, "cannot tell"},
		// Marking a system down saves the status alone: `updated` moves on
		// and the uptime is the one from before the outage, so the boot time
		// it gives is weeks too early and must not be used.
		{"agent down, stale uptime", "down", testFrom.Add(-40 * 24 * time.Hour), "cannot tell"},
	}
	for _, c := range cases {
		sys := testSystem()
		sys.Status = c.status
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
		t.Fatal(err)
	}
	for name, s := range map[string]summary{
		"partial-week": samplePartialWeek(ny),
		"rough-week":   sampleRoughWeek(ny),
	} {
		subject, text, html := mustRender(t, s, evaluate(s, ny), "https://kyles-nas.example.ts.net:8443/", ny)
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

func mustRender(t *testing.T, s summary, found []finding, dashboardURL string, loc *time.Location) (subject, text, html string) {
	t.Helper()
	subject, text, html, err := render(s, found, dashboardURL, loc)
	if err != nil {
		t.Fatal(err)
	}
	return subject, text, html
}

// The gap list names the longest gaps, even when shorter ones came first.
func TestGapListNamesTheLongest(t *testing.T) {
	month := []statsRecord{{Created: pbTime{testFrom.Add(-24 * time.Hour)}, Stats: calm()}}
	all := week(84, calm())
	var wk []statsRecord
	for i, r := range all {
		if (i >= 5 && i < 7) || (i >= 15 && i < 17) || (i >= 25 && i < 27) || (i >= 40 && i < 70) {
			continue
		}
		wk = append(wk, r)
	}
	s := summarize(testSystem(), wk, month, nil, testFrom, testTo)
	if len(s.Gaps) != 4 {
		t.Fatalf("want 4 gaps, got %v", s.Gaps)
	}
	f := coverageFinding(s, float64(s.Samples)/float64(s.Expected), func(t time.Time) string { return t.UTC().Format(stamp) })
	long := s.Gaps[3]
	if !strings.Contains(f.What, long.From.UTC().Format(stamp)+" to "+long.To.UTC().Format(stamp)) {
		t.Errorf("the 60-hour gap is not listed: %q", f.What)
	}
	if !strings.Contains(f.What, "and 1 shorter gap.") {
		t.Errorf("want the fourth gap counted as shorter: %q", f.What)
	}
}

// Scattered misses with no gap long enough to name get advice that does not
// refer to a gap.
func TestCoverageWithoutAGap(t *testing.T) {
	month := []statsRecord{{Created: pbTime{testFrom.Add(-24 * time.Hour)}, Stats: calm()}}
	var wk []statsRecord
	for i, r := range week(84, calm()) {
		if i%5 != 4 {
			wk = append(wk, r)
		}
	}
	s := summarize(testSystem(), wk, month, nil, testFrom, testTo)
	var f *finding
	for _, x := range evaluate(s, time.UTC) {
		if x.Area == "monitoring gaps" {
			f = &x
		}
	}
	if f == nil || len(s.Gaps) != 0 {
		t.Fatalf("want a coverage finding with no gaps, got %v and gaps %v", f, s.Gaps)
	}
	if strings.Contains(f.Do, "the gap") || strings.Contains(f.What, "cannot tell") {
		t.Errorf("advice refers to a gap that does not exist: %+v", f)
	}
}

// A partial week's coverage is of the time since monitoring started.
func TestPartialWeekCoverageSaysSince(t *testing.T) {
	var wk []statsRecord
	for i, r := range week(40, calm()) {
		if i%5 != 4 {
			wk = append(wk, r)
		}
	}
	s := summarize(testSystem(), wk, nil, nil, testFrom, testTo)
	for _, f := range evaluate(s, time.UTC) {
		if f.Area == "monitoring gaps" && !strings.Contains(f.Why, "since monitoring started") {
			t.Errorf("coverage reads as the whole week: %q", f.Why)
		}
	}
}

// An agent that was down all week is named, not taken for a fresh install.
func TestAgentDownWithNoData(t *testing.T) {
	sys := testSystem()
	sys.Status = "down"
	fs := evaluate(summarize(sys, nil, nil, nil, testFrom, testTo), time.UTC)
	if len(fs) != 1 || fs[0].Area != "agent" || fs[0].Level != levelAct {
		t.Fatalf("want one ACT agent finding, got %+v", fs)
	}
	if strings.Contains(fs[0].Do, "wait") {
		t.Errorf("tells the reader to wait for an agent that is down: %q", fs[0].Do)
	}
}

// The CPU finding names the container that used the most CPU over the week,
// not one that ran hot for a single window.
func TestTopCPUIsOverTheWeek(t *testing.T) {
	st := calm()
	st.CPU = 80
	recs := sampleContainers(84, testTo, []containerSample{{"steady", 30, 100}})
	recs[0].Stats = append([]containerSample{{"one-off", 60, 50}}, recs[0].Stats...)
	s := summarize(testSystem(), week(84, st), nil, recs, testFrom, testTo)
	if s.TopCPU.Name != "steady" {
		t.Fatalf("top CPU %q, want steady", s.TopCPU.Name)
	}
	for _, f := range evaluate(s, time.UTC) {
		if f.Area == "cpu" && !strings.Contains(f.Do, "steady used the most CPU this week (30.0%") {
			t.Errorf("cpu advice: %q", f.Do)
		}
	}
}

// Names from the hub need not be ASCII. On the wire the message still is,
// and both parts and the subject decode back to the names.
func TestNonASCIINamesSurviveTheWire(t *testing.T) {
	sys := testSystem()
	sys.Status = "up"
	sys.Name = "Kyle’s NAS"
	st := calm()
	st.ExtraFs = map[string]fsStats{"données": {Size: 100, Used: 95}}
	s := summarize(sys, week(84, st), []statsRecord{{Created: pbTime{testFrom.Add(-time.Hour)}, Stats: st}}, nil, testFrom, testTo)
	subject, text, html := mustRender(t, s, evaluate(s, time.UTC), "", time.UTC)
	if !strings.Contains(subject, "données") || !strings.Contains(text, "Kyle’s NAS") {
		t.Fatalf("fixture does not carry the names: %q", subject)
	}
	if !isASCII(html) || !strings.Contains(html, "Kyle&#8217;s NAS") {
		t.Errorf("HTML part is not ASCII with entities")
	}
	raw, err := buildMessage(testMail(), email{Subject: subject, Text: text, HTML: html}, testTo)
	if err != nil {
		t.Fatal(err)
	}
	if !isASCII(string(raw)) {
		t.Fatal("message has 8-bit bytes")
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); err != nil || got != subject {
		t.Errorf("subject decodes to %q (%v), want %q", got, err, subject)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	p, err := multipart.NewReader(msg.Body, params["boundary"]).NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if ct := p.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("text part content type %q", ct)
	}
	body, _ := io.ReadAll(p)
	// The CRLF before the boundary belongs to the boundary (RFC 2046).
	if got := strings.ReplaceAll(string(body), "\r\n", "\n"); got != strings.TrimSuffix(text, "\n") {
		t.Errorf("text part decodes to:\n%s\nwant:\n%s", got, text)
	}
}
