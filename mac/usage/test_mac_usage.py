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
        "repo": "cut", "run_id": 1, "workflow": "PR Tests", "event": "pull_request",
        "name": "ios-build / build", "labels": ["cut-mac"],
        "runner_name": "mac-studio-1-20261114023800", "runner_group_name": "cut-mac",
        "created_at": "2026-11-14T02:38:00Z", "started_at": "2026-11-14T02:38:30Z",
        "completed_at": "2026-11-14T02:42:01Z", "status": "completed", "conclusion": "success",
    }
    base.update(kw)
    return base


def hosted(**kw):
    kw.setdefault("labels", ["xcode-27"])
    kw.setdefault("runner_name", "GitHub Actions 9")
    kw.setdefault("runner_group_name", "GitHub Actions")
    return job(**kw)


# A job cancelled while it waited for a runner: GitHub gives it start and end
# times, but no runner.
QUEUED_CANCEL = dict(runner_name=None, runner_group_name=None, conclusion="cancelled",
                     started_at="2026-11-14T03:00:00Z", completed_at="2026-11-14T03:20:00Z",
                     created_at="2026-11-14T03:00:00Z")

SINCE = "2026-11-01T00:00:00Z"
NOW = "2026-11-15T00:00:00Z"


class CI(unittest.TestCase):
    def test_runner_kind(self):
        self.assertEqual(mu.runner_kind(job()), "mac-studio")
        self.assertEqual(mu.runner_kind(hosted()), "github-macos")
        self.assertEqual(mu.runner_kind(job(labels=["macos-26"], runner_group_name=None,
                                            runner_name="GitHub Actions 3")), "github-macos")
        self.assertEqual(mu.runner_kind(job(labels=["ubuntu-latest"], runner_group_name="GitHub Actions",
                                            runner_name="GitHub Actions 2")), "github-linux")

    def test_billed_minutes_round_up_per_job(self):
        self.assertEqual(mu.billed_minutes(job()), 4)
        self.assertEqual(mu.billed_minutes(job(completed_at="2026-11-14T02:38:31Z")), 1)
        self.assertIsNone(mu.billed_minutes(job(conclusion="skipped")))
        self.assertIsNone(mu.billed_minutes(job(started_at=None)))

    def test_job_cancelled_in_the_queue_has_no_minutes(self):
        self.assertIsNone(mu.billed_minutes(job(**QUEUED_CANCEL)))
        s = mu.summarize_ci({"1": job(), "2": job(**QUEUED_CANCEL), "3": hosted(**QUEUED_CANCEL)},
                            t(SINCE), t(NOW))
        self.assertEqual(s["mac-studio"]["jobs"], 1)
        self.assertEqual(s["mac-studio"]["minutes"], 4)
        self.assertEqual(s["mac-studio"]["never_ran"], 1)
        self.assertEqual(s["github-macos"]["minutes"], 0)
        self.assertEqual(s["github-macos"]["never_ran"], 1)

    def test_job_key_drops_the_callers_prefix(self):
        self.assertEqual(mu.job_key(job(name="pr-tests / ios-build / build")), "ios-build / build")
        self.assertEqual(mu.job_key(job(name="ios-build / build")), "ios-build / build")

    def test_summarize_ci(self):
        jobs = {
            # xcode-27 history before the Mac Studio: a build takes 6 or 8 minutes there.
            "h1": hosted(created_at="2026-11-13T10:00:00Z", started_at="2026-11-13T10:00:00Z",
                         completed_at="2026-11-13T10:06:00Z"),
            "h2": hosted(created_at="2026-11-13T11:00:00Z", started_at="2026-11-13T11:00:00Z",
                         completed_at="2026-11-13T11:08:00Z"),
            "1": job(),
            "2": job(name="pr-tests / ios-build / build", workflow="PR Tests Rerun",
                     created_at="2026-11-14T03:00:00Z", started_at="2026-11-14T03:05:00Z",
                     completed_at="2026-11-14T03:10:00Z", conclusion="failure"),
            "3": job(name="deploy-ios / deploy", workflow="Main Push", event="push",
                     created_at="2026-11-14T04:00:00Z", started_at="2026-11-14T04:00:10Z",
                     completed_at="2026-11-14T04:10:00Z"),
            "4": hosted(created_at="2026-11-14T05:00:00Z", started_at="2026-11-14T05:00:02Z",
                        completed_at="2026-11-14T05:07:00Z"),
        }
        s = mu.summarize_ci(jobs, t(SINCE), t(NOW))
        mac = s["mac-studio"]
        self.assertEqual(s["first_mac_job"], "2026-11-14T02:38:00Z")
        self.assertEqual((mac["jobs"], mac["minutes"], mac["failed"]), (3, 4 + 5 + 10, 1))
        self.assertEqual(mac["wait_max_s"], 300)
        # Both builds at xcode-27's median, 7; the deploy has no history.
        self.assertEqual(mac["priced_from_history"], 2)
        self.assertEqual(mac["hosted_minutes"], 7 + 7 + 10)
        self.assertEqual((mac["gated_jobs"], mac["gated_hosted_minutes"]), (1, 7))
        # The fallback counts from the first Mac Studio job, not the history.
        self.assertEqual(s["github-macos"]["jobs"], 1)
        self.assertEqual(s["speed"]["ios-build / build"]["hosted_median"], 7)
        self.assertEqual(s["mac_minutes_by_day"], {"2026-11-14": 19})


