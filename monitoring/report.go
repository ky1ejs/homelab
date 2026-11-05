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
// RAM that is free (docs/host.md), so "average 85%" leaves no room for both
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

	// Samples is how many two-hour samples the week has; Expected is how many
	// it would have had if the agent and hub ran throughout the part of the
	// week that monitoring existed for. HistoryStart is set when monitoring
	// began inside the window (a fresh install or a reset hub), which makes
	// the week partial rather than broken.
	Samples, Expected int
	HistoryStart      time.Time
	LastSample        time.Time
	Gaps              []gap
	// BootTime is when the NAS last booted, from the agent's uptime; zero when
	// the hub did not report one. Status is the hub's view of the agent at the
	// moment the report ran.
	BootTime time.Time
	Status   string

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
	TopCPU              containerSummary // the busiest container by CPU, of all of them
}

// gap is a stretch of the monitored week with no two-hour samples.
type gap struct {
	From, To time.Time
}

func (g gap) Duration() time.Duration { return g.To.Sub(g.From) }

// minGap is the shortest stretch reported as a gap: two missing samples. One
// missing sample is the hub writing a rollup a little late, not an outage.
const minGap = 2 * sampleWindow

// finding is one thing the email asks you to look at. Every finding answers
// three questions, in this order, because a verdict without a reason is easy
// to ignore and a reason without an action leaves you to work it out:
//
//	What  the measurement, with its numbers
//	Why   what it means for this box
//	Do    the action, as specific as the data allows
type finding struct {
	Level level
	Area  string // short, for the subject line: "memory", "disk data"
	Title string // one sentence, the finding's headline
	What  string
	Why   string
	Do    string
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
		Status:   sys.Status,
	}
	if s.Threads == 0 {
		s.Threads = sys.Info.Cores
	}
	if sys.Info.Uptime > 0 && !sys.Updated.IsZero() {
		s.BootTime = sys.Updated.Add(-time.Duration(sys.Info.Uptime) * time.Second)
	}
	s.HistoryStart = historyStart(week, month, from)
	start := from
	if !s.HistoryStart.IsZero() {
		start = s.HistoryStart
	}
	// From the real duration, which is 167 or 169 hours in a DST week.
	s.Expected = int(to.Sub(start) / sampleWindow)
	s.Gaps = findGaps(week, start, to)
	if len(week) > 0 {
		s.LastSample = week[len(week)-1].Created.Time
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
	s.Containers, s.ContainersTruncated, s.TopCPU = summarizeContainers(ctrs, 5)
	return s
}

// historyStart returns when monitoring began, if that was inside the window,
// and zero otherwise. The 30-day series is the evidence: if it has a record
// from before the window, the hub existed for the whole week and any missing
// samples are an outage. If it has none, the week's first sample is where the
// hub's history begins, and the hours before it were never monitored.
//
// One sample of slack, so a hub that started an hour into the week does not
// turn every report into a "partial week".
func historyStart(week, month []statsRecord, from time.Time) time.Time {
	if len(week) == 0 {
		return time.Time{}
	}
	for _, r := range month {
		if r.Created.Before(from) {
			return time.Time{}
		}
	}
	// A record is written at the end of the window it averages.
	start := week[0].Created.Add(-sampleWindow)
	if start.Sub(from) <= sampleWindow {
		return time.Time{}
	}
	return start
}

