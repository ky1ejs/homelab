"""Tests for mac-usage. Run: python3 -m unittest discover -s mac/usage"""

import datetime as dt
import importlib.machinery
import importlib.util
import os
import tempfile
import unittest

_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "mac-usage")
_loader = importlib.machinery.SourceFileLoader("mac_usage", _path)
_spec = importlib.util.spec_from_loader("mac_usage", _loader)
mu = importlib.util.module_from_spec(_spec)
_loader.exec_module(mu)

UTC = dt.timezone.utc


def t(s):
    return mu.parse_time(s)


VM_STAT = """Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               100000.
Pages active:                            1000000.
Pages inactive:                           900000.
Pages speculative:                         20000.
Pages throttled:                               0.
Pages wired down:                         300000.
Pages purgeable:                           50000.
"Translation faults":                  123456789.
Pages copy-on-write:                     1234567.
Anonymous pages:                         1250000.
Pages occupied by compressor:             100000.
"""

TOP = """Processes: 700 total, 3 running, 697 sleeping, 4000 threads
CPU usage: 3.10% user, 2.50% sys, 94.40% idle
Processes: 700 total, 3 running, 697 sleeping, 4000 threads
CPU usage: 40.00% user, 10.00% sys, 50.00% idle
"""

POWERMETRICS = """**** Processor usage ****

CPU Power: 12345 mW
GPU Power: 678 mW
ANE Power: 0 mW
Combined Power (CPU + GPU + ANE): 13023 mW

**** Thermal pressure ****

Current pressure level: Nominal
"""

PS = """  101   350.0  8388608 /System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine
  102     1.0    20480 /nix/store/abc-tart/bin/tart run cut-ci-1 --no-graphics
  103    25.0  1048576 /nix/store/xyz-claude-code/bin/claude remote-control --name x
  104    80.0   524288 /Applications/Xcode.app/Contents/Developer/usr/bin/xcodebuild build
  105     0.5    10240 /usr/sbin/sshd
"""


class Parsers(unittest.TestCase):
    def test_vm_stat(self):
        m = mu.parse_vm_stat(VM_STAT)
        gb = 16384 / 1024 ** 3
        self.assertAlmostEqual(m["app_gb"], round(1200000 * gb, 2))
        self.assertAlmostEqual(m["used_gb"], round((1200000 + 300000 + 100000) * gb, 2))

    def test_top_takes_second_sample(self):
        self.assertEqual(mu.parse_top_cpu(TOP), {"user": 40.0, "sys": 10.0, "idle": 50.0})

    def test_powermetrics(self):
        p = mu.parse_powermetrics(POWERMETRICS)
        self.assertEqual(p["combined_mw"], 13023)
        self.assertEqual(p["thermal"], "Nominal")

    def test_powermetrics_without_combined_line(self):
        p = mu.parse_powermetrics("CPU Power: 1000 mW\nGPU Power: 500 mW\n")
        self.assertEqual(p["combined_mw"], 1500)

    def test_loadavg_and_swap(self):
        self.assertEqual(mu.parse_loadavg("{ 1.50 2.25 3.00 }"), [1.5, 2.25, 3.0])
        self.assertEqual(mu.parse_swapusage("total = 2048.00M  used = 512.00M  free = 1536.00M"), 0.5)

    def test_ps_groups_and_no_command_lines(self):
        groups, vms = mu.parse_ps(PS)
        self.assertEqual(set(groups), {"vm", "tart", "claude", "xcode"})
        self.assertEqual(groups["vm"], {"n": 1, "cpu": 350.0, "rss_gb": 8.0})
        self.assertEqual(vms, [{"pid": 101, "cpu": 350.0, "rss_gb": 8.0}])
        self.assertNotIn("remote-control", repr(groups))


def job(**kw):
    base = {
        "repo": "cut", "run_id": 1, "name": "ios-build", "labels": ["cut-mac"],
        "runner_name": "mac-studio-1-20261114023800", "runner_group_name": "cut-mac",
        "created_at": "2026-11-14T02:38:00Z", "started_at": "2026-11-14T02:38:30Z",
        "completed_at": "2026-11-14T02:42:01Z", "status": "completed", "conclusion": "success",
    }
    base.update(kw)
    return base


