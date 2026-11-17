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

    def test_vm_stat_swapouts(self):
        self.assertNotIn("swapout_gb", mu.parse_vm_stat(VM_STAT))
        m = mu.parse_vm_stat(VM_STAT + "Swapins:                                   1000.\nSwapouts:                                 65536.\n")
        self.assertEqual(m["swapout_gb"], 1.0)

    def test_booted_sims(self):
        sim = ("  201     0.1    10240 /Library/Developer/PrivateFrameworks/CoreSimulator.framework/Resources/bin/"
               "launchd_sim /Users/worker/Library/Developer/CoreSimulator/Devices/X/data/var/run/launchd_bootstrap.plist\n")
        svc = "  202     0.1    10240 /Library/Developer/PrivateFrameworks/CoreSimulator.framework/Versions/A/XPCServices/SimulatorTrampoline.xpc\n"
        self.assertEqual(mu.count_booted_sims(PS + sim + sim.replace("201", "203") + svc), 2)
        self.assertEqual(mu.count_booted_sims(PS), 0)

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

    def test_ps_colima_host_agent(self):
        vm = PS.splitlines()[0].replace("101", "106", 1)
        lima = "  107     0.3    40960 /nix/store/q-lima/bin/limactl hostagent --pidfile ha.pid colima"
        groups, vms = mu.parse_ps(PS + vm + "\n" + lima + "\n")
        self.assertEqual(groups["vm"]["n"], 2)
        self.assertEqual(groups["colima"]["n"], 1)
        self.assertEqual(len(vms), 2)
        wrapped = "  108     0.3    40960 /nix/store/q-lima/bin/.limactl-wrapped hostagent --pidfile ha.pid colima"
        self.assertEqual(mu.parse_ps(wrapped)[0]["colima"]["n"], 1)


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
            # A slow run: the mean takes it in, the median doesn't.
            "h3": hosted(created_at="2026-11-13T12:00:00Z", started_at="2026-11-13T12:00:00Z",
                         completed_at="2026-11-13T12:19:00Z"),
            "h5": hosted(created_at="2026-11-13T13:00:00Z", started_at="2026-11-13T13:00:00Z",
                         completed_at="2026-11-13T13:10:00Z"),
            # A build that failed after 3 minutes there.
            "hf": hosted(name="pr-tests / ios-build / build", created_at="2026-11-13T14:00:00Z",
                         started_at="2026-11-13T14:00:00Z", completed_at="2026-11-13T14:03:00Z",
                         conclusion="failure"),
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
        # The build at the mean of xcode-27's five successes, (6 + 8 + 19 + 10 + 7) / 5 = 10,
        # not their median, 8. The failed build at xcode-27's one failure, 3 minutes, not at a
        # success's price. The deploy has no history, so at its own 10.
        self.assertEqual(mac["priced_from_history"], 2)
        self.assertEqual(mac["priced_at_median"], 1)
        self.assertEqual(mac["hosted_minutes"], 10 + 3 + 10)
        self.assertEqual((mac["gated_jobs"], mac["gated_hosted_minutes"]), (1, 3))
        # The fallback counts from the first Mac Studio job, not the history.
        self.assertEqual(s["github-macos"]["jobs"], 1)
        self.assertEqual(s["speed"]["ios-build / build"]["hosted_median"], 8)
        self.assertEqual(s["speed"]["ios-build / build"]["hosted_mean"], 10)
        self.assertEqual(s["speed"]["ios-build / build"]["hosted_n"], 5)
        self.assertEqual(s["mac_minutes_by_day"], {"2026-11-14": 19})

    def test_hosted_durations_by_outcome(self):
        def run(i, minutes, conclusion="success", name="deploy-ios / deploy"):
            start = t("2026-11-10T00:00:00Z") + dt.timedelta(hours=i)
            return hosted(name=name, conclusion=conclusion, created_at=mu.iso(start), started_at=mu.iso(start),
                          completed_at=mu.iso(start + dt.timedelta(minutes=minutes)))

        jobs = {
            "a": run(0, 10), "b": run(1, 12), "c": run(2, 11), "d": run(3, 13), "e": run(4, 54),
            # A deploy that timed out waiting on TestFlight, and one that failed fast: both
            # price a deploy that fails or times out, never a success.
            "f": run(5, 60, "timed_out"), "g": run(6, 2, "failure"),
            # Cancelled: neither.
            "h": run(7, 30, "cancelled"),
            # Too few runs for the mean.
            "i": run(8, 4, name="lint"), "j": run(9, 40, name="lint"),
        }
        h = mu.hosted_durations(jobs)
        ok = h[("deploy-ios / deploy", "success")]
        self.assertEqual((ok["n"], ok["median"], ok["mean"], ok["price"]), (5, 12, 20, 20))
        failed = h[("deploy-ios / deploy", "failed")]
        self.assertEqual((failed["n"], failed["price"]), (2, 31))
        self.assertEqual(h[("lint", "success")]["price"], 22)
        self.assertEqual(len(h), 3)


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
            old = (mu.GitHub, mu.token_file, mu.transcripts_globs, os.environ.get("MAC_USAGE_DATA_DIR"))
            mu.GitHub, mu.token_file = Broken, lambda: token
            mu.transcripts_globs = lambda: [os.path.join(d, "none", "*.jsonl")]
            os.environ["MAC_USAGE_DATA_DIR"] = d
            try:
                with open(os.devnull, "w") as devnull:
                    import contextlib
                    with contextlib.redirect_stderr(devnull):
                        mu.cmd_collect(None)
            finally:
                mu.GitHub, mu.token_file, mu.transcripts_globs = old[:3]
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
        self.assertEqual({k: s["by_server"]["cut"][k] for k in ("sessions", "hours", "busy_hours")},
                         {"sessions": 2, "hours": 1.5, "busy_hours": 1.5})
        self.assertEqual(s["by_server"]["cut"]["xcode_sessions"], 0)
        self.assertEqual(s["by_server"]["any"]["busy_hours"], 1.5)

    def test_subagent_time_counts_once_in_its_session(self):
        parent = "/w/.claude/projects/p/abc.jsonl"
        sub = "/w/.claude/projects/p/abc/subagents/agent-1.jsonl"
        self.assertEqual(mu.session_of(sub), parent)
        self.assertEqual(mu.session_of(parent), parent)
        sessions = {
            parent: {"server": "cut", "session": parent,
                     "spans": [["2026-11-14T10:00:00Z", "2026-11-14T10:30:00Z"]]},
            # The subagent ran on past the parent's last entry while the parent waited.
            sub: {"server": "cut", "session": parent,
                  "spans": [["2026-11-14T10:20:00Z", "2026-11-14T11:00:00Z"]]},
        }
        s = mu.summarize_sessions(sessions, t(SINCE), t(NOW))
        self.assertEqual((s["sessions"], s["session_hours"], s["peak_concurrent"]), (1, 1.0, 1))

    def test_collect_reads_subagent_transcripts(self):
        with tempfile.TemporaryDirectory() as d:
            proj = os.path.join(d, ".claude", "projects", "p")
            os.makedirs(os.path.join(proj, "abc", "subagents"))
            with open(os.path.join(proj, "abc.jsonl"), "w") as f:
                f.write('{"timestamp":"2026-11-14T10:00:00Z","cwd":"/Users/worker/code/cut"}\n')
            with open(os.path.join(proj, "abc", "subagents", "agent-1.jsonl"), "w") as f:
                f.write('{"timestamp":"2026-11-14T10:10:00Z","cwd":"/Users/worker/code/cut"}\n')
            old = mu.WORKER_HOME
            mu.WORKER_HOME = d
            try:
                state = {}
                mu.collect_sessions(state)
            finally:
                mu.WORKER_HOME = old
        self.assertEqual(len(state["sessions"]), 2)
        self.assertEqual({e["session"] for e in state["sessions"].values()}, {os.path.join(proj, "abc.jsonl")})


