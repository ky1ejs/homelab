package main

// The weekly report: what the last seven days looked like, and whether any of
// it says the box needs upgrading or the approach needs changing.
//
// Two series feed it, because Beszel keeps no single series that covers both:
//
//	120m records   kept 7 days    this week, at two-hour resolution
//	480m records   kept 30 days   the prior three weeks, and disk growth
//
// Every number is computed from averaged samples, so short spikes are
// flattened. Beszel keeps a per-window PEAK for CPU and memory, and the report
// uses it; it keeps none for temperature or load, so those peaks are "the
// hottest two hours", not "the hottest moment". The email says so.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type level int

const (
	levelOK level = iota
	levelWatch
	levelAct
)

func (l level) String() string {
	switch l {
	case levelAct:
		return "ACT"
	case levelWatch:
		return "WATCH"
	}
	return "OK"
}

// Thresholds. Constants rather than .env tunables on purpose: they are the
// judgement this report exists to apply, and a threshold quietly raised in an
// untracked file is a report that stops saying the thing it was built to say.
// Change them here, where git records why.
//
// The memory numbers are lower than they look like they should be because this
// host already overcommits: the stacks' mem_limit values add up to more than the
// RAM that is free (README.md#the-host), so "average 85%" leaves no room for both
// agents to be busy at once.
const (
	memAvgWatch, memAvgAct   = 75.0, 85.0
	memPeakWatch, memPeakAct = 90.0, 95.0
	// A little swap in use is normal - the kernel parks idle pages there. Swap
	// that stays in use at this scale means RAM is short.
	swapWatch, swapAct     = 25.0, 50.0
	cpuAvgWatch, cpuAvgAct = 50.0, 70.0
	// Load is judged against the thread count Beszel reports, so the same rule
	// holds if the box is ever replaced with a bigger one.
	loadWatchPerThread, loadActPerThread = 0.7, 1.0
	iowaitWatch, iowaitAct               = 10.0, 20.0
	diskWatch, diskAct                   = 80.0, 90.0
	// Days until a filesystem is full at its 30-day growth rate.
	fullWatchDays, fullActDays = 180.0, 60.0
	// Two-hour averages, across every sensor the agent can read (CPU package,
	// cores, and disks where the kernel exposes them).
	tempWatch, tempAct = 70.0, 80.0
	// Week-on-week rises, in percentage points against the prior three weeks.
	memRiseWatch, cpuRiseWatch = 10.0, 15.0
	// Fraction of expected two-hour samples present. Below this, something
	// stopped the agent or the hub for a meaningful part of the week.
	coverageWatch = 0.9
	// Minimum span of history before a growth rate is believed.
	minGrowthDays = 3.0
)

const sampleWindow = 2 * time.Hour // the 120m record type

type diskSummary struct {
	Name       string
	Pct        float64
	UsedGB     float64
	SizeGB     float64
	Measured   bool    // at least minGrowthDays of history to compute growth from
	GrowthGBPD float64 // GB per day over the 480m history
	DaysToFull float64 // -1 when not growing or not Measured
}

// missingDisk is a filesystem that has history but is absent from the newest
// sample. Beszel drops a filesystem whose usage it cannot read (unmounted,
// failed), so absence is the signal -- and the one a report that only listed
// what is present would never show.
type missingDisk struct {
	Name     string
	LastSeen time.Time
}

// sizeTolerance is how far a filesystem's size may differ from its current
// size for a sample to count as a growth point. The hub averages a filesystem
// over EVERY sample in a rollup window, including the ones it was absent
// from, so a window it was only partly present for reports Size and Used both
// scaled down. Taken as a starting point, one of those reads as weeks of
// growth that never happened. A real resize is rarer than that, and costs
// only a growth rate measured from the resize onwards.
const sizeTolerance = 0.01

type containerSummary struct {
	Name         string
	CPUAvg       float64
	MemAvgMB     float64
	MemBusiestMB float64 // the highest two-hour AVERAGE: Beszel keeps no container peak
}

type summary struct {
	System   string
	CPUModel string
	Threads  int
	From, To time.Time

	Samples, Expected int

	CPUAvg, CPUPeak       float64
	IOWaitAvg             float64
	HasIOWait             bool
	LoadAvg15, LoadPeak15 float64
	MemTotalGB            float64
	MemAvgPct, MemPeakPct float64
	SwapTotalGB           float64
	SwapPeakPct           float64 // the highest two-hour average; Beszel keeps no swap peak
	Disks                 []diskSummary
	MissingDisks          []missingDisk
	HotSensor             string
	HotTemp               float64

	PriorWeeks          float64 // how many weeks of prior history were found
	PriorCPU, PriorMem  float64
	PriorSamples        int
	Containers          []containerSummary
	ContainersTruncated int
}

