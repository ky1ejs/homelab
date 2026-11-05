package main

// Rendering the report: one view, two bodies.
//
// The email is multipart/alternative. The HTML part is what a phone or desktop
// mail client shows: a verdict banner, one card per finding, and real tables,
// so nothing depends on the reader's font being monospaced. The plain-text
// part is the fallback, and it avoids columns for the same reason -- every
// line is "label: value" and reads the same in any font.
//
// Everything this file writes is ASCII, same rule as bin/homelab. Names from
// the hub (the system, disks, sensors, containers) are whatever Beszel holds,
// so they may not be: the HTML part writes any such character as an entity
// and stays ASCII, and mail.go sends a text part that is not ASCII as UTF-8.
//
// The order is the point of the redesign. The first line is the verdict; then
// each finding says what happened, why it matters and what to do; then the
// numbers for anyone who wants them; then how to read them, last.

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

type row struct {
	Label, Value, Prior string
	Level               level
	Bar                 int // 0-100: draw a usage bar under the value; -1 for none
}

// chart is a week of two-hour averages drawn as bars. Email clients run no
// script and many drop SVG and images that are not attached, so a bar is a
// table cell with a coloured block of a set height: it draws the same in
// Gmail, Apple Mail and a phone, and keeps the module free of image code.
type chart struct {
	Title string
	Note  string
	Bars  []bar
	Days  []day
}

type bar struct {
	Height int    // px, of chartHeight
	Color  string // empty: nothing drawn (before monitoring started)
}

type day struct {
	Label string
	Span  int // slots
}

const chartHeight = 48 // px for 100%

// Bar colours. OK is a neutral blue; WATCH and ACT reuse the palette's edges.
// Gaps get a full-height pale block, so a missing stretch reads as missing,
// not as zero.
const (
	barOK  = "#60a5fa"
	barGap = "#ececef"
)

type view struct {
	Subject  string
	Headline string
	Tone     level // the worst finding's level, for the banner's colour
	System   string
	Period   string
	Partial  string // set when monitoring began inside the window
	Findings []finding
	Empty    bool // no samples at all: no table, because zeros read as a measurement

	Charts               []chart
	Rows                 []row
	PriorHeader, NoPrior string
	Containers           []containerSummary
	MoreContainers       int

	Notes     string
	Dashboard string
}

func newView(s summary, found []finding, dashboardURL string, loc *time.Location) view {
	at := func(t time.Time) string { return t.In(loc).Format(stamp) }
	v := view{
		Subject:   subject(found),
		Headline:  headline(found),
		System:    s.System,
		Period:    at(s.From) + " to " + at(s.To),
		Findings:  found,
		Empty:     s.Samples == 0,
		Dashboard: dashboardURL,
		Notes: "CPU and memory peaks are true peaks. Load, swap, temperature and container memory " +
			"are the busiest two-hour average, so a shorter spike reads lower than it was. " +
			"The thresholds are in monitoring/README.md.",
	}
	for _, f := range found {
		v.Tone = max(v.Tone, f.Level)
	}
	if !s.HistoryStart.IsZero() {
		days := s.To.Sub(s.HistoryStart).Hours() / 24
		v.Partial = fmt.Sprintf("Monitoring started at %s, so this covers %.1f of the week's 7 days. "+
			"Next Monday's report covers a full week.", at(s.HistoryStart), days)
	}
	if v.Empty {
		return v
	}

	levels := map[string]level{}
	for _, f := range found {
		levels[f.Area] = max(levels[f.Area], f.Level)
	}
	add := func(label, value, prior string, areas ...string) {
		r := row{Label: label, Value: value, Prior: prior, Bar: -1}
		for _, a := range areas {
			r.Level = max(r.Level, levels[a])
		}
		v.Rows = append(v.Rows, r)
	}
	prior := func(p float64) string {
		if s.PriorSamples == 0 {
			return ""
		}
		return pct(p)
	}

	add("CPU", fmt.Sprintf("%s average, %s peak", pct(s.CPUAvg), pct(s.CPUPeak)), prior(s.PriorCPU), "cpu", "cpu trend")
	if s.HasIOWait {
		add("iowait", fmt.Sprintf("%.1f%% average", s.IOWaitAvg), "", "iowait")
	}
	if s.Threads > 0 {
		add("Load, 15 min", fmt.Sprintf("%.2f average on %d threads, %.2f busiest", s.LoadAvg15, s.Threads, s.LoadPeak15), "", "load")
	}
	add("Memory", fmt.Sprintf("%s average, %s peak, of %.1f GB", pct(s.MemAvgPct), pct(s.MemPeakPct), s.MemTotalGB),
		prior(s.PriorMem), "memory", "memory trend")
	if s.SwapTotalGB > 0 {
		add("Swap", fmt.Sprintf("%s of %.1f GB at the busiest", pct(s.SwapPeakPct), s.SwapTotalGB), "", "swap")
	}
	for _, d := range s.Disks {
		growth := fmt.Sprintf("growth needs %.0f days of history", minGrowthDays)
		switch {
		case d.DaysToFull >= 0:
			growth = fmt.Sprintf("+%.1f GB a day, full in about %.0f days", d.GrowthGBPD, d.DaysToFull)
		case d.Measured:
			growth = "not growing"
		}
		add("Disk "+d.Name, fmt.Sprintf("%s of %s, %s", pct(d.Pct), gb(d.SizeGB), growth), "", "disk "+d.Name)
		v.Rows[len(v.Rows)-1].Bar = int(math.Round(math.Min(math.Max(d.Pct, 0), 100)))
	}
	for _, m := range s.MissingDisks {
		add("Disk "+m.Name, "not reported since "+at(m.LastSeen), "", "disk "+m.Name)
	}
	if s.HotSensor != "" {
		add("Hottest sensor", fmt.Sprintf("%s, %.0f C", sensorName(s.HotSensor), s.HotTemp), "", "temperature")
	}
	add("Samples", fmt.Sprintf("%d of %d two-hour windows", s.Samples, s.Expected), "", "monitoring gaps", "agent")

	v.Charts = []chart{
		weekChart(s, loc, fmt.Sprintf("Memory, %% of %.1f GB", s.MemTotalGB), func(x slot) float64 { return x.Mem }, memAvgWatch, memAvgAct),
		weekChart(s, loc, "CPU, % busy", func(x slot) float64 { return x.CPU }, cpuAvgWatch, cpuAvgAct),
	}

	if s.PriorSamples > 0 {
		v.PriorHeader = "Prior " + weeks(s.PriorWeeks)
	} else {
		v.NoPrior = "There are no earlier weeks to compare against yet."
	}
	v.Containers, v.MoreContainers = s.Containers, s.ContainersTruncated
	return v
}