WT = "/Users/worker/code/cut/.claude/worktrees/otter"
SIM_A = "11111111-2222-3333-4444-555555555555"
SIM_B = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
DEVICES = "/Users/worker/Library/Developer/CoreSimulator/Devices"
PS_SESSIONS = "\n".join([
    # Cut's scripts pass an absolute -derivedDataPath, and the destination's UDID.
    "  101 150.0 512000 /Applications/Xcode.app/Contents/Developer/usr/bin/xcodebuild -workspace Cut.xcworkspace "
    "-derivedDataPath %s/ios/.derivedData -destination platform=iOS Simulator,id=%s,arch=arm64 test"
    % (WT, SIM_A.lower()),
    "  102 300.5 900000 /Applications/Xcode.app/.../swift-frontend -frontend -c %s/ios/Cut/App.swift" % WT,
    # No folder in its arguments: its working directory says.
    "  103  50.0 100000 /Applications/Xcode.app/Contents/Developer/usr/bin/xcodebuild -scheme Cut build",
    # In no session's folder.
    "  104  10.0 100000 /Applications/Xcode.app/Contents/Developer/usr/bin/xcodebuild -version",
    "  201   2.0  50000 /Library/Developer/PrivateFrameworks/CoreSimulator.framework/Resources/bin/launchd_sim "
    "%s/%s/data/var/run/launchd_bootstrap.plist" % (DEVICES, SIM_A),
    "  202  40.0 200000 %s/%s/data/Containers/Bundle/Application/X/Cut.app/Cut" % (DEVICES, SIM_A),
    "  203   1.0  50000 /Library/Developer/PrivateFrameworks/CoreSimulator.framework/Resources/bin/launchd_sim "
    "%s/%s/data/var/run/launchd_bootstrap.plist" % (DEVICES, SIM_B),
    # The shared service is in no one simulator.
    "  204   5.0  50000 /Library/Developer/PrivateFrameworks/CoreSimulator.framework/Versions/A/XPCServices/"
    "com.apple.CoreSimulator.CoreSimulatorService.xpc/Contents/MacOS/com.apple.CoreSimulator.CoreSimulatorService",
    "  301  99.0 100000 /usr/bin/python3 -c secret",
])


def write_lines(path, recs):
    import json
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        for r in recs:
            f.write(json.dumps(r) + "\n")