// findGaps lists the stretches of [start, to) that no sample covers, ignoring
// any shorter than minGap. A record covers the two hours before it was written.
func findGaps(week []statsRecord, start, to time.Time) []gap {
	var out []gap
	covered := start
	for _, r := range week {
		begins := r.Created.Add(-sampleWindow)
		if begins.Sub(covered) >= minGap {
			out = append(out, gap{covered, begins})
		}
		if r.Created.After(covered) {
			covered = r.Created.Time
		}
	}
	if to.Sub(covered) >= minGap {
		out = append(out, gap{covered, to})
	}
	return out
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
// started mid-week) is averaged over the samples it appears in. The busiest
// container by CPU is returned separately, from all of them, because a CPU
// finding needs it and it need not be among the top few by memory.
func summarizeContainers(recs []containerRecord, top int) ([]containerSummary, int, containerSummary) {
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
	var topCPU containerSummary
	for _, c := range out {
		if c.CPUAvg > topCPU.CPUAvg {
			topCPU = c
		}
	}
	if len(out) > top {
		return out[:top], len(out) - top, topCPU
	}
	return out, 0, topCPU
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

// stamp is how every time in the email is written. The zone is per time, not
// per email, because in a DST week the two ends of the window are in different
// zones. Abbreviations rather than loc's name: with /etc/localtime mounted the
// location is called "Local", which tells the reader nothing.
const stamp = "Mon 2 Jan 15:04 MST"

// evaluate turns a summary into the findings the email leads with. Only WATCH
// and ACT are returned; an all-clear week returns none. loc is the zone the
// findings' times are written in.
func evaluate(s summary, loc *time.Location) []finding {
	var out []finding
	add := func(f finding) {
		if f.Level > levelOK {
			out = append(out, f)
		}
	}
	at := func(t time.Time) string { return t.In(loc).Format(stamp) }

	if s.Samples == 0 {
		return []finding{{
			Level: levelAct, Area: "monitoring",
			Title: "There is no data for this week.",
			What:  fmt.Sprintf("The hub has no two-hour samples from %s to %s.", at(s.From), at(s.To)),
			Why:   "This report cannot judge anything, and Beszel's alerts are blind too.",
			Do: "If monitoring was set up in the last two hours, wait: the hub writes its first sample two hours after the agent connects. " +
				"Otherwise run `homelab status monitoring` and open the hub's systems page.",
		}}
	}

	if s.Status == "down" {
		add(finding{
			Level: levelAct, Area: "agent",
			Title: "The agent is not reporting right now.",
			What:  fmt.Sprintf("When this report ran, the hub listed %s as down. The last sample was at %s.", s.System, at(s.LastSample)),
			Why:   "Nothing is being measured, and Beszel's alerts cannot fire until it is back.",
			Do:    "Run `homelab status monitoring`, then `homelab logs monitoring` for the agent's last lines.",
		})
	}

	if s.Expected > 0 {
		if cov := float64(s.Samples) / float64(s.Expected); cov < coverageWatch {
			add(coverageFinding(s, cov, at))
		}
	}

	memLevel := max(grade(s.MemAvgPct, memAvgWatch, memAvgAct), grade(s.MemPeakPct, memPeakWatch, memPeakAct))
	add(finding{
		Level: memLevel, Area: "memory",
		Title: map[level]string{levelWatch: "Memory is getting full.", levelAct: "Memory is too full."}[memLevel],
		What:  fmt.Sprintf("It averaged %.0f%% and peaked at %.0f%% of %.1f GB.", s.MemAvgPct, s.MemPeakPct, s.MemTotalGB),
		// docs/host.md: the stacks' mem_limit values add up to more than the
		// RAM that is free.
		Why: "The stacks' memory limits already add up to more than the free RAM, so if both agents get busy at once the kernel kills a container to make room.",
		Do: biggest(s) + "Lower AGENT_MEM_LIMIT or RESEARCH_MEM_LIMIT in obsidian-vault/.env, " +
			"move a service off the box, or add RAM.",
	})

	if s.SwapTotalGB > 0 {
		add(finding{
			Level: grade(s.SwapPeakPct, swapWatch, swapAct), Area: "swap",
			Title: "Swap is in heavy use.",
			What:  fmt.Sprintf("In the busiest two hours, %.0f%% of %.1f GB of swap was in use.", s.SwapPeakPct, s.SwapTotalGB),
			Why:   "A little swap is normal. This much means the working set no longer fits in RAM, and everything slows while pages come back from disk.",
			Do:    biggest(s) + "Use the same fixes as for memory: shrink or move that container, or add RAM.",
		})
	}

	cpuDo := "Find the busiest container in the dashboard, then move it off the box or accept slower runs."
	if s.TopCPU.Name != "" {
		cpuDo = fmt.Sprintf("%s is the busiest container (%.1f%% of the CPU on average). Move it off the box, or accept slower runs.", s.TopCPU.Name, s.TopCPU.CPUAvg)
	}
	add(finding{
		Level: grade(s.CPUAvg, cpuAvgWatch, cpuAvgAct), Area: "cpu",
		Title: "The CPU is busy most of the time.",
		What:  fmt.Sprintf("It averaged %.0f%% and peaked at %.0f%%.", s.CPUAvg, s.CPUPeak),
		Why:   "At this average, background jobs and agent runs queue behind each other, so everything on the box gets slower.",
		Do:    cpuDo,
	})

	iowaitHigh := s.HasIOWait && s.IOWaitAvg >= iowaitWatch
	if s.Threads > 0 {
		t := float64(s.Threads)
		loadDo := "iowait is low, so the queue is for the CPU: treat this like a busy CPU and find the busiest container."
		if iowaitHigh {
			loadDo = "iowait is high too, so the queue is for the disks, not the CPU: see the iowait finding."
		} else if !s.HasIOWait {
			loadDo = "Check iowait in the dashboard: if it is high the disks are the bottleneck, otherwise the CPU is."
		}
		add(finding{
			Level: grade(s.LoadAvg15/t, loadWatchPerThread, loadActPerThread), Area: "load",
			Title: "Work is queueing for the CPU or the disks.",
			What: fmt.Sprintf("The 15-minute load averaged %.2f on %d threads (%.2f per thread). The busiest two hours averaged %.2f.",
				s.LoadAvg15, s.Threads, s.LoadAvg15/t, s.LoadPeak15),
			Why: fmt.Sprintf("Above %.1f per thread, tasks spend a noticeable part of their time waiting for a turn.", loadWatchPerThread),
			Do:  loadDo,
		})
	}

	if s.HasIOWait {
		add(finding{
			Level: grade(s.IOWaitAvg, iowaitWatch, iowaitAct), Area: "iowait",
			Title: "The disks are the bottleneck.",
			What:  fmt.Sprintf("The CPU spent %.1f%% of its time waiting for the disks.", s.IOWaitAvg),
			Why:   "Everything that reads or writes waits on them, and a faster CPU would not help.",
			Do:    "Move the busiest data (Docker's data root, snapshots) to SSD, or add an SSD cache.",
		})
	}

	for _, m := range s.MissingDisks {
		add(finding{
			Level: levelAct, Area: "disk " + m.Name,
			Title: fmt.Sprintf("The %s volume stopped reporting.", m.Name),
			What:  fmt.Sprintf("The agent has not reported it since %s.", at(m.LastSeen)),
			Why:   "The agent drops a filesystem it cannot read, so it may be unmounted or failed, or its probe directory may be gone.",
			Do:    "Open Storage & Snapshots in QTS before anything else.",
		})
	}

	for _, d := range s.Disks {
		add(finding{
			Level: grade(d.Pct, diskWatch, diskAct), Area: "disk " + d.Name,
			Title: fmt.Sprintf("The %s volume is %.0f%% full.", d.Name, d.Pct),
			What:  fmt.Sprintf("%s of %s is used, leaving %s.", gb(d.UsedGB), gb(d.SizeGB), gb(d.SizeGB-d.UsedGB)),
			Why:   "When it fills, writes fail: containers crash and snapshots stop.",
			Do:    "Free space first: on the data volume, check the snapshot repo and the agents' scratch. Add capacity if that is not enough.",
		})
		if d.DaysToFull >= 0 {
			l := levelOK
			if d.DaysToFull < fullActDays {
				l = levelAct
			} else if d.DaysToFull < fullWatchDays {
				l = levelWatch
			}
			add(finding{
				Level: l, Area: "disk " + d.Name,
				Title: fmt.Sprintf("The %s volume will be full in about %.0f days.", d.Name, d.DaysToFull),
				What: fmt.Sprintf("It grew %.1f GB a day over the last 30 days, and has %s free.",
					d.GrowthGBPD, gb(d.SizeGB-d.UsedGB)),
				Why: fmt.Sprintf("At that rate it fills around %s.", s.To.Add(time.Duration(d.DaysToFull*24)*time.Hour).In(loc).Format("2 Jan 2006")),
				Do:  "Find what grew before buying disks: the snapshot repo, the agents' scratch and Docker images are the likeliest places.",
			})
		}
	}

	if s.HotSensor != "" {
		add(finding{
			Level: grade(s.HotTemp, tempWatch, tempAct), Area: "temperature",
			Title: "The NAS is running hot.",
			What:  fmt.Sprintf("The %s sensor averaged %.0f C over its hottest two hours.", sensorName(s.HotSensor), s.HotTemp),
			Why:   "Sustained heat shortens disk life and makes the CPU slow itself down.",
			Do:    "Check the fans, the dust and the airflow around the NAS before blaming the workload.",
		})
	}

	if s.PriorSamples > 0 {
		if rise := s.MemAvgPct - s.PriorMem; rise >= memRiseWatch {
			add(finding{
				Level: levelWatch, Area: "memory trend",
				Title: "Memory use is climbing.",
				What:  fmt.Sprintf("It averaged %.0f%% this week, against %.0f%% over the prior %s.", s.MemAvgPct, s.PriorMem, weeks(s.PriorWeeks)),
				Why:   "A rise with no new service to explain it usually means something is leaking.",
				Do:    "If you added a service, this is expected. If not, look at each container's memory history in the dashboard for the one that keeps growing.",
			})
		}
		if rise := s.CPUAvg - s.PriorCPU; rise >= cpuRiseWatch {
			add(finding{
				Level: levelWatch, Area: "cpu trend",
				Title: "CPU use is climbing.",
				What:  fmt.Sprintf("It averaged %.0f%% this week, against %.0f%% over the prior %s.", s.CPUAvg, s.PriorCPU, weeks(s.PriorWeeks)),
				Why:   "A rise with no new service to explain it usually means a process is stuck in a loop.",
				Do:    "If you added a service, this is expected. If not, look at each container's CPU history in the dashboard for the one that changed.",
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Level > out[j].Level })
	return out
}

// coverageFinding explains missing samples. The question it has to answer is
// whether the NAS was off or only the monitoring was, because the fixes
// differ, and the agent's uptime can usually tell them apart: a boot inside a
// gap means the NAS restarted; a boot before every gap means it stayed up and
// the stack itself stopped.
func coverageFinding(s summary, cov float64, at func(time.Time) string) finding {
	var down time.Duration
	var spans []string
	for i, g := range s.Gaps {
		down += g.Duration()
		if i < 3 {
			spans = append(spans, fmt.Sprintf("%s to %s", at(g.From), at(g.To)))
		}
	}
	if len(s.Gaps) > 3 {
		spans = append(spans, fmt.Sprintf("%d shorter ones", len(s.Gaps)-3))
	}

	f := finding{Level: levelWatch, Area: "monitoring gaps"}
	if len(s.Gaps) == 0 {
		f.Title = "Samples are missing across the week."
		f.What = fmt.Sprintf("The hub has %d of the %d two-hour samples it should, missing a few at a time.", s.Samples, s.Expected)
	} else {
		f.Title = fmt.Sprintf("Monitoring was down for about %s.", hours(down))
		f.What = fmt.Sprintf("No samples from %s.", strings.Join(spans, ", "))
	}

	var rebootGap *gap
	for i, g := range s.Gaps {
		// A boot just after a gap ends belongs to it too: the agent takes a
		// minute to come up, and a sample's window ends where the next starts.
		if !s.BootTime.Before(g.From) && !s.BootTime.After(g.To.Add(sampleWindow)) {
			rebootGap = &s.Gaps[i]
			break
		}
	}
	switch {
	case rebootGap != nil:
		f.What += fmt.Sprintf(" The NAS booted at %s, so it was off or restarting during the gap that ended then.", at(s.BootTime))
		f.Do = "If you did not restart it, check QTS's System Logs for why: a power cut, a firmware update or a crash."
	case len(s.Gaps) > 0 && !s.BootTime.IsZero() && s.BootTime.Before(s.Gaps[0].From):
		f.What += fmt.Sprintf(" The NAS has been up since %s, so it stayed on and the monitoring stack stopped.", at(s.BootTime))
		f.Do = fmt.Sprintf("Run `homelab logs monitoring` and look for the hub or the agent exiting around %s. A redeploy explains it; a crash needs fixing.", at(s.Gaps[0].From))
	default:
		f.What += " The data cannot tell whether the NAS was off or only the monitoring stack was."
		f.Do = "Run `homelab status monitoring`. If everything is up, check QTS's System Logs for a restart around the gap."
	}
	f.Why = fmt.Sprintf("Every number below covers %.0f%% of the week, and anything that happened in the gaps is missing from them.", 100*cov)
	return f
}

// biggest names the container using the most memory, as the start of a Do.
func biggest(s summary) string {
	if len(s.Containers) == 0 {
		return ""
	}
	c := s.Containers[0]
	return fmt.Sprintf("%s uses the most memory (%s on average). ", c.Name, mb(c.MemAvgMB))
}

// sensorName turns Beszel's sensor keys into what they are. Unknown keys are
// shown as they are: a wrong guess at a sensor is worse than its raw name.
func sensorName(key string) string {
	k := strings.ToLower(key)
	switch {
	case strings.HasPrefix(k, "coretemp_package"):
		return "CPU package"
	case strings.HasPrefix(k, "coretemp_core_"):
		return "CPU core " + strings.TrimPrefix(k, "coretemp_core_")
	case strings.HasPrefix(k, "nvme"):
		return "NVMe (" + key + ")"
	}
	return key
}

func weeks(w float64) string {
	if w > 0.95 && w < 1.05 {
		return "week"
	}
	return fmt.Sprintf("%.1f weeks", w)
}

func hours(d time.Duration) string {
	h := d.Hours()
	if h >= 48 {
		return fmt.Sprintf("%.1f days", h/24)
	}
	return fmt.Sprintf("%.0f hours", h)
}

// gb writes a size in GB, switching to TB where the GB figure gets long.
func gb(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1f TB", v/1000)
	}
	return fmt.Sprintf("%.0f GB", v)
}

func pct(v float64) string { return fmt.Sprintf("%.0f%%", v) }

func mb(v float64) string {
	if v >= 1024 {
		return fmt.Sprintf("%.1f GB", v/1024)
	}
	return fmt.Sprintf("%.0f MB", v)
}