type finding struct {
	Level   level
	Area    string
	Finding string
	Meaning string
}

// summarize reduces the raw records to the numbers the email prints. week holds
// 120m records from [from, to); month holds 480m records from the last 30 days.
func summarize(sys systemRecord, week, month []statsRecord, ctrs []containerRecord, from, to time.Time) summary {
	s := summary{
		System:   sys.Name,
		CPUModel: sys.Info.CPUModel,
		Threads:  sys.Info.Threads,
		From:     from,
		To:       to,
		// From the real duration, which is 167 or 169 hours in a DST week.
		Expected: int(to.Sub(from) / sampleWindow),
	}
	if s.Threads == 0 {
		s.Threads = sys.Info.Cores
	}

	var iowaitN int
	for _, r := range week {
		st := r.Stats
		s.Samples++
		s.CPUAvg += st.CPU
		s.CPUPeak = math.Max(s.CPUPeak, math.Max(st.MaxCPU, st.CPU))
		if len(st.CPUBreak) >= 3 {
			s.IOWaitAvg += st.CPUBreak[2]
			iowaitN++
		}
		s.LoadAvg15 += st.LoadAvg[2]
		s.LoadPeak15 = math.Max(s.LoadPeak15, st.LoadAvg[2])
		s.MemAvgPct += st.MemPct
		if st.MemTotal > 0 {
			s.MemTotalGB = st.MemTotal
			s.MemPeakPct = math.Max(s.MemPeakPct, 100*math.Max(st.MaxMem, st.MemUsed)/st.MemTotal)
		} else {
			s.MemPeakPct = math.Max(s.MemPeakPct, st.MemPct)
		}
		if st.Swap > 0 {
			s.SwapTotalGB = st.Swap
			s.SwapPeakPct = math.Max(s.SwapPeakPct, 100*st.SwapUsed/st.Swap)
		}
		for name, t := range st.Temps {
			if t > s.HotTemp {
				s.HotSensor, s.HotTemp = name, t
			}
		}
	}
	if s.Samples > 0 {
		n := float64(s.Samples)
		s.CPUAvg /= n
		s.LoadAvg15 /= n
		s.MemAvgPct /= n
	}
	if iowaitN > 0 {
		s.IOWaitAvg /= float64(iowaitN)
		s.HasIOWait = true
	}

	// The prior weeks are the 480m records that predate this week.
	var prior []statsRecord
	for _, r := range month {
		if r.Created.Before(from) {
			prior = append(prior, r)
		}
	}
	if len(prior) > 0 {
		for _, r := range prior {
			s.PriorCPU += r.Stats.CPU
			s.PriorMem += r.Stats.MemPct
		}
		s.PriorCPU /= float64(len(prior))
		s.PriorMem /= float64(len(prior))
		s.PriorSamples = len(prior)
		s.PriorWeeks = from.Sub(prior[0].Created.Time).Hours() / (24 * 7)
	}

	s.Disks, s.MissingDisks = summarizeDisks(week, month)
	s.Containers, s.ContainersTruncated = summarizeContainers(ctrs, 5)
	return s
}

