package main

// The machines the report covers. Every host goes through the same
// arithmetic (report.go); what differs is which lines it is judged by and
// what the email tells you to do, because the same number means different
// things on QTS and on macOS.

import "strings"

// limits is one area's WATCH and ACT lines. An ACT of zero means the area is
// not judged on that host: it still shows in the email's table, with no
// finding and no colour.
type limits struct{ Watch, Act float64 }

func (l limits) judged() bool { return l.Act > 0 }

type host struct {
	// Key names the host in alerts.json.
	Key string
	// System is Beszel's name for it.
	System string
	// Name is how sentences say it: "the NAS".
	Name string
	// Subject starts the email's subject line.
	Subject string
	// Optional hosts are reported on only when the hub has them; the NAS is
	// not, so a hub that has lost it is an error rather than silence.
	Optional bool

	MemAvg, MemPeak limits
	SwapPct         limits // percent of swap; used where swap has a fixed size
	SwapGB          limits // GB of swap; used instead where swap grows on demand
	CPUAvg          limits
	LoadPerThread   limits
	IOWait          limits
	Disk            limits
	Temp            limits

	advice
}

// advice is what the findings say about this host. Every string is a whole
// sentence or more, written for the email.
type advice struct {
	// Check is the Do for an agent that is down; NoData, after "wait", for
	// a week with no samples at all.
	Check, NoData string
	// Note ends the email's "how to read this" paragraph.
	Note string

	MemWhy, MemDo   string
	SwapWhy, SwapDo string
	// CPUDo is used when no container stands out.
	CPUWhy, CPUDo string
	// LoadNoIOWait is the load finding's Do where iowait is not measured.
	LoadNoIOWait string
	DiskWhy      string
	DiskDo       string
	// DiskNote ends the disk findings' What, where the free space they give
	// is not the host's own figure.
	DiskNote  string
	GrowthDo  string
	MissingDo string
	TempDo    string
	// Monitoring is what stops when the host stays up but sends nothing.
	Monitoring string
	// Rebooted is the Do when the host booted inside a gap; Stopped, when it
	// stayed up and the monitoring stopped (it takes the gap's start); Unsure,
	// when the data cannot tell.
	Rebooted, Stopped, Unsure string
	MemTrendDo, CPUTrendDo    string
}

// title is Name at the start of a sentence.
func (h host) title() string {
	if h.Name == "" {
		return ""
	}
	return strings.ToUpper(h.Name[:1]) + h.Name[1:]
}

// The NAS's lines. Constants, not .env tunables, on purpose: they are the
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
	// Two-hour averages, across every sensor the agent can read (CPU package,
	// cores, and disks where the kernel exposes them).
	tempWatch, tempAct = 70.0, 80.0
)

// The mac studio's lines, where they differ from the NAS's.
//
// Memory is not judged by percent. macOS keeps memory full of cache and
// compresses before it swaps, so a high percent is normal there; what says RAM
// is short is macOS's pressure level, which Beszel does not read.
// mac/claude/heartbeat alerts on it, and `mac-usage report` judges a week of
// it. Swap is judged in GB because macOS grows its swap files on demand, so a
// percent of a size that keeps changing means nothing; 8 GB is the heartbeat's
// own line.
//
// The disk lines are the NAS's, and on its 1 TB disk they land near the
// heartbeat's: 80% is about 160 GB free and 90% about 70 GB. The percent counts
// the data volume against the whole disk, so it reads a few points low.
//
// Temperature is not judged: Apple silicon runs its dies near 100 C under load
// by design and slows itself down before harm, and macOS's thermal pressure,
// which says when it has, is in `mac-usage report`. macOS has no iowait.
const (
	macSwapWatchGB, macSwapActGB = 4.0, 8.0
	macDiskWatch, macDiskAct     = diskWatch, diskAct
)

// nasHost is the NAS, under the name the hub gives it (REPORT_SYSTEM).
func nasHost(system string) host {
	return host{
		Key: "nas", System: system, Name: "the NAS", Subject: "[homelab] weekly",
		MemAvg:        limits{memAvgWatch, memAvgAct},
		MemPeak:       limits{memPeakWatch, memPeakAct},
		SwapPct:       limits{swapWatch, swapAct},
		CPUAvg:        limits{cpuAvgWatch, cpuAvgAct},
		LoadPerThread: limits{loadWatchPerThread, loadActPerThread},
		IOWait:        limits{iowaitWatch, iowaitAct},
		Disk:          limits{diskWatch, diskAct},
		Temp:          limits{tempWatch, tempAct},
		advice: advice{
			Check:  "Run `homelab status monitoring`, then `homelab logs monitoring` for the agent's last lines.",
			NoData: "Otherwise run `homelab status monitoring` and open the hub's systems page.",
			// docs/host.md: the stacks' mem_limit values add up to more than
			// the RAM that is free.
			MemWhy: "The stacks' memory limits already add up to more than the free RAM, so if both agents get busy at once the kernel kills a container to make room.",
			MemDo: "Lower AGENT_MEM_LIMIT or RESEARCH_MEM_LIMIT in obsidian-vault/.env, " +
				"move a service off the box, or add RAM.",
			SwapWhy:      "A little swap is normal. This much means the working set no longer fits in RAM, and everything slows while pages come back from disk.",
			SwapDo:       "Use the same fixes as for memory: shrink or move that container, or add RAM.",
			CPUWhy:       "At this average, background jobs and agent runs queue behind each other, so everything on the box gets slower.",
			CPUDo:        "Find the busiest container in the dashboard, then move it off the box or accept slower runs.",
			LoadNoIOWait: "Check iowait in the dashboard: if it is high the disks are the bottleneck, otherwise the CPU is.",
			DiskWhy:      "When it fills, writes fail: containers crash and snapshots stop.",
			DiskDo:       "Free space first: on the data volume, check the snapshot repo and the agents' scratch. Add capacity if that is not enough.",
			GrowthDo:     "Find what grew before buying disks: the snapshot repo, the agents' scratch and Docker images are the likeliest places.",
			MissingDo:    "Open Storage & Snapshots in QTS before anything else.",
			TempDo:       "Check the fans, the dust and the airflow around the NAS before blaming the workload.",
			Monitoring:   "the monitoring stack",
			Rebooted:     "If you did not restart it, check QTS's System Logs for why: a power cut, a firmware update or a crash.",
			Stopped:      "Run `homelab logs monitoring` and look for the hub or the agent exiting around %s. A redeploy explains it; a crash needs fixing.",
			Unsure:       "Run `homelab status monitoring`. If everything is up, check QTS's System Logs for a restart around the gap.",
			MemTrendDo:   "If you added a service, this is expected. If not, look at each container's memory history in the dashboard for the one that keeps growing.",
			CPUTrendDo:   "If you added a service, this is expected. If not, look at each container's CPU history in the dashboard for the one that changed.",
		},
	}
}