class SessionDetail(unittest.TestCase):
    def test_session_ids_from_folders(self):
        self.assertEqual(mu.session_id_of(WT + "/ios"), "cut/otter")
        self.assertEqual(mu.session_id_of("/Users/worker/code/any/sessions/fox/repo"), "any/fox")
        self.assertIsNone(mu.session_id_of("/Users/worker/code/cut"))
        self.assertIsNone(mu.session_id_of(None))

    def test_lsof_working_directories(self):
        self.assertEqual(mu.parse_lsof_cwds("p103\nfcwd\nn%s/ios\np9\nfcwd\nn/\n" % WT),
                         {103: WT + "/ios", 9: "/"})

    def test_xcode_and_simulators_are_traced_to_their_session(self):
        asked = []

        def cwd_of(pids):
            asked.append(sorted(pids))
            return {103: "/Users/worker/code/cut/.claude/worktrees/heron/ios"}

        # SIM_B is named "... @cut/heron" (sim_session).
        out = mu.attribute_sessions(PS_SESSIONS, cwd_of, {SIM_B: "cut/heron"}.get)
        # Only the Xcode processes with no folder in their arguments are looked up.
        self.assertEqual(asked, [[103, 104]])
        self.assertEqual(out["cut/otter"], {"xcode_n": 2, "xcode_cpu": 450.5, "sims": 1, "sim_cpu": 42.0})
        self.assertEqual(out["cut/heron"], {"xcode_n": 1, "xcode_cpu": 50.0, "sims": 1, "sim_cpu": 1.0})
        self.assertEqual(set(out), {"cut/otter", "cut/heron"})
        self.assertNotIn("secret", repr(out))

    def test_a_simulator_is_named_for_its_session(self):
        with tempfile.TemporaryDirectory() as d:
            dev = os.path.join(d, "Library", "Developer", "CoreSimulator", "Devices", SIM_B)
            os.makedirs(dev)
            with open(os.path.join(dev, "device.plist"), "wb") as f:
                mu.plistlib.dump({"name": "Cut E2E @cut/heron", "UDID": SIM_B}, f, fmt=mu.plistlib.FMT_BINARY)
            old = mu.WORKER_HOME
            mu.WORKER_HOME = d
            try:
                self.assertEqual(mu.sim_session(SIM_B), "cut/heron")
                self.assertIsNone(mu.sim_session(SIM_A))
            finally:
                mu.WORKER_HOME = old

    def test_transcript_counts_prompts_and_tools_without_their_text(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "s.jsonl")
            write_lines(path, [
                {"timestamp": "2026-11-14T10:00:00Z", "cwd": WT, "type": "user",
                 "message": {"role": "user", "content": "build it, secret"}},
                {"timestamp": "2026-11-14T10:01:00Z", "type": "assistant", "message": {"content": [
                    {"type": "tool_use", "id": "t1", "name": "Bash",
                     "input": {"command": "cd ios && ./scripts/build-app.sh --secret"}},
                    {"type": "tool_use", "id": "t2", "name": "Bash", "input": {"command": "xcrun simctl boot X"}},
                    {"type": "tool_use", "id": "t3", "name": "mcp__XcodeBuildMCP__build_sim", "input": {}},
                    {"type": "tool_use", "id": "t4", "name": "Read", "input": {"file_path": "/x"}}]}},
                # The same call written twice counts once.
                {"timestamp": "2026-11-14T10:01:00Z", "type": "assistant", "message": {"content": [
                    {"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "xcodebuild"}}]}},
                {"timestamp": "2026-11-14T10:02:00Z", "type": "user", "message": {"content": [
                    {"type": "tool_result", "tool_use_id": "t1", "content": "ok"}]}},
                {"timestamp": "2026-11-14T10:03:00Z", "type": "user", "isMeta": True, "message": {"content": "x"}},
                {"timestamp": "2026-11-14T11:00:00Z", "type": "user",
                 "message": {"content": [{"type": "text", "text": "now test"}]}},
            ])
            e = mu.parse_transcript(path)
        self.assertEqual((e["prompts"], e["tools"], e["xcode_cmds"], e["sim_cmds"]), (2, 4, 2, 1))
        self.assertEqual(e["worktree"], "cut/otter")
        self.assertEqual(e["open"], ["2026-11-14T10:00:00Z", "2026-11-14T11:00:00Z"])
        self.assertEqual(len(e["spans"]), 2)
        self.assertNotIn("secret", repr(e))

    def test_cached_transcripts_are_read_again_by_a_new_version(self):
        with tempfile.TemporaryDirectory() as d:
            proj = os.path.join(d, ".claude", "projects", "p")
            path = os.path.join(proj, "abc.jsonl")
            write_lines(path, [{"timestamp": "2026-11-14T10:00:00Z", "cwd": WT}])
            st = os.stat(path)
            state = {"sessions": {path: {"key": "%d:%d" % (st.st_size, int(st.st_mtime)), "spans": []}}}
            old = mu.WORKER_HOME
            mu.WORKER_HOME = d
            try:
                mu.collect_sessions(state)
            finally:
                mu.WORKER_HOME = old
        self.assertEqual(state["sessions"][path]["worktree"], "cut/otter")

    def manifest(self, d, rel, logs):
        path = os.path.join(d, rel, "LogStoreManifest.plist")
        os.makedirs(os.path.dirname(path))
        with open(path, "wb") as f:
            mu.plistlib.dump({"logFormatVersion": 11, "logs": logs}, f, fmt=mu.plistlib.FMT_BINARY)
        return path

    def test_build_and_test_logs_are_read_and_kept(self):
        # 2026-11-14T10:00:00Z in seconds since 2001.
        at = (t("2026-11-14T10:00:00Z") - mu.APPLE_EPOCH).total_seconds()
        with tempfile.TemporaryDirectory() as d:
            self.manifest(d, "code/cut/.claude/worktrees/otter/ios/.derivedData/Logs/Build", {
                "B1": {"timeStartedRecording": at, "timeStoppedRecording": at + 300, "title": "secret",
                       "primaryObservable": {"highLevelStatus": "S"}},
                "B2": {"timeStartedRecording": at + 600, "timeStoppedRecording": at + 660,
                       "primaryObservable": {"highLevelStatus": "E"}},
                "bad": {"timeStartedRecording": "x"}})
            test = self.manifest(d, "code/cut/.claude/worktrees/otter/ios/.derivedData/pr-verification/Logs/Test", {
                "T1": {"timeStartedRecording": at + 900, "timeStoppedRecording": at + 1500, "highLevelStatus": "W"}})
            self.manifest(d, "code/cut/.claude/worktrees/otter/ios/.derivedData/Logs/Launch", {
                "L1": {"timeStartedRecording": at, "timeStoppedRecording": at + 1}})
            dd = os.path.join(d, "Library/Developer/Xcode/DerivedData/Cut-abc")
            self.manifest(d, "Library/Developer/Xcode/DerivedData/Cut-abc/Logs/Build", {
                "B3": {"timeStartedRecording": at, "timeStoppedRecording": at + 60}})
            with open(os.path.join(dd, "info.plist"), "wb") as f:
                mu.plistlib.dump({"WorkspacePath": "/Users/worker/code/any/sessions/fox/App.xcodeproj"}, f)
            with open(os.path.join(d, "broken.plist"), "w") as f:
                f.write("x")
            old = mu.WORKER_HOME
            mu.WORKER_HOME = d
            try:
                state = {}
                mu.collect_builds(state)
                # Xcode drops T1 from its index; the cache keeps it.
                with open(test, "wb") as f:
                    mu.plistlib.dump({"logs": {"T2": {"timeStartedRecording": at + 2000,
                                                      "timeStoppedRecording": at + 2100}}}, f)
                st = os.stat(test)
                os.utime(test, (st.st_atime, st.st_mtime + 5))
                mu.collect_builds(state)
            finally:
                mu.WORKER_HOME = old
        by_sid = {}
        for e in state["builds"].values():
            by_sid.setdefault(e["session"], {}).update(e["logs"])
        self.assertEqual(set(by_sid), {"cut/otter", "any/fox"})
        self.assertEqual(sorted(by_sid["cut/otter"]), ["B1", "B2", "T1", "T2"])
        self.assertEqual(by_sid["cut/otter"]["B1"], {"kind": "build", "start": "2026-11-14T10:00:00Z",
                                                     "seconds": 300.0, "status": "S"})
        self.assertEqual(by_sid["cut/otter"]["T1"]["status"], "W")
        self.assertNotIn("secret", repr(state))

    def test_summary_counts_xcode_time_as_active_and_prices_only_xcode_sessions(self):
        base = t("2026-11-14T10:00:00Z")
        sessions = {
            # Ten minutes of transcript, then a 40-minute test run with no entries.
            "/p/a.jsonl": {"server": "cut", "worktree": "cut/otter", "session": "/p/a.jsonl",
                           "spans": [["2026-11-14T10:00:00Z", "2026-11-14T10:10:00Z"]],
                           "open": ["2026-11-14T10:00:00Z", "2026-11-14T12:00:00Z"],
                           "prompts": 3, "tools": 20, "xcode_cmds": 1, "sim_cmds": 0},
            # Its subagent, whose task isn't a prompt.
            "/p/a/subagents/x.jsonl": {"server": "cut", "worktree": None, "session": "/p/a.jsonl",
                                       "spans": [["2026-11-14T10:05:00Z", "2026-11-14T10:08:00Z"]],
                                       "prompts": 1, "tools": 5},
            # A Cut session that only read and wrote code.
            "/p/b.jsonl": {"server": "cut", "worktree": "cut/heron", "session": "/p/b.jsonl",
                           "spans": [["2026-11-14T10:00:00Z", "2026-11-14T11:00:00Z"]],
                           "open": ["2026-11-14T10:00:00Z", "2026-11-14T11:00:00Z"], "prompts": 1, "tools": 9},
            # An old transcript, read before transcripts had a worktree.
            "/p/c.jsonl": {"server": "cut", "spans": [["2026-11-14T13:00:00Z", "2026-11-14T13:30:00Z"]]},
        }
        samples = []
        for i in range(40):
            samples.append({"_t": base + dt.timedelta(minutes=10 + i),
                            "groups": {"xcode": {"n": 2, "cpu": 400.0}},
                            "by_session": {"cut/otter": {"xcode_n": 2, "xcode_cpu": 300.0, "sims": 1,
                                                         "sim_cpu": 60.0}}})
        # An old sample, before samples traced sessions.
        samples.append({"_t": base + dt.timedelta(hours=3), "groups": {"xcode": {"n": 1, "cpu": 999.0}}})
        builds = {"m": {"session": "cut/otter", "logs": {
            "B1": {"kind": "build", "start": "2026-11-14T10:10:00Z", "seconds": 600, "status": "S"},
            "T1": {"kind": "test", "start": "2026-11-14T10:20:00Z", "seconds": 1800, "status": "E"}}},
            "n": {"session": None, "logs": {
                "B9": {"kind": "build", "start": "2026-11-14T10:10:00Z", "seconds": 60, "status": "S"}}}}
        ag = mu.summarize_sessions(sessions, t(SINCE), t(NOW), samples, builds)
        self.assertEqual(ag["sessions"], 3)
        # otter: 10:00 to 10:50, its transcript then its Xcode minutes and test run.
        otter = [r for r in ag["busiest"] if r["session"] == "cut/otter"][0]
        self.assertEqual(otter["active_hours"], round(50 / 60, 2))
        self.assertEqual(otter["open_hours"], 2.0)
        self.assertEqual((otter["prompts"], otter["tools"], otter["builds"], otter["tests"], otter["tests_failed"]),
                         (3, 25, 1, 1, 1))
        self.assertEqual((otter["xcode_cpu_hours"], otter["sim_hours"]), (2.0, round(40 / 60, 2)))
        self.assertEqual(ag["busiest"][0]["session"], "cut/heron")
        self.assertIn(None, [r["session"] for r in ag["busiest"]])
        x, sim = ag["xcode"], ag["simulators"]
        self.assertEqual((x["sessions"], x["builds"], x["tests"], x["tests_failed"], x["untraced_runs"]),
                         (1, 2, 1, 1, 1))
        self.assertEqual((x["running_hours"], x["cpu_hours"], x["traced_pct"]), (round(40 / 60, 2), 2.0, 75.0))
        self.assertEqual((sim["sessions"], sim["booted_hours"], sim["cpu_hours"]), (1, round(40 / 60, 2), 0.4))
        cut = ag["by_server"]["cut"]
        self.assertEqual((cut["sessions"], cut["xcode_sessions"], cut["xcode_hours"]), (3, 1, round(50 / 60, 2)))
        self.assertEqual(ag["prompts"], 4)
        # Priced: otter's 50 minutes; not heron's hour or c's half hour.
        state = {"jobs": mac_jobs(1), "last_collect": NOW, "sessions": sessions, "builds": builds}
        s = mu.build_summary(state, samples, t(SINCE), t(NOW))
        m = s["money"]
        # Hours are kept to two places, so a few seconds out.
        self.assertAlmostEqual(m["sessions_low_minutes"], 50, delta=0.5)
        self.assertEqual(m["sessions_high_minutes"], m["sessions_low_minutes"])
        self.assertEqual((m["sessions_priced"], m["sessions_all"]), (1, 3))
        text = mu.render_markdown(s)
        self.assertIn("1 of 3: $2.99 to $2.99 so far", text)
        self.assertIn("- Xcode, in 1 session: 2 builds (0.2 hours) and 1 test run, 1 failed (0.5 hours), from "
                      "Xcode's logs; 1 command running xcodebuild or Cut's build scripts. 1 run was in no "
                      "session's folder.", text)
        self.assertIn("75% of the host's Xcode CPU was traced to a session", text)
        self.assertIn("| cut/otter | 2026-11-14 10:00 | 2.0 | 0.8 | 3 | 1 | 1 (1 failed) | 2.0 | 0.7 |", text)
        self.assertIn("| cut, no worktree |", text)
        text.encode("ascii")


def state_with(jobs, last_collect, server="cut"):
    return {"jobs": jobs, "last_collect": last_collect, "sessions": {
        # Eight sessions at once for ten hours, each running xcodebuild.
        str(i): {"server": server, "xcode_cmds": 1, "spans": [["2026-11-14T08:00:00Z", "2026-11-14T18:00:00Z"]]}
        for i in range(8)}}


def mac_jobs(n):
    out = {}
    for i in range(n):
        start = t("2026-11-14T08:00:00Z") + dt.timedelta(minutes=30 * i)
        out[str(i)] = job(created_at=mu.iso(start), started_at=mu.iso(start),
                          completed_at=mu.iso(start + dt.timedelta(minutes=10)))
    return out


class Money(unittest.TestCase):
    def test_cut_sessions_are_priced_apart_from_ci(self):
        jobs = mac_jobs(20)
        with_agents = mu.build_summary(state_with(jobs, "2026-11-15T00:00:00Z"), [], t(SINCE), t(NOW))
        without = mu.build_summary({"jobs": jobs, "last_collect": "2026-11-15T00:00:00Z"}, [], t(SINCE), t(NOW))
        self.assertEqual(with_agents["agents"]["busy_hours"], 10.0)
        m, m0 = with_agents["money"], without["money"]
        # CI's figures don't move; sessions get their own.
        for k in ("value_usd", "payback_months", "gated_payback_months"):
            self.assertEqual(m[k], m0[k])
        self.assertEqual(m["value_usd"], round(200 * mu.HOSTED_USD_PER_MIN, 2))
        days = (t(NOW) - t("2026-11-14T08:00:00Z")).total_seconds() / 86400

        def months(usd):
            return round(mu.PURCHASE_NET_USD / round(usd / max(1.0, days) * mu.DAYS_PER_MONTH, 2), 1)

        self.assertEqual(m["payback_months"], months(m["value_usd"]))
        self.assertIsNone(m["gated_payback_months"])
        # 8 sessions at once for 10 hours: one Mac for 10 hours at the low end, 8 at the high.
        self.assertEqual((m["sessions_low_minutes"], m["sessions_high_minutes"]), (600, 4800))
        self.assertEqual(m["sessions_low_usd"], round(600 * mu.HOSTED_MAC_SESSION_USD_PER_MIN, 2))
        self.assertEqual(m["sessions_high_usd"], round(4800 * mu.HOSTED_MAC_SESSION_USD_PER_MIN, 2))
        self.assertEqual(m["with_sessions_payback_months"],
                         {"low": months(m["value_usd"] + m["sessions_low_usd"]),
                          "high": months(m["value_usd"] + m["sessions_high_usd"])})
        self.assertEqual(m["gated_with_sessions_payback_months"],
                         {"low": months(m["sessions_low_usd"]), "high": months(m["sessions_high_usd"])})
        self.assertEqual((m0["sessions_low_usd"], m0["sessions_high_usd"]), (0, 0))
        text = mu.render_markdown(with_agents)
        self.assertIn("never at this pace", text)
        self.assertIn("Cut's agent sessions that used Xcode or a simulator, 8 of 8: $36.00 to $288.00 so far", text)
        self.assertIn("(10.0 hours), the high end one per session (80.0 session-hours)", text)
        self.assertIn("between turns", text)
        self.assertNotIn("not in the payback", text)

    def test_other_servers_and_earlier_sessions_are_not_priced(self):
        jobs = mac_jobs(20)
        state = state_with(jobs, "2026-11-15T00:00:00Z", server="any")
        # A Cut session before the first Mac Studio job, outside the pace's span.
        state["sessions"]["early"] = {"server": "cut", "xcode_cmds": 1,
                                      "spans": [["2026-11-13T08:00:00Z", "2026-11-13T18:00:00Z"]]}
        # A Cut session that never touched Xcode or a simulator.
        state["sessions"]["chat"] = {"server": "cut", "spans": [["2026-11-14T08:00:00Z", "2026-11-14T09:00:00Z"]]}
        s = mu.build_summary(state, [], t(SINCE), t(NOW))
        m = s["money"]
        self.assertEqual((m["sessions_low_usd"], m["sessions_high_usd"]), (0, 0))
        self.assertEqual((m["sessions_priced"], m["sessions_all"]), (0, 1))
        self.assertEqual(m["sessions_all_high_usd"], round(60 * mu.HOSTED_MAC_SESSION_USD_PER_MIN, 2))
        self.assertEqual({k: s["agents"]["by_server"]["any"][k] for k in ("sessions", "hours", "busy_hours")},
                         {"sessions": 8, "hours": 80.0, "busy_hours": 10.0})

    def test_a_session_across_the_first_job_is_cut_there(self):
        # The first Mac Studio job starts at 08:00.
        state = {"jobs": mac_jobs(20), "last_collect": "2026-11-15T00:00:00Z", "sessions": {
            "a": {"server": "cut", "xcode_cmds": 1, "spans": [["2026-11-14T07:00:00Z", "2026-11-14T09:00:00Z"]]}}}
        m = mu.build_summary(state, [], t(SINCE), t(NOW))["money"]
        self.assertEqual((m["sessions_low_minutes"], m["sessions_high_minutes"]), (60, 60))

    def test_since_after_the_first_job_keeps_the_pace(self):
        # A 10-minute job every 2 hours for 4 days, and a Cut session beside each.
        jobs, sessions = {}, {}
        for i in range(48):
            start = t("2026-11-14T08:00:00Z") + dt.timedelta(hours=2 * i)
            jobs[str(i)] = job(created_at=mu.iso(start), started_at=mu.iso(start),
                               completed_at=mu.iso(start + dt.timedelta(minutes=10)))
            sessions[str(i)] = {"server": "cut", "xcode_cmds": 1,
                                "spans": [[mu.iso(start), mu.iso(start + dt.timedelta(minutes=30))]]}
        state = {"jobs": jobs, "last_collect": "2026-11-18T08:00:00Z", "sessions": sessions}
        now = t("2026-11-18T08:10:00Z")
        full = mu.build_summary(state, [], t(SINCE), now)["money"]
        late = mu.build_summary(state, [], t("2026-11-16T00:00:00Z"), now)["money"]
        self.assertEqual(late["start"], "2026-11-16T00:00:00Z")
        for k in ("value_per_month_usd", "sessions_high_per_month_usd", "payback_months"):
            self.assertAlmostEqual(late[k], full[k], delta=0.02 * full[k])

    def test_headline_with_all_four_paybacks(self):
        jobs = mac_jobs(20)
        # Half of them labelled runs, which GitHub would have run anyway.
        for i in range(10):
            jobs[str(i)]["workflow"] = "PR Tests Rerun"
        s = mu.build_summary(state_with(jobs, "2026-11-15T00:00:00Z"), [], t(SINCE), t("2026-11-15T00:10:00Z"))
        m = s["money"]
        for k in ("payback_months", "gated_payback_months"):
            self.assertIsNotNone(m[k])
        for k in ("with_sessions_payback_months", "gated_with_sessions_payback_months"):
            self.assertIsNotNone(m[k]["low"])
            self.assertLessEqual(m[k]["high"], m[k]["low"])
        headline = mu.render_markdown(s).splitlines()[2]
        self.assertEqual(headline, (
            "At the pace so far it pays back in %s for all the CI it ran, or %s counting only the CI GitHub's "
            "runners would have run anyway. Adding Cut's agent sessions at hosted Macs' price, %s or %s. "
            "Net price $2,909." % (mu.fmt_months(m["payback_months"]), mu.fmt_months(m["gated_payback_months"]),
                                   mu.fmt_range(m["with_sessions_payback_months"]),
                                   mu.fmt_range(m["gated_with_sessions_payback_months"]))))
        self.assertIn(" to ", headline)

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
        self.assertEqual(h["colima_up_pct"], 0)

    def test_colima_vm_not_counted_as_ci(self):
        samples = self.samples()
        samples[0]["groups"]["colima"] = {"n": 1}
        h = mu.summarize_host(samples, t("2026-11-14T10:02:30Z"))
        self.assertEqual(h["vms_avg"], 1.67)
        self.assertEqual(h["colima_up_pct"], 33.3)

    def test_a_stopped_sampler_shows(self):
        now = t("2026-11-14T11:00:00Z")
        h = mu.summarize_host(self.samples(), now)
        self.assertEqual(h["coverage"], round(3 / 61, 3))
        self.assertEqual(h["last_age_min"], 58.0)
        s = mu.build_summary({}, self.samples(), t(SINCE), now)
        self.assertTrue(s["host_stale"])
        self.assertIn("The sampler has stopped.", mu.render_markdown(s))


class Headroom(unittest.TestCase):
    def samples(self, n=5, pressure=None, swapout=None, start="2026-11-14T23:58:00Z", step_min=1):
        base = t(start)
        out = []
        for i in range(n):
            out.append({
                "_t": base + dt.timedelta(minutes=i * step_min), "cpu": {"idle": 80.0},
                "mem": {"used_gb": 40.0 + i % 50, "total_gb": 64.0, "compressed_gb": 2.0,
                        "swapout_gb": (swapout or [0.0] * n)[i]},
                "mem_free_pct": 50 - i % 50, "swap_gb": 0.5, "disk_free_gb": 480.0, "disk_total_gb": 926.4,
                "pressure": (pressure or [1] * n)[i], "sims_booted": i % 5, "boot": 1000,
                "groups": {"vm": {"n": 3, "rss_gb": 17.0}, "colima": {"n": 1},
                           "claude": {"n": 4, "rss_gb": 2.0}, "simulator": {"n": 9, "rss_gb": 3.0}},
            })
        return out

    def days(self, n_days, pressure_at=None):
        """A sample every minute for n_days; pressure_at maps an index to a level."""
        n = n_days * 1440 + 1
        pressure = [1] * n
        for i, level in (pressure_at or {}).items():
            pressure[i] = level
        return self.samples(n=n, pressure=pressure, start="2026-11-14T00:00:00Z")

    def test_enough_when_pressure_stays_normal(self):
        h = mu.summarize_headroom(self.days(3))
        self.assertEqual((h["memory"], h["memory_too_early"]), ("enough", []))
        self.assertEqual((h["disk"], h["disk_too_early"], h["disk_gb_per_day"]), ("enough", [], 0.0))
        self.assertIsNone(h["at_worst"])

    def test_short_data_is_provisional(self):
        h = mu.summarize_headroom(self.samples())
        self.assertEqual(h["memory"], "enough")
        self.assertTrue(h["memory_too_early"])
        out = "\n".join(mu.render_headroom(h))
        self.assertIn("- Memory for 64.0 GB: enough so far, too early to call (0.0 days of pressure samples", out)

    def test_sparse_samples_are_provisional(self):
        # Two samples three days apart span the days, not the minutes.
        samples = self.samples(n=2, step_min=3 * 1440)
        h = mu.summarize_headroom(samples)
        self.assertIn("pressure samples for only 0% of minutes", h["memory_too_early"])

    def test_swap_counter_needs_its_own_days(self):
        samples = self.days(3)
        for r in samples[:-60]:
            del r["mem"]["swapout_gb"]
        samples[-1]["mem"]["swapout_gb"] = 5.0
        h = mu.summarize_headroom(samples)
        self.assertIn("0.0 days of swap writes", h["memory_too_early"])
        # 5 GB in the counter's only hour is about 120 GB a day, not 5.
        self.assertGreater(h["swap_written_per_day_gb"], 100)
        self.assertEqual(h["memory"], "short")

    def test_a_warning_is_tight(self):
        h = mu.summarize_headroom(self.days(3, {100: 2}))
        self.assertEqual(h["memory"], "tight")
        self.assertIn("pressure reached warning in 1 sample, never critical", h["memory_why"][0])

    def test_one_critical_sample_is_short(self):
        h = mu.summarize_headroom(self.days(3, {100: 4}))
        self.assertEqual(h["memory"], "short")
        self.assertEqual(h["memory_why"], ["pressure reached critical in 1 sample"])

    def test_an_hour_a_day_at_warning_is_short(self):
        # 70 minutes a day is under 5% of all minutes, but over 30 in a day.
        at = {}
        for day in range(3):
            for m in range(70):
                at[day * 1440 + 600 + m] = 2
        h = mu.summarize_headroom(self.days(3, at))
        self.assertEqual(h["memory"], "short")
        self.assertIn("70 minutes at warning or worse on 2026-11-14", h["memory_why"][0])
        self.assertEqual((h["longest_not_normal_min"], h["longest_not_normal_from"]), (70, "2026-11-14T10:00:00Z"))

    def test_a_gap_breaks_a_stretch(self):
        samples = self.samples(n=3, pressure=[2, 2, 2])
        samples[2]["_t"] = samples[1]["_t"] + dt.timedelta(hours=10)
        h = mu.summarize_headroom(samples)
        self.assertEqual(h["longest_not_normal_min"], 2)

    def test_swap_writes_across_a_restart(self):
        samples = self.samples(swapout=[1.0, 1.5, 0.1, 0.4, 3.0])
        # A drop: the counter's 0.1 since boot was written in that interval.
        h = mu.summarize_headroom(samples)
        self.assertEqual(h["swap_written_gb"], 3.5)
        self.assertEqual(h["days"]["2026-11-14"]["swap_written_gb"], 0.5)
        self.assertEqual(h["days"]["2026-11-15"]["swap_written_gb"], 3.0)
        # No drop, but kern.boottime changed: all of 3.0 is new.
        samples = self.samples(swapout=[0.0, 0.0, 0.0, 0.2, 3.0])
        samples[4]["boot"] = 2000
        self.assertEqual(mu.summarize_headroom(samples)["swap_written_gb"], 3.2)

    def test_worst_pressure_is_shown_apart_from_the_gb_peak(self):
        samples = self.days(3, {100: 4})
        samples[100]["groups"]["claude"]["n"] = 9
        h = mu.summarize_headroom(samples)
        self.assertEqual(h["at_worst"]["t"], "2026-11-14T01:40:00Z")
        self.assertEqual(h["at_worst"]["claude_procs"], 9)
        self.assertNotEqual(h["at_peak"]["t"], h["at_worst"]["t"])
        out = "\n".join(mu.render_headroom(h))
        self.assertIn("- Worst pressure at 2026-11-14T01:40:00Z", out)
        self.assertIn("pressure critical", out)

    def test_old_samples_without_the_new_readings(self):
        samples = self.samples()
        for r in samples:
            del r["mem"]["swapout_gb"], r["mem"]["total_gb"], r["mem_free_pct"], r["sims_booted"]
            del r["disk_total_gb"], r["boot"]
        h = mu.summarize_headroom(samples)
        self.assertIsNone(h["swap_written_per_day_gb"])
        out = "\n".join(mu.render_headroom(h))
        self.assertIn("no samples with the counter yet", out)
        self.assertIn("- Memory: enough so far", out)
        self.assertIn("- Disk: enough so far", out)
        self.assertNotIn("None", out)

    def test_disk_low_or_falling(self):
        samples = self.days(3)
        samples[3]["disk_free_gb"] = 120.0
        self.assertEqual(mu.summarize_headroom(samples)["disk"], "tight")
        samples[4]["disk_free_gb"] = 60.0
        self.assertEqual(mu.summarize_headroom(samples)["disk"], "short")
        # 480 GB falling 5 GB a day reaches 150 in 66 days: tight.
        samples = self.days(3)
        for i, r in enumerate(samples):
            r["disk_free_gb"] = 495.0 - 5 * i / 1440
        h = mu.summarize_headroom(samples)
        self.assertEqual((h["disk"], h["disk_gb_per_day"]), ("tight", -5.0))
        self.assertIn("150 GB in about 66 days at that rate", h["disk_why"])

    def test_peak_and_days(self):
        h = mu.summarize_headroom(self.samples())
        p = h["at_peak"]
        self.assertEqual((p["t"], p["used_gb"], p["ci_vms"], p["colima"], p["sims_booted"]),
                         ("2026-11-15T00:02:00Z", 44.0, 2, True, 4))
        self.assertEqual(list(h["days"]), ["2026-11-14", "2026-11-15"])
        self.assertEqual(h["days"]["2026-11-15"]["samples"], 3)
        self.assertEqual(h["days"]["2026-11-15"]["vms_max"], 2)

    def test_report_renders(self):
        s = mu.build_summary({}, self.samples(), t(SINCE), t("2026-11-15T00:03:00Z"))
        out = mu.render_markdown(s)
        self.assertIn("because pressure stayed normal in all 5 samples", out)
        self.assertIn("- Disk (926.4 GB): enough so far", out)
        self.assertIn("Claude 2.0 GB (4 processes)", out)
        self.assertIn("compressed 2.0 GB; other about 20.0 GB (by resident memory, approximate)", out)
        self.assertNotIn("None", out)

if __name__ == "__main__":
    unittest.main()