class CI(unittest.TestCase):
    def test_runner_kind(self):
        self.assertEqual(mu.runner_kind(job()), "mac-studio")
        self.assertEqual(mu.runner_kind(job(labels=["xcode-27"], runner_name="GitHub Actions 1000",
                                            runner_group_name="GitHub Actions")), "github-macos")
        self.assertEqual(mu.runner_kind(job(labels=["macos-26"], runner_group_name=None,
                                            runner_name=None)), "github-macos")
        self.assertEqual(mu.runner_kind(job(labels=["ubuntu-latest"], runner_group_name="GitHub Actions",
                                            runner_name="GitHub Actions 2")), "github-linux")

    def test_billed_minutes_round_up_per_job(self):
        self.assertEqual(mu.billed_minutes(job()), 4)
        self.assertEqual(mu.billed_minutes(job(completed_at="2026-11-14T02:38:31Z")), 1)
        self.assertIsNone(mu.billed_minutes(job(conclusion="skipped")))
        self.assertIsNone(mu.billed_minutes(job(started_at=None)))

    def test_summarize_ci(self):
        jobs = {
            "1": job(),
            "2": job(created_at="2026-11-14T03:00:00Z", started_at="2026-11-14T03:05:00Z",
                     completed_at="2026-11-14T03:10:00Z", conclusion="failure"),
            "3": job(labels=["xcode-27"], runner_name="GitHub Actions 9", runner_group_name="GitHub Actions"),
            "4": job(created_at="2026-11-12T00:00:00Z"),
        }
        s = mu.summarize_ci(jobs, t("2026-11-13T00:00:00Z"), t("2026-11-15T00:00:00Z"))
        self.assertEqual(s["mac-studio"]["jobs"], 2)
        self.assertEqual(s["mac-studio"]["minutes"], 9)
        self.assertEqual(s["mac-studio"]["failed"], 1)
        self.assertEqual(s["mac-studio"]["wait_max_s"], 300)
        self.assertEqual(s["github-macos"]["minutes"], 4)
        self.assertEqual(s["first_mac_job"], "2026-11-14T02:38:30Z")
        self.assertEqual(s["mac_minutes_by_day"], {"2026-11-14": 9})


class FakeGitHub:
    def __init__(self, runs, jobs):
        self.runs, self.jobs, self.job_calls = runs, jobs, 0

    def pages(self, path, key):
        if path.startswith("/orgs/"):
            return iter([{"name": "cut"}, {"name": "old", "archived": True}])
        if "/jobs" in path:
            self.job_calls += 1
            run_id = int(path.split("/runs/")[1].split("/")[0])
            return iter(self.jobs[run_id])
        day = path.split("created=")[1]
        return iter([r for r in self.runs if r["created_at"].startswith(day)])


class Collect(unittest.TestCase):
    def test_backfill_then_only_changed_runs(self):
        runs = [{"id": 7, "name": "CI", "event": "pull_request", "created_at": "2026-11-14T02:30:00Z",
                 "updated_at": "2026-11-14T02:50:00Z", "status": "completed"}]
        jobs = {7: [{"id": 70, "run_attempt": 1, "name": "ios-build", "labels": ["cut-mac"],
                     "runner_name": "mac-studio-1-x", "runner_group_name": "cut-mac",
                     "created_at": "2026-11-14T02:38:00Z", "started_at": "2026-11-14T02:38:30Z",
                     "completed_at": "2026-11-14T02:42:01Z", "status": "completed", "conclusion": "success"}]}
        gh = FakeGitHub(runs, jobs)
        state = {}
        now = t("2026-11-14T12:00:00Z")
        mu.collect_jobs(gh, state, now)
        self.assertEqual(list(state["jobs"]), ["70"])
        self.assertEqual(state["jobs"]["70"]["workflow"], "CI")
        mu.collect_jobs(gh, state, now + dt.timedelta(minutes=15))
        self.assertEqual(gh.job_calls, 1)
        runs[0]["updated_at"] = "2026-11-14T13:00:00Z"
        mu.collect_jobs(gh, state, now + dt.timedelta(minutes=30))
        self.assertEqual(gh.job_calls, 2)