// summarizeDisks takes current usage from the newest weekly sample and the
// growth rate from the 30-day series, which is long enough to see a trend that
// a single week of a slowly filling volume would read as noise. Filesystems
// with history but absent from the newest sample are returned separately.
func summarizeDisks(week, month []statsRecord) ([]diskSummary, []missingDisk) {
	if len(week) == 0 {
		return nil, nil
	}
	latest := week[len(week)-1].Stats
	cur := map[string]fsStats{"root": {Size: latest.DiskSize, Used: latest.DiskUsed}}
	for name, fs := range latest.ExtraFs {
		cur[name] = fs
	}

	// Oldest first: the month, then this week. The growth series is the month
	// plus the newest sample; last-seen times use every sample.
	all := append(append([]statsRecord{}, month...), week...)
	type point struct {
		at   time.Time
		used float64
	}
	first := map[string]point{}
	lastSeen := map[string]time.Time{}
	for i, r := range all {
		fss := map[string]fsStats{"root": {Size: r.Stats.DiskSize, Used: r.Stats.DiskUsed}}
		for name, fs := range r.Stats.ExtraFs {
			fss[name] = fs
		}
		growthPoint := i < len(month) || i == len(all)-1
		for name, fs := range fss {
			if fs.Size <= 0 {
				continue
			}
			lastSeen[name] = r.Created.Time
			c, ok := cur[name]
			if !growthPoint || !ok || c.Size <= 0 || math.Abs(fs.Size-c.Size)/c.Size > sizeTolerance {
				continue
			}
			if _, seen := first[name]; !seen {
				first[name] = point{r.Created.Time, fs.Used}
			}
		}
	}

	var missing []missingDisk
	for name, at := range lastSeen {
		if c, ok := cur[name]; !ok || c.Size <= 0 {
			missing = append(missing, missingDisk{name, at})
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })

	now := week[len(week)-1].Created.Time
	var out []diskSummary
	for name, fs := range cur {
		if fs.Size <= 0 {
			continue
		}
		d := diskSummary{Name: name, UsedGB: fs.Used, SizeGB: fs.Size, Pct: 100 * fs.Used / fs.Size, DaysToFull: -1}
		if f, ok := first[name]; ok {
			days := now.Sub(f.at).Hours() / 24
			if days >= minGrowthDays {
				d.Measured = true
				d.GrowthGBPD = (fs.Used - f.used) / days
				if d.GrowthGBPD > 0 {
					d.DaysToFull = (fs.Size - fs.Used) / d.GrowthGBPD
				}
			}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Name == "root") != (out[j].Name == "root") {
			return out[i].Name == "root"
		}
		return out[i].Name < out[j].Name
	})
	return out, missing
}