class FakeGitHub:
    def __init__(self, runs, jobs):
        self.runs, self.jobs, self.job_calls, self.days = runs, jobs, 0, []
        self.last_total = None

    def pages(self, path, key):
        if path.startswith("/orgs/"):
            return iter([{"name": "cut"}, {"name": "old", "archived": True}])
        if "/jobs" in path:
            self.job_calls += 1
            self.last_total = None
            run_id = int(path.split("/runs/")[1].split("/")[0])
            return iter(self.jobs[run_id])
        day = path.split("created=")[1]
        self.days.append(day)
        found = [r for r in self.runs if r["created_at"].startswith(day)]
        self.last_total = 1000 if day == "2026-11-02" else len(found)
        return iter(found)


RUNS = [{"id": 7, "name": "PR Tests", "event": "pull_request", "created_at": "2026-11-14T02:30:00Z",
         "updated_at": "2026-11-14T02:50:00Z", "status": "completed"}]
JOBS = {7: [{"id": 70, "run_attempt": 1, "name": "ios-build", "labels": ["cut-mac"],
             "runner_name": "mac-studio-1-x", "runner_group_name": "cut-mac",
             "created_at": "2026-11-14T02:38:00Z", "started_at": "2026-11-14T02:38:30Z",
             "completed_at": "2026-11-14T02:42:01Z", "status": "completed", "conclusion": "success"}]}


