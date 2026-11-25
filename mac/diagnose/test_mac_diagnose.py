"""Tests for mac-diagnose. Run: python3 -m unittest discover -s mac/diagnose"""

import datetime as dt
import email
import email.policy
import importlib.machinery
import importlib.util
import json
import os
import plistlib
import re
import tempfile
import unittest

_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "mac-diagnose")
_loader = importlib.machinery.SourceFileLoader("mac_diagnose", _path)
_spec = importlib.util.spec_from_loader("mac_diagnose", _loader)
md = importlib.util.module_from_spec(_spec)
_loader.exec_module(md)

UTC = dt.timezone.utc
NOW = dt.datetime(2026, 11, 30, 18, 0, tzinfo=UTC)
GB = 1024 * 1024  # KB in a GB

# Names that must never leave the mac studio: session worktrees and
# DerivedData folders, which carry session and branch names.
SECRET_NAMES = ("fix-login-crash-1a2b", "Cut-abcdefghijk", "kyle-secret-branch")


def fake_du(tree):
    """A du_children stand-in over {path: {child: KB}}."""
    def du(path):
        children = tree.get(path)
        if children is None:
            return None, {}
        return sum(children.values()), dict(children)
    return du


def fake_age(ages):
    return lambda path, now: ages.get(os.path.basename(path), 0.5)


class ParseTests(unittest.TestCase):
    def test_parse_du(self):
        out = "12\t/a/b\n30\t/a\nnot a line\n\t/x\n"
        self.assertEqual(md.parse_du(out), {"/a/b": 12, "/a": 30})

    def test_parse_apfs_picks_the_data_container(self):
        p = {"Containers": [
            {"CapacityCeiling": 5e11, "CapacityFree": 4e11,
             "Volumes": [{"Name": "Other", "Roles": [], "CapacityInUse": 1e10}]},
            {"CapacityCeiling": 994e9, "CapacityFree": 447e9,
             "Volumes": [{"Name": "Nix Store", "Roles": [], "CapacityInUse": 60e9},
                         {"Name": "Data", "Roles": ["Data"], "CapacityInUse": 450e9}]},
        ]}
        a = md.parse_apfs(plistlib.dumps(p))
        self.assertEqual(a["free_gb"], 447.0)
        self.assertEqual(a["container_gb"], 994.0)
        self.assertEqual([v["name"] for v in a["volumes"]], ["Data", "Nix Store"])

    def test_parse_apfs_bad_input(self):
        self.assertIsNone(md.parse_apfs(b"not a plist"))
        self.assertIsNone(md.parse_apfs(plistlib.dumps({"Containers": []})))


class RuntimeTests(unittest.TestCase):
    def test_parse_runtimes(self):
        out = json.dumps({
            "A": {"platformIdentifier": "com.apple.platform.iphonesimulator", "version": "27.1", "sizeBytes": 8 * 1024 ** 3},
            "B": {"platformIdentifier": "com.apple.platform.appletvsimulator", "version": "27.1", "sizeBytes": 4 * 1024 ** 3},
            "C": {"platformIdentifier": "com.apple.platform.iphonesimulator", "version": "26.4", "sizeBytes": 7 * 1024 ** 3},
        })
        row = md.parse_runtimes(out)
        self.assertEqual(row["gb"], 19.0)
        self.assertEqual(row["n"], 3)
        self.assertEqual(row["top"][0], {"name": "iOS 27.1", "gb": 8.0})
        self.assertEqual([t["name"] for t in row["top"]], ["iOS 27.1", "iOS 26.4", "tvOS 27.1"])

    def test_parse_runtimes_bad_input(self):
        self.assertIsNone(md.parse_runtimes(""))
        self.assertIsNone(md.parse_runtimes("[]"))


class ScanTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()

    def test_ages_category_keeps_no_names(self):
        path = os.path.join(self.tmp, "DerivedData")
        os.mkdir(path)
        du = fake_du({path: {SECRET_NAMES[1]: 3 * GB, "Other-zz": 1 * GB, "Old-yy": 2 * GB}})
        age = fake_age({SECRET_NAMES[1]: 0.2, "Other-zz": 3, "Old-yy": 30})
        row = md.scan_category("derived-data", "Xcode DerivedData", path, "ages", None, NOW, du=du, age=age)
        self.assertEqual(row["gb"], 6.0)
        self.assertEqual(row["n"], 3)
        self.assertEqual(row["ages"]["under_1d"], {"n": 1, "gb": 3.0})
        self.assertEqual(row["ages"]["1_to_7d"], {"n": 1, "gb": 1.0})
        self.assertEqual(row["ages"]["over_7d"], {"n": 1, "gb": 2.0})
        self.assertNotIn(SECRET_NAMES[1], json.dumps(row))
        self.assertNotIn("top", row)

    def test_names_category_lists_largest_above_the_floor(self):
        path = os.path.join(self.tmp, "Caches")
        os.mkdir(path)
        kids = {"c%d" % i: (i + 1) * GB for i in range(10)}
        kids["tiny"] = 10
        row = md.scan_category("worker-caches", "Caches", path, "names", "worker-home", NOW, du=fake_du({path: kids}))
        self.assertEqual(len(row["top"]), md.TOP_CHILDREN)
        self.assertEqual(row["top"][0], {"name": "c9", "gb": 10.0})
        self.assertNotIn("tiny", [t["name"] for t in row["top"]])
        self.assertEqual(row["parent"], "worker-home")

    def test_missing_and_unreadable(self):
        row = md.scan_category("x", "X", os.path.join(self.tmp, "nope"), None, None, NOW, du=fake_du({}))
        self.assertTrue(row["missing"])
        row = md.scan_category("x", "X", self.tmp, None, None, NOW, du=fake_du({}))
        self.assertTrue(row["unreadable"])

    def test_paths_are_shortened(self):
        row = md.scan_category("x", "X", md.WORKER + "/nope", None, None, NOW, du=fake_du({}))
        self.assertEqual(row["path"], "~worker/nope")

    def test_worktrees_per_repo_without_names(self):
        code = os.path.join(self.tmp, "code")
        wt = os.path.join(code, "cut", ".claude", "worktrees")
        os.makedirs(wt)
        os.makedirs(os.path.join(code, "homelab"))
        old = md.WORKTREES
        md.WORKTREES = (code, ".claude/worktrees")
        try:
            rows = md.scan_worktrees(NOW, du=fake_du({wt: {SECRET_NAMES[0]: 5 * GB, SECRET_NAMES[2]: 1 * GB}}),
                                     age=fake_age({SECRET_NAMES[2]: 10}))
        finally:
            md.WORKTREES = old
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["repo"], "cut")
        self.assertEqual(rows[0]["gb"], 6.0)
        self.assertEqual(rows[0]["ages"]["over_7d"], {"n": 1, "gb": 1.0})
        for s in SECRET_NAMES:
            self.assertNotIn(s, json.dumps(rows))

    def test_every_category_has_a_routine_row(self):
        """ROUTINE.md's table names each category key, so the reader knows
        what trims it."""
        with open(os.path.join(os.path.dirname(_path), "ROUTINE.md")) as f:
            text = f.read()
        for key in [c[0] for c in md.CATEGORIES] + ["worktrees", "sim-runtimes"]:
            self.assertIn("`%s`" % key, text, key)


class GrowthTests(unittest.TestCase):
    def test_growth_and_leaves(self):
        cats = [{"key": "worker-home", "gb": 300.0},
                {"key": "tart-vms", "gb": 120.0, "parent": "worker-home"},
                {"key": "worktrees", "repo": "cut", "gb": 50.0, "parent": "code"},
                {"key": "code", "gb": 80.0, "parent": "worker-home"},
                {"key": "empty", "gb": None}]
        md.add_growth(cats, {"tart-vms": 100.0, "worktrees/cut": 55.0})
        self.assertEqual(cats[1]["growth_gb"], 20.0)
        self.assertEqual(cats[2]["growth_gb"], -5.0)
        self.assertNotIn("growth_gb", cats[0])
        self.assertEqual([md.cat_id(r) for r in md.leaves(cats)], ["tart-vms", "worktrees/cut"])