class Sessions(unittest.TestCase):
    def test_active_intervals_split_on_idle(self):
        base = t("2026-11-14T10:00:00Z")
        times = [base, base + dt.timedelta(minutes=2), base + dt.timedelta(minutes=4),
                 base + dt.timedelta(minutes=30)]
        spans = mu.active_intervals(times)
        self.assertEqual(len(spans), 2)
        self.assertEqual((spans[0][1] - spans[0][0]).total_seconds(), 240)
        self.assertEqual((spans[1][1] - spans[1][0]).total_seconds(), 60)

    def test_transcript_reads_timestamps_and_folder_only(self):
        with tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False) as f:
            f.write('{"timestamp":"2026-11-14T10:00:00Z","cwd":"/Users/worker/code/cut/.claude/worktrees/x",'
                    '"message":{"content":"secret"}}\n')
            f.write('not json\n')
            f.write('{"timestamp":"2026-11-14T10:03:00Z"}\n')
        try:
            e = mu.parse_transcript(f.name)
        finally:
            os.unlink(f.name)
        self.assertEqual(e["server"], "cut")
        self.assertEqual(e["spans"], [["2026-11-14T10:00:00Z", "2026-11-14T10:03:00Z"]])
        self.assertNotIn("secret", repr(e))

    def test_summarize_sessions_peak(self):
        sessions = {
            "a": {"server": "cut", "spans": [["2026-11-14T10:00:00Z", "2026-11-14T11:00:00Z"]]},
            "b": {"server": "any", "spans": [["2026-11-14T10:30:00Z", "2026-11-14T12:00:00Z"]]},
        }
        s = mu.summarize_sessions(sessions, t("2026-11-13T00:00:00Z"), t("2026-11-15T00:00:00Z"))
        self.assertEqual(s["active_hours"], 2.5)
        self.assertEqual(s["peak_concurrent"], 2)
        self.assertEqual(s["by_server"]["cut"], {"sessions": 1, "hours": 1.0})


class Money(unittest.TestCase):
    def test_payback(self):
        ci = mu.summarize_ci({}, t("2026-11-13T00:00:00Z"), t("2026-11-15T00:00:00Z"))
        ci["mac-studio"]["minutes"] = 1000
        ci["first_mac_job"] = "2026-11-14T00:00:00Z"
        agents = {"active_hours": 10.0, "first": "2026-11-14T00:00:00Z"}
        host = {"soc_watts_avg": 20.0}
        m = mu.money_model(ci, agents, host, t("2026-11-15T00:00:00Z"))
        std = m["rows"][0]
        self.assertEqual(std["ci_value_usd"], 62.0)
        self.assertAlmostEqual(std["ci_per_month_usd"], round(62.0 * mu.DAYS_PER_MONTH, 2))
        self.assertAlmostEqual(m["agent_per_month_usd"], round(36.0 * mu.DAYS_PER_MONTH, 2))
        power = 0.02 * 24 * mu.DAYS_PER_MONTH * mu.ELECTRICITY_USD_PER_KWH
        net = 62.0 * mu.DAYS_PER_MONTH + 36.0 * mu.DAYS_PER_MONTH - power
        self.assertAlmostEqual(std["payback_months"], round(mu.PURCHASE_NET_USD / net, 1))
        self.assertAlmostEqual(m["slot_use_pct"], round(100 * 1000 / (2 * 1440), 1))

    def test_report_renders_with_no_data(self):
        now = t("2026-11-15T00:00:00Z")
        s = mu.build_summary({}, [], t("2026-11-13T00:00:00Z"), now)
        text = mu.render_markdown(s)
        self.assertIn("Not enough use yet", text)
        self.assertIn("No host samples yet.", text)
        text.encode("ascii")


class Host(unittest.TestCase):
    def test_summarize_host(self):
        base = t("2026-11-14T10:00:00Z")
        samples = []
        for i in range(3):
            samples.append({
                "_t": base + dt.timedelta(minutes=i),
                "cpu": {"idle": 50.0 + i}, "mem": {"used_gb": 30.0 + i, "total_gb": 64.0},
                "swap_gb": 0.0, "pressure": 1 if i < 2 else 2, "disk_free_gb": 500.0 - i,
                "power": {"combined_mw": 20000.0, "thermal": "Nominal"},
                "groups": {"vm": {"n": 2}},
            })
        h = mu.summarize_host(samples)
        self.assertEqual(h["samples"], 3)
        self.assertEqual(h["coverage"], 1.0)
        self.assertEqual(h["cpu_busy_avg"], 49.0)
        self.assertEqual(h["pressure_not_normal_pct"], 33.3)
        self.assertEqual(h["soc_watts_avg"], 20.0)
        self.assertEqual(h["vms_avg"], 2)


if __name__ == "__main__":
    unittest.main()