class Collect(unittest.TestCase):
    def test_backfill_then_only_changed_runs(self):
        runs = [dict(r) for r in RUNS]
        gh = FakeGitHub(runs, JOBS)
        state, problems = {}, []
        now = t("2026-11-14T12:00:00Z")
        mu.collect_jobs(gh, state, now, problems)
        self.assertEqual(list(state["jobs"]), ["70"])
        self.assertEqual(state["jobs"]["70"]["workflow"], "PR Tests")
        self.assertEqual(gh.days[0], mu.HISTORY_FROM)
        mu.collect_jobs(gh, state, now + dt.timedelta(minutes=15), problems)
        self.assertEqual(gh.job_calls, 1)
        runs[0]["updated_at"] = "2026-11-14T13:00:00Z"
        mu.collect_jobs(gh, state, now + dt.timedelta(minutes=30), problems)
        self.assertEqual(gh.job_calls, 2)

    def test_recent_days_then_a_daily_full_rescan(self):
        gh = FakeGitHub(RUNS, JOBS)
        state = {}
        now = t("2026-11-20T12:00:00Z")
        mu.collect_jobs(gh, state, now, [])
        gh.days = []
        mu.collect_jobs(gh, state, now + dt.timedelta(hours=1), [])
        self.assertEqual(gh.days[0], "2026-11-18")
        gh.days = []
        mu.collect_jobs(gh, state, now + dt.timedelta(hours=25), [])
        self.assertEqual(gh.days[0], mu.HISTORY_FROM)

    def test_a_day_at_the_runs_cap_is_a_problem(self):
        problems = []
        mu.collect_jobs(FakeGitHub(RUNS, JOBS), {}, t("2026-11-03T00:00:00Z"), problems)
        self.assertEqual(len(problems), 1)
        self.assertIn("2026-11-02", problems[0])

    def test_any_failure_is_recorded_and_the_state_saved(self):
        import http.client

        class Broken:
            def __init__(self, _token):
                pass

            def pages(self, path, key):
                raise http.client.IncompleteRead(b"")

        with tempfile.TemporaryDirectory() as d:
            token = os.path.join(d, "token")
            with open(token, "w") as f:
                f.write("x")
            old = (mu.GitHub, mu.token_file, mu.transcripts_glob, os.environ.get("MAC_USAGE_DATA_DIR"))
            mu.GitHub, mu.token_file = Broken, lambda: token
            mu.transcripts_glob = lambda: os.path.join(d, "none", "*.jsonl")
            os.environ["MAC_USAGE_DATA_DIR"] = d
            try:
                with open(os.devnull, "w") as devnull:
                    import contextlib
                    with contextlib.redirect_stderr(devnull):
                        mu.cmd_collect(None)
            finally:
                mu.GitHub, mu.token_file, mu.transcripts_glob = old[:3]
                if old[3] is None:
                    del os.environ["MAC_USAGE_DATA_DIR"]
                else:
                    os.environ["MAC_USAGE_DATA_DIR"] = old[3]
            state = mu.read_json(os.path.join(d, "state.json"), {})
            self.assertIn("IncompleteRead", state["problems"][0])
            self.assertTrue(os.path.exists(os.path.join(d, "summary.md")))


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

    def test_concurrent_sessions_count_once_in_busy_hours(self):
        sessions = {
            "a": {"server": "cut", "spans": [["2026-11-14T10:00:00Z", "2026-11-14T11:00:00Z"]]},
            "b": {"server": "any", "spans": [["2026-11-14T10:30:00Z", "2026-11-14T12:00:00Z"]]},
            "c": {"server": "cut", "spans": [["2026-11-14T13:00:00Z", "2026-11-14T13:30:00Z"]]},
        }
        s = mu.summarize_sessions(sessions, t(SINCE), t(NOW))
        self.assertEqual(s["session_hours"], 3.0)
        self.assertEqual(s["busy_hours"], 2.5)
        self.assertEqual(s["peak_concurrent"], 2)
        self.assertEqual(s["by_server"]["cut"], {"sessions": 2, "hours": 1.5})


def state_with(jobs, last_collect):
    return {"jobs": jobs, "last_collect": last_collect, "sessions": {
        # Eight sessions at once all day: usage, but no value.
        str(i): {"server": "cut", "spans": [["2026-11-14T08:00:00Z", "2026-11-14T18:00:00Z"]]}
        for i in range(8)}}


def mac_jobs(n):
    out = {}
    for i in range(n):
        start = t("2026-11-14T08:00:00Z") + dt.timedelta(minutes=30 * i)
        out[str(i)] = job(created_at=mu.iso(start), started_at=mu.iso(start),
                          completed_at=mu.iso(start + dt.timedelta(minutes=10)))
    return out