// weekChart draws one metric across the week's slots, coloured by the same
// average thresholds the findings use, so a red bar is a two-hour stretch
// that was over the ACT line.
func weekChart(s summary, loc *time.Location, title string, value func(slot) float64, watch, act float64) chart {
	c := chart{Title: title}
	var gaps bool
	// Same rule as findGaps: one missing window is a rollup written late or
	// on the boundary, not an outage, so it is left blank rather than drawn.
	missing := func(i int) bool { return s.Slots[i].Monitored && !s.Slots[i].Has }
	for i, x := range s.Slots {
		switch {
		case !x.Monitored:
			c.Bars = append(c.Bars, bar{})
		case !x.Has:
			if (i > 0 && missing(i-1)) || (i+1 < len(s.Slots) && missing(i+1)) {
				gaps = true
				c.Bars = append(c.Bars, bar{Height: chartHeight, Color: barGap})
			} else {
				c.Bars = append(c.Bars, bar{})
			}
		default:
			v := math.Min(math.Max(value(x), 0), 100)
			col := barOK
			if l := grade(v, watch, act); l > levelOK {
				col = palettes[l].Edge
			}
			// At least 1px, so a quiet but measured window is not a blank.
			c.Bars = append(c.Bars, bar{Height: max(1, int(math.Round(v*chartHeight/100))), Color: col})
		}
	}
	// One label per calendar day in loc, spanning the slots that start on
	// it. The first and last days of the window are partial; one with under
	// eight hours of slots is left unlabelled, as there is no room for it.
	for i := 0; i < len(s.Slots); {
		start := s.Slots[i].End.Add(-sampleWindow).In(loc)
		y, m, d := start.Date()
		j := i + 1
		for j < len(s.Slots) {
			if y2, m2, d2 := s.Slots[j].End.Add(-sampleWindow).In(loc).Date(); y2 != y || m2 != m || d2 != d {
				break
			}
			j++
		}
		label := start.Format("Mon")
		if j-i < 4 {
			label = ""
		}
		c.Days = append(c.Days, day{Label: label, Span: j - i})
		i = j
	}
	c.Note = fmt.Sprintf("Two-hour averages. Amber is over %.0f%%, red over %.0f%%.", watch, act)
	if gaps {
		c.Note += " Pale grey marks hours with no data."
	}
	return c
}

// subject names the areas, not just a count, so the notification alone says
// what the week was about: "[homelab] weekly: act on memory; watch disk data".
func subject(found []finding) string {
	var acts, watches []string
	seen := map[string]bool{}
	for _, f := range found { // sorted ACT first, so an area is listed at its worst
		if seen[f.Area] {
			continue
		}
		seen[f.Area] = true
		if f.Level == levelAct {
			acts = append(acts, f.Area)
		} else {
			watches = append(watches, f.Area)
		}
	}
	var parts []string
	if len(acts) > 0 {
		parts = append(parts, "act on "+areaList(acts))
	}
	if len(watches) > 0 {
		parts = append(parts, "watch "+areaList(watches))
	}
	if len(parts) == 0 {
		return "[homelab] weekly: all clear"
	}
	return "[homelab] weekly: " + strings.Join(parts, "; ")
}

func areaList(a []string) string {
	if len(a) > 2 {
		return fmt.Sprintf("%s and %d more", a[0], len(a)-1)
	}
	return strings.Join(a, " and ")
}