class DueTests(unittest.TestCase):
    def setUp(self):
        os.environ["TZ"] = "UTC"
        if hasattr(__import__("time"), "tzset"):
            __import__("time").tzset()

    def test_weekly_once_per_week_after_the_hour(self):
        monday_early = dt.datetime(2026, 11, 30, 3, 17, tzinfo=UTC)
        self.assertIsNone(md.due({}, monday_early, 400))
        monday = dt.datetime(2026, 11, 30, 9, 17, tzinfo=UTC)
        self.assertEqual(md.due({}, monday, 400), "weekly")
        state = md.record_sent({}, "weekly", md.report_id("weekly", monday), monday, [])
        self.assertIsNone(md.due(state, monday + dt.timedelta(hours=6), 400))
        self.assertIsNone(md.due(state, monday + dt.timedelta(days=6), 400))
        self.assertEqual(md.due(state, monday + dt.timedelta(days=7), 400), "weekly")

    def test_low_and_critical_repeat_limits(self):
        state = md.record_sent({}, "weekly", md.report_id("weekly", NOW), NOW, [])
        self.assertEqual(md.due(state, NOW, 140), "low")
        state = md.record_sent(state, "low", "x", NOW, [])
        self.assertIsNone(md.due(state, NOW + dt.timedelta(hours=71), 140))
        self.assertEqual(md.due(state, NOW + dt.timedelta(hours=72), 140), "low")
        # Critical isn't held back by a recent low report.
        self.assertEqual(md.due(state, NOW + dt.timedelta(hours=1), 70), "critical")
        state = md.record_sent(state, "critical", "y", NOW + dt.timedelta(hours=1), [])
        self.assertIsNone(md.due(state, NOW + dt.timedelta(hours=20), 70))

    def test_unknown_free_space_sends_only_weekly(self):
        self.assertEqual(md.due({}, NOW, None), "weekly")

    def test_report_ids(self):
        self.assertEqual(md.report_id("weekly", NOW), "mac-studio-diag-2026-W49")
        self.assertEqual(md.report_id("low", NOW), "mac-studio-diag-2026-11-30T1800-low")

    def test_record_keeps_sizes_for_growth(self):
        s = md.record_sent({"last_weekly_id": "w"}, "manual", "m", NOW,
                           [{"key": "a", "gb": 1.0}, {"key": "worktrees", "repo": "cut", "gb": 2.0}, {"key": "b"}])
        self.assertEqual(s["sizes_gb"], {"a": 1.0, "worktrees/cut": 2.0})
        self.assertEqual(s["last_weekly_id"], "w")
        self.assertEqual(s["last_sent_id"], "m")