// macStudioHost is the mac studio. Its agent is mac/beszel; the hub adds it
// when MON_MAC_STUDIO_HOST is set (setup.go).
var macStudioHost = host{
	Key: "mac-studio", System: "mac-studio", Name: "the mac studio", Subject: "[homelab] mac studio weekly",
	Optional:      true,
	SwapGB:        limits{macSwapWatchGB, macSwapActGB},
	CPUAvg:        limits{cpuAvgWatch, cpuAvgAct},
	LoadPerThread: limits{loadWatchPerThread, loadActPerThread},
	Disk:          limits{macDiskWatch, macDiskAct},
	advice: advice{
		Check: "On the mac studio, run `sudo launchctl print system/org.nixos.beszel-agent` and read /var/log/beszel-agent.log. " +
			"If it is running, the hub cannot reach it: `homelab logs monitoring` on the NAS shows the hub's side.",
		NoData: "Otherwise check the agent on the mac studio: `sudo launchctl print system/org.nixos.beszel-agent`, and its log, /var/log/beszel-agent.log.",
		Note: "Memory percent is not judged here: macOS keeps memory full of cache, so pressure says when it is short. " +
			"The heartbeat alerts on pressure, and `mac-usage report` judges it over the week, with compressed memory. " +
			"Beszel's own memory chart averages each point unless set to Max, so a short spike reads flat there. " +
			"Temperature is not judged either: Apple silicon runs near 100 C under load by design and slows itself before harm, " +
			"and `mac-usage report` shows when it has.",
		SwapWhy: "macOS swaps only once compressing memory is not enough, so this much swap means the working set does not fit in 64 GB, and CI and agent sessions slow while pages come back from disk.",
		SwapDo:  "Run `mac-usage report` on the mac studio for the worst pressure and what ran then: CI VMs, Xcode, agent sessions. Fewer CI slots or standing sessions at once is the fix short of more RAM.",
		CPUWhy:  "At this average, CI jobs and agent sessions queue behind each other, so each takes longer than it would alone.",
		CPUDo:   "Run `mac-usage report` on the mac studio for the CI jobs and agent sessions behind it. Fewer CI slots or standing sessions at once frees CPU; otherwise accept slower runs.",
		LoadNoIOWait: "macOS reports no iowait, so load cannot tell the CPU from the disk. `mac-usage report` names what ran; " +
			"if the CPU was not busy at the same time, the disk was.",
		DiskWhy:   "Cut's CI stops taking jobs at 50 GB free, and when the disk fills, builds and agent sessions fail.",
		DiskNote:  "The disk is shared with macOS's other volumes, so this free space is an upper bound: the heartbeat's figure is the one to trust.",
		DiskDo:    "The heartbeat names worker's biggest folders. Old CI images (`ls ~/.tart/vms` in worker-shell), Xcode's DerivedData and simulators are the usual places.",
		GrowthDo:  "Find what grew before buying storage: CI images, Xcode's DerivedData and worker's checkouts are the likeliest places.",
		MissingDo: "Check the agent's log, /var/log/beszel-agent.log on the mac studio: it lists the filesystems it found at start.",
		// The agent or the hub: a stayed-up gap looks the same from either.
		Monitoring: "its monitoring",
		Rebooted:   "If you did not restart it, `log show --last 7d --predicate 'eventMessage CONTAINS \"Previous shutdown cause\"'` on the mac studio says why. After a power cut it waits at FileVault's unlock: `ssh ms-unlock`.",
		Stopped:    "Read /var/log/beszel-agent.log on the mac studio, and `homelab logs monitoring` on the NAS, around %s. An apply that restarted the agent explains it; a crash needs fixing.",
		Unsure:     "Check that the mac studio is on and reachable (`ssh ms`), then that its agent is running: `sudo launchctl print system/org.nixos.beszel-agent`.",
		MemTrendDo: "Run `mac-usage report` on the mac studio: more CI jobs or agent sessions explain it.",
		CPUTrendDo: "Run `mac-usage report` on the mac studio: more CI jobs or agent sessions explain it.",
	},
}

// hosts is every machine the report covers, in the order their emails go.
func hosts(nasSystem string) []host {
	return []host{nasHost(nasSystem), macStudioHost}
}