class Money(unittest.TestCase):
    def test_agent_sessions_do_not_move_the_payback(self):
        jobs = mac_jobs(20)
        with_agents = mu.build_summary(state_with(jobs, "2026-11-15T00:00:00Z"), [], t(SINCE), t(NOW))
        without = mu.build_summary({"jobs": jobs, "last_collect": "2026-11-15T00:00:00Z"}, [], t(SINCE), t(NOW))
        self.assertEqual(with_agents["money"], without["money"])
        self.assertEqual(with_agents["agents"]["busy_hours"], 10.0)
        m = with_agents["money"]
        self.assertEqual(m["value_usd"], round(200 * mu.HOSTED_USD_PER_MIN, 2))
        days = (t(NOW) - t("2026-11-14T08:00:00Z")).total_seconds() / 86400
        self.assertEqual(m["payback_months"],
                         round(mu.PURCHASE_NET_USD / round(m["value_usd"] / max(1.0, days) * mu.DAYS_PER_MONTH, 2), 1))
        self.assertIsNone(m["gated_payback_months"])
        self.assertIn("never at this pace", mu.render_markdown(with_agents))

    def test_stale_ci_data_warns_and_keeps_its_pace(self):
        state = {"jobs": mac_jobs(20), "last_collect": "2026-11-15T00:00:00Z"}
        fresh = mu.build_summary(state, [], t(SINCE), t("2026-11-15T00:30:00Z"))
        stale = mu.build_summary(state, [], t(SINCE), t("2026-11-18T00:00:00Z"))
        self.assertFalse(fresh["ci_stale"])
        self.assertTrue(stale["ci_stale"])
        self.assertEqual(fresh["money"], stale["money"])
        text = mu.render_markdown(stale)
        self.assertTrue(text.splitlines()[2].startswith("No payback: the CI data is 72.0 h old"))
        self.assertIn("pays back in", mu.render_markdown(fresh))

    def test_power_lowers_the_value(self):
        self.assertEqual(mu.payback(100.0, 20.0), round(mu.PURCHASE_NET_USD / 80.0, 1))
        self.assertIsNone(mu.payback(10.0, 20.0))

    def test_report_renders_with_no_data(self):
        s = mu.build_summary({}, [], t(SINCE), t(NOW))
        text = mu.render_markdown(s)
        self.assertIn("No CI data yet", text)
        self.assertIn("No host samples yet.", text)
        text.encode("ascii")


class Host(unittest.TestCase):
    def samples(self):
        base = t("2026-11-14T10:00:00Z")
        out = []
        for i in range(3):
            out.append({
                "_t": base + dt.timedelta(minutes=i),
                "cpu": {"idle": 50.0 + i}, "mem": {"used_gb": 30.0 + i, "total_gb": 64.0},
                "swap_gb": 0.0, "pressure": 1 if i < 2 else 2, "disk_free_gb": 500.0 - i,
                "power": {"combined_mw": 20000.0, "thermal": "Nominal"},
                "groups": {"vm": {"n": 2}},
            })
        return out

    def test_summarize_host(self):
        h = mu.summarize_host(self.samples(), t("2026-11-14T10:02:30Z"))
        self.assertEqual(h["samples"], 3)
        self.assertEqual(h["coverage"], 1.0)
        self.assertEqual(h["cpu_busy_avg"], 49.0)
        self.assertEqual(h["pressure_not_normal_pct"], 33.3)
        self.assertEqual(h["soc_watts_avg"], 20.0)
        self.assertEqual(h["vms_avg"], 2)

    def test_a_stopped_sampler_shows(self):
        now = t("2026-11-14T11:00:00Z")
        h = mu.summarize_host(self.samples(), now)
        self.assertEqual(h["coverage"], round(3 / 61, 3))
        self.assertEqual(h["last_age_min"], 58.0)
        s = mu.build_summary({}, self.samples(), t(SINCE), now)
        self.assertTrue(s["host_stale"])
        self.assertIn("The sampler has stopped.", mu.render_markdown(s))


if __name__ == "__main__":
    unittest.main()