// summarizeContainers ranks by average memory, because memory is what this
// host runs out of first. A container absent from some samples (stopped, or
// started mid-week) is averaged over the samples it appears in.
func summarizeContainers(recs []containerRecord, top int) ([]containerSummary, int) {
	type acc struct {
		cpu, mem, peak float64
		n              int
	}
	byName := map[string]*acc{}
	for _, r := range recs {
		for _, c := range r.Stats {
			a := byName[c.Name]
			if a == nil {
				a = &acc{}
				byName[c.Name] = a
			}
			a.cpu += c.CPU
			a.mem += c.Mem
			a.peak = math.Max(a.peak, c.Mem) // of two-hour averages
			a.n++
		}
	}
	out := make([]containerSummary, 0, len(byName))
	for name, a := range byName {
		out = append(out, containerSummary{
			Name:         name,
			CPUAvg:       a.cpu / float64(a.n),
			MemAvgMB:     a.mem / float64(a.n),
			MemBusiestMB: a.peak,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MemAvgMB != out[j].MemAvgMB {
			return out[i].MemAvgMB > out[j].MemAvgMB
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > top {
		return out[:top], len(out) - top
	}
	return out, 0
}

func grade(v, watch, act float64) level {
	switch {
	case v >= act:
		return levelAct
	case v >= watch:
		return levelWatch
	}
	return levelOK
}

// evaluate turns a summary into the findings the email leads with. Only WATCH
// and ACT are returned; an all-clear week returns none.
func evaluate(s summary) []finding {
	var out []finding
	add := func(l level, area, what, meaning string) {
		if l > levelOK {
			out = append(out, finding{l, area, what, meaning})
		}
	}

	if s.Samples == 0 {
		return []finding{{levelAct, "data", "no two-hour samples in the window",
			"The hub writes its first two-hour sample two hours after the agent connects, so a fresh install reads like this. Otherwise the agent has not been reporting: check `homelab status monitoring` and the hub's systems page."}}
	}

	if s.Expected > 0 {
		cov := float64(s.Samples) / float64(s.Expected)
		if cov < coverageWatch {
			add(levelWatch, "data", fmt.Sprintf("%d of %d two-hour samples (%.0f%%)", s.Samples, s.Expected, 100*cov),
				"The agent or the hub was down for part of the week, so every number below covers less than seven days.")
		}
	}

	const memMeaning = "RAM is the constraint. Lower AGENT_MEM_LIMIT/RESEARCH_MEM_LIMIT in obsidian-vault/.env, move a service off the box, or add RAM."
	mem := max(grade(s.MemAvgPct, memAvgWatch, memAvgAct), grade(s.MemPeakPct, memPeakWatch, memPeakAct))
	add(mem, "memory", fmt.Sprintf("average %.0f%%, peak %.0f%% of %.1f GB", s.MemAvgPct, s.MemPeakPct, s.MemTotalGB), memMeaning)

	if s.SwapTotalGB > 0 {
		add(grade(s.SwapPeakPct, swapWatch, swapAct), "swap",
			fmt.Sprintf("peak %.0f%% of %.1f GB in use", s.SwapPeakPct, s.SwapTotalGB),
			"Sustained swap means the working set no longer fits in RAM. Same remedies as memory.")
	}

	add(grade(s.CPUAvg, cpuAvgWatch, cpuAvgAct), "cpu", fmt.Sprintf("average %.0f%% (peak %.0f%%)", s.CPUAvg, s.CPUPeak),
		"The Celeron is busy most of the time. Find the top consumer below; move it off the box or accept slower agents.")

	if s.Threads > 0 {
		t := float64(s.Threads)
		add(grade(s.LoadAvg15/t, loadWatchPerThread, loadActPerThread), "load",
			fmt.Sprintf("15-minute load averaged %.2f on %d threads (busiest two hours %.2f)", s.LoadAvg15, s.Threads, s.LoadPeak15),
			"Work is queueing for the CPU or for disk. If iowait is also high it is the disks, not the CPU.")
	}

	if s.HasIOWait {
		add(grade(s.IOWaitAvg, iowaitWatch, iowaitAct), "iowait", fmt.Sprintf("average %.1f%%", s.IOWaitAvg),
			"The disks are the bottleneck. An SSD cache or moving hot data (snapshots, Docker's data root) to SSD helps more than a faster CPU.")
	}

	for _, m := range s.MissingDisks {
		add(levelAct, "disk "+m.Name, "not reported since "+m.LastSeen.Format("Mon 2 Jan 15:04 MST"),
			"The agent could not read it: unmounted, failed, or its probe directory gone. Check Storage & Snapshots in QTS before anything else.")
	}

	for _, d := range s.Disks {
		add(grade(d.Pct, diskWatch, diskAct), "disk "+d.Name, fmt.Sprintf("%.0f%% full (%.0f of %.0f GB)", d.Pct, d.UsedGB, d.SizeGB),
			"Free space or add capacity. On the data volume, check the snapshot repo and scratch first.")
		if d.DaysToFull >= 0 {
			l := levelOK
			if d.DaysToFull < fullActDays {
				l = levelAct
			} else if d.DaysToFull < fullWatchDays {
				l = levelWatch
			}
			add(l, "disk "+d.Name, fmt.Sprintf("growing %.1f GB/day, full in about %.0f days", d.GrowthGBPD, d.DaysToFull),
				"Something is accumulating. Find what grew before deciding it needs more disk.")
		}
	}

	if s.HotSensor != "" {
		add(grade(s.HotTemp, tempWatch, tempAct), "temperature", fmt.Sprintf("%s reached %.0f C (two-hour average)", s.HotSensor, s.HotTemp),
			"Check fans, dust and where the NAS sits before blaming the workload.")
	}

	if s.PriorSamples > 0 {
		if rise := s.MemAvgPct - s.PriorMem; rise >= memRiseWatch {
			add(levelWatch, "trend", fmt.Sprintf("memory average up %.0f points on the prior %.1f weeks", rise, s.PriorWeeks),
				"Usage is climbing. If a new service explains it, fine; if not, something is leaking.")
		}
		if rise := s.CPUAvg - s.PriorCPU; rise >= cpuRiseWatch {
			add(levelWatch, "trend", fmt.Sprintf("cpu average up %.0f points on the prior %.1f weeks", rise, s.PriorWeeks),
				"Usage is climbing. If a new service explains it, fine; if not, look for a stuck process.")
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Level > out[j].Level })
	return out
}

func verdict(found []finding) string {
	var acts, watches int
	for _, g := range found {
		switch g.Level {
		case levelAct:
			acts++
		case levelWatch:
			watches++
		}
	}
	switch {
	case acts == 0 && watches == 0:
		return "all clear"
	case acts == 0:
		return fmt.Sprintf("%d to watch", watches)
	case watches == 0:
		return fmt.Sprintf("%d to act on", acts)
	}
	return fmt.Sprintf("%d to act on, %d to watch", acts, watches)
}

// render produces the subject and a plain-text body. ASCII only, same rule as
// bin/homelab: it has to read the same in a phone's mail client, a terminal, and
// a mail gateway that mangles anything else.
func render(s summary, found []finding, dashboardURL string, loc *time.Location) (string, string) {
	subject := fmt.Sprintf("[homelab] weekly: %s", verdict(found))
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	const stamp = "Mon 2 Jan 15:04"

	w("Weekly report for %s\n", s.System)
	// Zone abbreviations, not loc's name: with /etc/localtime mounted the
	// location is called "Local", which tells the reader nothing. One per end,
	// because in a DST week the two ends are in different zones.
	const stampZ = stamp + " MST"
	w("%s to %s\n\n", s.From.In(loc).Format(stampZ), s.To.In(loc).Format(stampZ))
	w("Verdict: %s\n\n", verdict(found))

	for _, g := range found {
		w("%-6s %s: %s\n", g.Level, g.Area, g.Finding)
		w("       %s\n\n", g.Meaning)
	}

	if s.Samples == 0 {
		// A table of zeros would read as a measurement.
		if dashboardURL != "" {
			w("Dashboard: %s\n", dashboardURL)
		}
		return subject, b.String()
	}

	w("HOST                this week              prior %s\n", priorLabel(s))
	w("cpu average         %-22s %s\n", pct(s.CPUAvg), priorPct(s, s.PriorCPU))
	w("cpu peak            %s\n", pct(s.CPUPeak))
	if s.HasIOWait {
		w("iowait average      %s\n", fmt.Sprintf("%.1f%%", s.IOWaitAvg))
	}
	if s.Threads > 0 {
		w("load (15m)          %.2f avg, %.2f busiest, on %d threads\n", s.LoadAvg15, s.LoadPeak15, s.Threads)
	}
	w("memory average      %-22s %s\n", pct(s.MemAvgPct)+fmt.Sprintf(" of %.1f GB", s.MemTotalGB), priorPct(s, s.PriorMem))
	w("memory peak         %s\n", pct(s.MemPeakPct))
	if s.SwapTotalGB > 0 {
		w("swap, busiest 2h    %s of %.1f GB\n", pct(s.SwapPeakPct), s.SwapTotalGB)
	}
	for _, d := range s.Disks {
		growth := "growth unknown (under 3 days of history)"
		switch {
		case d.DaysToFull >= 0:
			growth = fmt.Sprintf("+%.1f GB/day, full in ~%.0f days", d.GrowthGBPD, d.DaysToFull)
		case d.Measured:
			growth = "not growing"
		}
		w("%-19s %s of %.0f GB, %s\n", "disk "+d.Name, pct(d.Pct), d.SizeGB, growth)
	}
	for _, m := range s.MissingDisks {
		w("%-19s NOT REPORTED since %s\n", "disk "+m.Name, m.LastSeen.In(loc).Format(stampZ))
	}
	if s.HotSensor != "" {
		w("hottest sensor      %s %.0f C (two-hour average)\n", s.HotSensor, s.HotTemp)
	}

	if len(s.Containers) > 0 {
		w("\nCONTAINERS, by average memory\n")
		w("%-24s %10s %11s %8s\n", "name", "mem avg", "busiest 2h", "cpu avg")
		for _, c := range s.Containers {
			w("%-24s %10s %11s %8s\n", trunc(c.Name, 24), mb(c.MemAvgMB), mb(c.MemBusiestMB), fmt.Sprintf("%.1f%%", c.CPUAvg))
		}
		if s.ContainersTruncated > 0 {
			w("... and %d more\n", s.ContainersTruncated)
		}
	}

	w("\nSamples: %d of %d two-hour windows. CPU and memory peaks are real peaks;\n", s.Samples, s.Expected)
	w("load, swap, temperature and container memory are the busiest two-hour\n")
	w("average, so a spike shorter than that reads lower than it was.\n")
	if dashboardURL != "" {
		w("Dashboard: %s\n", dashboardURL)
	}
	return subject, b.String()
}

func priorLabel(s summary) string {
	if s.PriorSamples == 0 {
		return "weeks"
	}
	return fmt.Sprintf("%.1f weeks", s.PriorWeeks)
}

func priorPct(s summary, v float64) string {
	if s.PriorSamples == 0 {
		return "-"
	}
	return pct(v)
}

func pct(v float64) string { return fmt.Sprintf("%.0f%%", v) }

func mb(v float64) string {
	if v >= 1024 {
		return fmt.Sprintf("%.1f GB", v/1024)
	}
	return fmt.Sprintf("%.0f MB", v)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "~"
}

// failureReport is what gets mailed when the report itself cannot be built.
// The mail path does not depend on the hub, so a hub that is down still
// produces an email saying so -- which is more useful than a missing one.
func failureReport(system string, err error, dashboardURL string) (string, string) {
	body := fmt.Sprintf("The weekly report for %s could not be built:\n\n  %v\n\n"+
		"Check the hub with `homelab status monitoring` and `homelab logs monitoring`.\n", system, err)
	if dashboardURL != "" {
		body += "Dashboard: " + dashboardURL + "\n"
	}
	return "[homelab] weekly: report failed", body
}