class MessageTests(unittest.TestCase):
    def report(self):
        return {"schema": 1, "report_id": "mac-studio-diag-2026-W49", "kind": "weekly", "host": "mac-studio",
                "generated": "2026-11-30T18:00:00Z", "previous_report_id": None,
                "apfs": {"free_gb": 447.0, "container_gb": 994.0,
                         "volumes": [{"name": "Data", "roles": ["Data"], "gb": 450.0}]},
                "snapshots": 2,
                "categories": [{"key": "tart-vms", "label": "CI VM images", "path": "~worker/.tart/vms",
                                "gb": 120.0, "growth_gb": 4.5, "n": 3, "parent": "worker-home"}],
                "usage": {"memory": "enough", "memory_why": ["no samples at warning"],
                          "disk_gb_per_day": -18.5, "disk": "tight"}}

    def test_headers_and_json_block_round_trip(self):
        m = md.build_message("kyle@example.com", self.report())
        raw = m.as_bytes()
        parsed = email.message_from_bytes(raw, policy=email.policy.default)
        self.assertEqual(parsed["From"], "kyle@example.com")
        self.assertEqual(parsed["To"], "kyle@example.com")
        self.assertEqual(parsed["X-Homelab-Report-Id"], "mac-studio-diag-2026-W49")
        self.assertIn("(mac-studio-diag-2026-W49)", parsed["Subject"])
        self.assertTrue(parsed["Subject"].startswith("[homelab] mac studio diagnostics: weekly"))
        body = parsed.get_content()
        self.assertTrue(body.startswith("Report-Id: mac-studio-diag-2026-W49\n"))
        block = re.search(r"^BEGIN HOMELAB REPORT\n(.*)\nEND HOMELAB REPORT$", body, re.S | re.M).group(1)
        self.assertEqual(json.loads(block), self.report())
        self.assertIn("120.0 (+4.5)  CI VM images", body)

    def test_body_is_plain_ascii_with_short_lines(self):
        """So Gmail keeps it as sent: 7-bit, no line over 998 characters."""
        m = md.build_message("kyle@example.com", self.report())
        self.assertEqual(m["Content-Transfer-Encoding"], "7bit")
        self.assertTrue(all(len(line) < 998 for line in m.as_string().splitlines()))


class MailFileTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.path = os.path.join(self.dir, "mail")

    def write(self, text, mode=0o600):
        with open(self.path, "w") as f:
            f.write(text)
        os.chmod(self.path, mode)

    def test_reads_address_and_password(self):
        self.write("address=kyle@example.com\npassword=abcd\n")
        cfg, err = md.read_mail_file(self.path, require_root=False)
        self.assertIsNone(err)
        self.assertEqual(cfg["address"], "kyle@example.com")

    def test_refuses_a_file_others_can_read(self):
        self.write("address=kyle@example.com\npassword=abcd\n", 0o644)
        cfg, err = md.read_mail_file(self.path, require_root=True)
        self.assertIsNone(cfg)
        self.assertIn("root", err)

    def test_refuses_a_symlink(self):
        self.write("address=kyle@example.com\npassword=abcd\n")
        link = self.path + "-link"
        os.symlink(self.path, link)
        cfg, err = md.read_mail_file(link, require_root=False)
        self.assertIsNone(cfg)
        self.assertIn("regular", err)

    def test_needs_both_lines(self):
        self.write("address=not-an-address\npassword=abcd\n")
        self.assertIsNone(md.read_mail_file(self.path, require_root=False)[0])
        self.write("address=kyle@example.com\n")
        self.assertIsNone(md.read_mail_file(self.path, require_root=False)[0])

    def test_missing_file_says_how_to_fix_it(self):
        cfg, err = md.read_mail_file(os.path.join(self.dir, "none"), require_root=False)
        self.assertIn("mac-diagnose login", err)

    def test_smtp_host_is_fixed(self):
        """The host is a constant, so the mail file can't redirect the
        password."""
        self.assertEqual((md.SMTP_HOST, md.SMTP_PORT), ("smtp.gmail.com", 587))


class UsageTests(unittest.TestCase):
    def test_pick_usage_drops_sessions(self):
        summary = {"headroom": {"memory": "tight", "memory_why": ["x"], "disk": "enough",
                                "at_worst": {"t": "T", "pressure": 2, "groups_gb": {"vm": 16.0}, "secret": "s"}},
                   "host": {"cpu_busy_p95": 80.0},
                   "agents": {"sessions": [{"id": SECRET_NAMES[0]}]}}
        u = md.pick_usage(summary)
        self.assertEqual(u["memory"], "tight")
        self.assertEqual(u["at_worst"]["groups_gb"], {"vm": 16.0})
        self.assertNotIn("secret", u["at_worst"])
        self.assertNotIn(SECRET_NAMES[0], json.dumps(u))


if __name__ == "__main__":
    unittest.main()