// headline is the body's first line, which is also what a mail client shows
// as the preview under the subject.
func headline(found []finding) string {
	var acts, watches int
	for _, f := range found {
		if f.Level == levelAct {
			acts++
		} else {
			watches++
		}
	}
	things := func(n int) string {
		if n == 1 {
			return "1 thing"
		}
		return fmt.Sprintf("%d things", n)
	}
	switch {
	case acts == 0 && watches == 0:
		return "Nothing needs you this week."
	case watches == 0:
		return things(acts) + " to act on."
	case acts == 0:
		return things(watches) + " to keep an eye on."
	}
	return fmt.Sprintf("%s to act on and %d to keep an eye on.", things(acts), watches)
}

// render produces the subject and both bodies.
// render returns the HTML part's error separately: the text part is complete
// without it, so a caller can still send that.
func render(s summary, found []finding, dashboardURL string, loc *time.Location) (subject, text, html string, err error) {
	v := newView(s, found, dashboardURL, loc)
	html, err = renderHTML(v)
	return v.Subject, renderText(v), html, err
}

func renderText(v view) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("%s\n\n", v.Headline)
	w("%s, %s\n", v.System, v.Period)
	if v.Partial != "" {
		w("%s\n", v.Partial)
	}
	for i, f := range v.Findings {
		w("\n%d. %s: %s\n", i+1, f.Level, f.Title)
		w("   What: %s\n", f.What)
		w("   Why: %s\n", f.Why)
		w("   Do: %s\n", f.Do)
	}
	if !v.Empty {
		w("\nThis week\n")
		for _, r := range v.Rows {
			w("- %s: %s", r.Label, r.Value)
			if r.Prior != "" {
				w(" (prior %s: %s)", strings.TrimPrefix(v.PriorHeader, "Prior "), r.Prior)
			}
			if r.Level > levelOK {
				w(" [%s]", r.Level)
			}
			w("\n")
		}
		if v.NoPrior != "" {
			w("%s\n", v.NoPrior)
		}
		if len(v.Containers) > 0 {
			w("\nContainers by average memory\n")
			for _, c := range v.Containers {
				w("- %s: %s average, %s in the busiest two hours, %.1f%% CPU\n", c.Name, mb(c.MemAvgMB), mb(c.MemBusiestMB), c.CPUAvg)
			}
			if v.MoreContainers > 0 {
				w("- and %d more in the dashboard\n", v.MoreContainers)
			}
		}
		w("\n%s\n", v.Notes)
	}
	if v.Dashboard != "" {
		w("Dashboard: %s\n", v.Dashboard)
	}
	return b.String()
}

//go:embed report.html
var reportHTML string

var reportTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"code": code,
	"mb":   mb,
	"palette": func(l level) palette {
		return palettes[l]
	},
}).Parse(reportHTML))

// palette is a level's colours: a tinted background and a dark foreground,
// which pass 4.5:1 against each other. The level is also written out in
// words, so nothing depends on telling the colours apart.
type palette struct{ Bg, Fg, Edge string }

var palettes = map[level]palette{
	levelOK:    {Bg: "#e8f5ec", Fg: "#14532d", Edge: "#16a34a"},
	levelWatch: {Bg: "#fdf3dc", Fg: "#713f12", Edge: "#d97706"},
	levelAct:   {Bg: "#fde8e8", Fg: "#7f1d1d", Edge: "#dc2626"},
}

func renderHTML(v view) (string, error) {
	var b bytes.Buffer
	// html/template checks escaping when it first executes, not when it is
	// parsed, so a broken edit to report.html shows up here.
	if err := reportTemplate.Execute(&b, v); err != nil {
		return "", fmt.Errorf("rendering the HTML part: %w", err)
	}
	return asciiHTML(b.String()), nil
}

// asciiHTML writes every character above ASCII as a numeric entity, which
// HTML reads back as the same character, so the part stays us-ascii 7bit
// whatever the hub's names hold.
func asciiHTML(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > 127 {
			fmt.Fprintf(&b, "&#%d;", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// code turns `backticks` in a finding's text into <code>, escaping the rest.
func code(s string) template.HTML {
	var b strings.Builder
	for i, part := range strings.Split(s, "`") {
		esc := template.HTMLEscapeString(part)
		if i%2 == 1 {
			b.WriteString(`<code style="font-family:Menlo,Consolas,monospace;font-size:13px;background:#f1f1f3;padding:1px 4px;border-radius:3px;">` + esc + `</code>`)
		} else {
			b.WriteString(esc)
		}
	}
	return template.HTML(b.String())
}

// failureReport is what gets mailed when the report itself cannot be built.
// The mail path does not depend on the hub, so a hub that is down still
// produces an email saying so -- which is more useful than a missing one.
// Plain text only: there is nothing to lay out.
func failureReport(system string, err error, dashboardURL string) (string, string) {
	body := fmt.Sprintf("The weekly report for %s could not be built:\n\n  %v\n\n"+
		"Check the hub with `homelab status monitoring` and `homelab logs monitoring`.\n", system, err)
	if dashboardURL != "" {
		body += "Dashboard: " + dashboardURL + "\n"
	}
	return "[homelab] weekly: report failed", body
}
