"""Tests for mac-diag. Run: python3 -m unittest discover -s mac/diag-mcp"""

import argparse
import datetime as dt
import importlib.machinery
import importlib.util
import json
import os
import stat
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))


def _load(name, path):
    loader = importlib.machinery.SourceFileLoader(name, path)
    spec = importlib.util.spec_from_loader(name, loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    return mod


mc = _load("mac_diag", os.path.join(HERE, "mac-diag"))
# The real mac/diagnose, as default.nix substitutes its path: the collector
# reuses its scan and names, so the tests do too.
md = mc.load_diagnose(os.path.join(HERE, "..", "diagnose", "mac-diagnose"))

UTC = dt.timezone.utc
NOW = dt.datetime(2026, 11, 28, 15, 0, tzinfo=UTC)


def ago(**kw):
    return mc.iso(NOW - dt.timedelta(**kw))


class HostSample(unittest.TestCase):
    def test_newest_line_without_sessions_or_pids(self):
        with tempfile.TemporaryDirectory() as d:
            rec = {"t": ago(minutes=1), "load": [1, 2, 3], "pressure": 2, "boot": int(NOW.timestamp()) - 3 * 86400,
                   "by_session": {"fix-login-crash-1a2b": {"mem": 3}},
                   "vms": [{"name": "ci-1", "pid": 123, "rss_gb": 8}], "secret": "x"}
            with open(os.path.join(d, "host-2026-11-27.jsonl"), "w") as f:
                f.write(json.dumps({"t": ago(days=1)}) + "\n")
            with open(os.path.join(d, "host-2026-11-28.jsonl"), "w") as f:
                f.write(json.dumps({"t": ago(minutes=2)}) + "\n" + json.dumps(rec) + "\n" + "{torn")
            with open(os.path.join(d, "other.jsonl"), "w") as f:
                f.write("{}\n")
            out = mc.host_sample(NOW, d)
        self.assertEqual(out["load"], [1, 2, 3])
        self.assertEqual(out["pressure_level"], "warning")
        self.assertEqual(out["uptime_days"], 3.0)
        self.assertEqual(out["age_s"], 60)
        self.assertEqual(out["vms"], [{"name": "ci-1", "rss_gb": 8}])
        self.assertNotIn("by_session", out)
        self.assertNotIn("secret", out)
        self.assertNotIn("fix-login-crash", json.dumps(out))

    def test_no_samples(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertIn("error", mc.host_sample(NOW, d))
        self.assertIn("error", mc.host_sample(NOW, "/nonexistent"))


class CISlots(unittest.TestCase):
    def test_phase_status_and_hold(self):
        with tempfile.TemporaryDirectory() as d:
            t = int(NOW.timestamp()) - 600
            files = {
                "runner-1.phase": "running %d\n" % t,
                "runner-1.status": "error %d branch fix-login-crash-1a2b failed\n" % t,
                "runner-2.phase": "$(rm -rf /) 1\n",
                "drain-1.cut": str(int(NOW.timestamp()) + 3600),
                "drain-2": str(int(NOW.timestamp()) - 1),
            }
            for n, v in files.items():
                with open(os.path.join(d, n), "w") as f:
                    f.write(v)
            rows = mc.ci_slots(["1", "2", "3"], d, NOW, launchd_state=lambda s: "running")
        one, two, three = rows
        self.assertEqual((one["phase"], one["phase_minutes"], one["status"], one["held"], one["runner"]),
                         ("running", 10, "error", True, "running"))
        self.assertNotIn("fix-login-crash", json.dumps(rows))
        self.assertNotIn("phase", two)
        self.assertFalse(two["held"])
        self.assertEqual(three, {"slot": "3", "held": False, "runner": "running"})


class TartImages(unittest.TestCase):
    def test_folders_with_config_and_names_guarded(self):
        with tempfile.TemporaryDirectory() as d:
            for n in ("cut-ci", "cut-ci-new", "half-made", "bad\nname"):
                os.mkdir(os.path.join(d, n))
                if n != "half-made":
                    open(os.path.join(d, n, "config.json"), "w").close()
            self.assertEqual(mc.tart_images(md.SAFE_NAME_RE, d), ["(other)", "cut-ci", "cut-ci-new"])
        self.assertIsNone(mc.tart_images(md.SAFE_NAME_RE, "/nonexistent"))


class Docker(unittest.TestCase):
    def test_sizes(self):
        self.assertEqual(mc.docker_gb("1.5GB"), 1.5)
        self.assertEqual(mc.docker_gb("512MB (40%)"), 0.51)
        self.assertEqual(mc.docker_gb("0B"), 0.0)
        self.assertIsNone(mc.docker_gb("lots"))
        self.assertIsNone(mc.docker_gb(None))

    def test_system_df(self):
        out = "\n".join([
            json.dumps({"Type": "Images", "TotalCount": "12", "Active": "4", "Size": "20.5GB", "Reclaimable": "8GB (39%)"}),
            json.dumps({"Type": "Build Cache", "TotalCount": "30", "Active": "0", "Size": "3.2GB", "Reclaimable": "3.2GB"}),
            json.dumps({"Type": "Something new", "Size": "1GB"}),
            "not json",
        ])
        rows = mc.parse_docker_df(out)
        self.assertEqual(rows["images"], {"count": 12, "active": 4, "gb": 20.5, "reclaimable_gb": 8.0})
        self.assertEqual(rows["build_cache"]["gb"], 3.2)
        self.assertEqual(set(rows), {"images", "build_cache"})

    def test_compose_projects(self):
        out = "cut\trunning\ncut\texited\n\trunning\nfix-login-crash-1a2b\trunning\nbad;name\texited\n"
        rows = mc.parse_compose_projects(out, md.SAFE_NAME_RE)
        self.assertEqual(rows["cut"], {"containers": 2, "running": 1})
        self.assertEqual(rows["(none)"], {"containers": 1, "running": 1})
        self.assertEqual(rows["(other)"], {"containers": 1, "running": 0})

    def test_pg_sizes_never_names(self):
        gb = 1024 ** 3
        out = "\n".join([
            "postgres|%d" % (gb // 100),
            "cut_wt_0123abcd|%d" % gb,
            "cut_wt_89abcdef|%d" % gb,
            "tests_0123abcd_worker_1|%d" % (gb // 2),
            "tests_0123abcd_worker_2|%d" % (gb // 2),
            "tests_89abcdef_worker_1|%d" % (gb // 2),
            "kyle_secret_branch|%d" % gb,
            "garbage",
        ])
        kinds = mc.parse_pg_sizes(out)
        self.assertEqual(kinds["worktree"], {"count": 2, "gb": 2.0})
        self.assertEqual(kinds["test"], {"count": 3, "gb": 1.5, "checkouts": 2})
        self.assertEqual(kinds["other"]["count"], 2)
        text = json.dumps(kinds)
        for name in ("0123abcd", "kyle_secret", "postgres"):
            self.assertNotIn(name, text)


class Colima(unittest.TestCase):
    def test_list(self):
        out = json.dumps({"name": "other", "status": "Running"}) + "\n" + json.dumps(
            {"name": "default", "status": "Running", "cpus": 6, "memory": 16 * 1024 ** 3, "disk": 200 * 1024 ** 3})
        self.assertEqual(mc.parse_colima(out), {"status": "running", "cpus": 6, "memory_gb": 16.0, "disk_gb": 200.0})
        self.assertIsNone(mc.parse_colima("nope"))
        self.assertIsNone(mc.parse_colima(json.dumps({"name": "default", "status": "Running; rm"}))["status"])

    def test_df(self):
        out = ("Filesystem 1024-blocks Used Available Capacity Mounted on\n"
               "/dev/vda1 10485760 5242880 5242880 50% /\n"
               "/dev/vdb1 209715200 104857600 104857600 50% /mnt/lima-colima\n"
               "tmpfs 1024 0 1024 0% /run\n"
               "/dev/vdc 1024 0 1024 0% /Users/worker/secret-project\n")
        self.assertEqual(mc.parse_df(out), [
            {"mount": "/", "size_gb": 10.0, "used_gb": 5.0},
            {"mount": "/mnt/lima-colima", "size_gb": 200.0, "used_gb": 100.0},
        ])


class Due(unittest.TestCase):
    def test_fast(self):
        self.assertTrue(mc.fast_due({}, NOW, False))
        self.assertFalse(mc.fast_due({"fast": {"collected": ago(seconds=200)}}, NOW, False))
        self.assertTrue(mc.fast_due({"fast": {"collected": ago(seconds=280)}}, NOW, False))
        self.assertFalse(mc.fast_due({"fast": {"collected": ago(seconds=10)}}, NOW, True))
        self.assertTrue(mc.fast_due({"fast": {"collected": ago(seconds=30)}}, NOW, True))

    def test_disk(self):
        self.assertTrue(mc.disk_due({}, NOW, False))
        self.assertFalse(mc.disk_due({"disk": {"collected": ago(hours=1)}}, NOW, False))
        self.assertTrue(mc.disk_due({"disk": {"collected": ago(hours=1)}}, NOW, True))
        self.assertFalse(mc.disk_due({"disk": {"collected": ago(minutes=5)}}, NOW, True))
        self.assertTrue(mc.disk_due({"disk": {"collected": ago(hours=7)}}, NOW, False))
        # A scan under way blocks a second; one left behind by a crash does not.
        running = {"disk_scan": {"running_since": ago(minutes=10)}}
        self.assertFalse(mc.disk_due(running, NOW, True))
        self.assertTrue(mc.disk_due({"disk_scan": {"running_since": ago(hours=3)}}, NOW, True))

    def test_usage(self):
        self.assertTrue(mc.usage_due({}, NOW))
        self.assertFalse(mc.usage_due({"usage": {"collected": ago(minutes=30)}}, NOW))
        self.assertTrue(mc.usage_due({"usage": {"collected": ago(minutes=61)}}, NOW))


class Queue(unittest.TestCase):
    def test_drain_by_name_only(self):
        with tempfile.TemporaryDirectory() as d:
            q = os.path.join(d, "queue")
            os.mkdir(q)
            outside = os.path.join(d, "outside")
            os.mkdir(outside)
            open(os.path.join(outside, "keep"), "w").close()
            with open(os.path.join(q, "disk"), "w") as f:
                f.write("rm -rf /")
            open(os.path.join(q, "fast"), "w").close()
            open(os.path.join(q, "everything"), "w").close()
            os.mkdir(os.path.join(q, "a-dir"))
            os.symlink(outside, os.path.join(q, "link"))
            self.assertEqual(mc.drain_queue(q), {"disk", "fast"})
            self.assertEqual(os.listdir(q), [])
            # The symlink went, not what it points at.
            self.assertTrue(os.path.exists(os.path.join(outside, "keep")))
        self.assertEqual(mc.drain_queue("/nonexistent"), set())


class Snapshot(unittest.TestCase):
    def test_write_and_read(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "snapshot.json")
            mc.write_snapshot({"schema": mc.SCHEMA, "fast": {}}, p)
            self.assertEqual(stat.S_IMODE(os.stat(p).st_mode), 0o640)
            self.assertEqual(mc.read_snapshot(p)["schema"], mc.SCHEMA)
            self.assertEqual([n for n in os.listdir(d) if n.startswith(".snapshot.")], [])
            with open(p, "w") as f:
                json.dump({"schema": 99}, f)
            self.assertEqual(mc.read_snapshot(p), {})
            with open(p, "w") as f:
                f.write("{torn")
            self.assertEqual(mc.read_snapshot(p), {})


class FakeDiag:
    """mac/diagnose's names, with scan and usage_summary standing in."""
    SAFE_NAME_RE = md.SAFE_NAME_RE
    REPORT_DAYS = md.REPORT_DAYS
    cat_id = staticmethod(md.cat_id)
    add_growth = staticmethod(md.add_growth)
    add_own = staticmethod(md.add_own)
    leaves = staticmethod(md.leaves)

    def __init__(self):
        self.scans = 0

    def scan(self, now):
        self.scans += 1
        return {"apfs": {"free_gb": 480}, "snapshots": [],
                "categories": [{"key": "tart-vms", "label": "CI VMs", "gb": 130.0},
                               {"key": "derived-data", "label": "DerivedData", "gb": 18.0}]}

    def usage_summary(self, now):
        return {"memory": "ok", "disk": "watch"}


class Run(unittest.TestCase):
    def run_once(self, d, now, diag, queue=()):
        q = os.path.join(d, "queue")
        os.makedirs(q, exist_ok=True)
        for name in queue:
            open(os.path.join(q, name), "w").close()
        writes = []
        snap_path = os.path.join(d, "snapshot.json")

        def write(s):
            writes.append(json.loads(json.dumps(s)))
            mc.write_snapshot(s, snap_path)

        with mock.patch.object(mc, "QUEUE", q), mock.patch.object(mc, "SNAPSHOT", snap_path), \
                mock.patch.object(mc, "collect_fast", lambda now, diag, slots: {"collected": mc.iso(now), "slots": slots}), \
                mock.patch("sys.stdout"):
            mc.cmd_run(argparse.Namespace(slots="1,2,x"), now=now, diag=diag, write=write)
        return writes

    def test_first_run_then_requests(self):
        diag = FakeDiag()
        with tempfile.TemporaryDirectory() as d:
            writes = self.run_once(d, NOW, diag)
            # Once with the scan marked as running, once with it done.
            self.assertEqual(len(writes), 2)
            self.assertIn("disk_scan", writes[0])
            last = writes[1]
            self.assertNotIn("disk_scan", last)
            self.assertEqual(last["fast"]["slots"], ["1", "2"])
            self.assertEqual(last["usage"]["summary"]["disk"], "watch")
            self.assertEqual(last["disk"]["top"][0]["key"], "tart-vms")
            self.assertIsNone(last["disk"]["growth_since"])

            # A minute later nothing is due.
            self.assertEqual(self.run_once(d, NOW + dt.timedelta(minutes=1), diag), [])

            # Asked for a disk scan 20 minutes on: a new scan, with growth.
            later = NOW + dt.timedelta(minutes=20)
            writes = self.run_once(d, later, diag, queue=("disk",))
            self.assertEqual(diag.scans, 2)
            self.assertEqual(writes[-1]["disk"]["growth_since"], mc.iso(NOW))
            self.assertEqual(writes[-1]["fast"]["collected"], mc.iso(later))
            self.assertEqual(os.listdir(os.path.join(d, "queue")), [])

    def test_scan_failure_clears_marker(self):
        diag = FakeDiag()
        diag.scan = mock.Mock(side_effect=RuntimeError("du failed"))
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(RuntimeError):
                self.run_once(d, NOW, diag)
            # The marker went to disk before the scan; the next run's read
            # sees it and does not take a crashed scan for a running one
            # longer than DISK_SCAN_MAX_MIN.
            snap = mc.read_snapshot(os.path.join(d, "snapshot.json"))
            self.assertIn("disk_scan", snap)
            self.assertTrue(mc.disk_due(snap, NOW + dt.timedelta(minutes=mc.DISK_SCAN_MAX_MIN + 1), False))


class Login(unittest.TestCase):
    def test_settings_round_trip(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "homelab", "diag-mcp")
            mc.write_settings("https://auth.example.com", "user_01ABCDEFGHJK", p)
            self.assertEqual(stat.S_IMODE(os.stat(p).st_mode), 0o640)
            self.assertEqual(mc.read_settings(p), {"OAUTH_ISSUER": "https://auth.example.com",
                                                   "OAUTH_ALLOWED_SUBJECTS": "user_01ABCDEFGHJK"})
        self.assertEqual(mc.read_settings("/nonexistent"), {})

    def test_ask(self):
        with mock.patch("builtins.input", return_value=""):
            self.assertEqual(mc.ask("x", "https://auth.example.com", mc.ISSUER_RE, "a URL"), "https://auth.example.com")
        # A value that would add a line, or point the server elsewhere.
        for bad in ("http://auth.example.com", "https://auth.example.com\nOAUTH_RESOURCE=x",
                    "https://a.example.com/x?y=1"):
            with mock.patch("builtins.input", return_value=bad), self.assertRaises(SystemExit):
                mc.ask("x", None, mc.ISSUER_RE, "a URL")
        for bad in ("user_short", "user_01ABCDEFGHJK,user_01ZYXWVUTSRQ", "admin"):
            with mock.patch("builtins.input", return_value=bad), self.assertRaises(SystemExit):
                mc.ask("x", None, mc.SUBJECT_RE, "an id")

    def test_needs_root(self):
        if os.geteuid() == 0:
            self.skipTest("running as root")
        with self.assertRaises(SystemExit):
            mc.cmd_login(None)


class Placeholders(unittest.TestCase):
    def test_default_nix_fills_every_one(self):
        # check-mac.yml evaluates the config on Linux but cannot build the
        # mac studio's copy of this script, where --replace-fail would catch
        # a placeholder left out.
        import re
        with open(os.path.join(HERE, "default.nix")) as f:
            filled = set(re.findall(r"replace-fail '(\"@\w+@\")'", f.read()))
        with open(os.path.join(HERE, "mac-diag")) as f:
            used = set(re.findall(r'"@\w+@"', f.read()))
        self.assertEqual(filled, used)


if __name__ == "__main__":
    unittest.main()
