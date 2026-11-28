"""Unit tests for voice.py, with a fake synthesizer: no model, no audio libs
beyond numpy. CI runs them inside the built image, so the numpy here is the
pinned one.

    python3 -m unittest discover -s voice
"""

import datetime
import json
import os
import shutil
import tempfile
import unittest

import voice

DATE = "2026-11-28"


def issue(**over):
    i = {
        "date": DATE,
        "title": "Rates hold",
        "summary": "The Bank held rates.",
        "sections": [
            {"title": "Business", "items": [
                {"headline": "Rates held", "text": "At four percent.", "source": "BBC", "url": "https://bbc.co.uk/x"},
                {"headline": "FTSE flat?", "text": "Unchanged.", "source": "FT", "url": "https://ft.com/y"},
            ]},
            {"title": "AI", "items": [
                {"headline": "A model", "text": "Shipped.", "source": "Verge", "url": "https://theverge.com/z"},
            ]},
        ],
    }
    i.update(over)
    return i


def fake_synth():
    # 100 samples per character at 1 kHz: 0.1 s each, easy to check.
    return lambda text: ([0.1] * (100 * len(text)), 1000)


class Base(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        voice.ISSUES_DIR = os.path.join(self.root, "issues")
        voice.AUDIO_DIR = os.path.join(self.root, "audio")
        real = voice.write_mp3

        def fake_mp3(path, samples, rate):
            with open(path, "wb") as f:
                f.write(b"ID3" + bytes(len(samples) % 7))

        voice.write_mp3 = fake_mp3
        self.addCleanup(setattr, voice, "write_mp3", real)
        self.addCleanup(shutil.rmtree, self.root)

    def publish(self, i, date=DATE, raw=None):
        d = os.path.join(voice.ISSUES_DIR, "published", date)
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "script.json"), "w") as f:
            f.write(raw if raw is not None else json.dumps(i))

    def audio(self, *parts):
        return os.path.join(voice.AUDIO_DIR, *parts)


class TestLoadIssue(Base):
    def test_accepts(self):
        self.publish(issue())
        self.assertEqual(voice.load_issue(os.path.join(voice.ISSUES_DIR, "published", DATE, "script.json"), DATE)["title"], "Rates hold")

    def test_refuses(self):
        cases = {
            "extra key": json.dumps({**issue(), "x": 1}),
            "other day": json.dumps(issue(date="2026-11-27")),
            "control": json.dumps(issue(title="a\nb")),
            "not text": json.dumps(issue(summary=5)),
            "no sections": json.dumps(issue(sections=[])),
            "long": json.dumps(issue(sections=[{"title": "S", "items": [{"headline": "h", "text": "a" * 1999}] * 12}])),
            "not json": "nope",
            "list": "[]",
        }
        path = os.path.join(voice.ISSUES_DIR, "published", DATE, "script.json")
        for name, raw in cases.items():
            with self.subTest(name):
                self.publish(None, raw=raw)
                with self.assertRaises(voice.BadIssue):
                    voice.load_issue(path, DATE)


class TestScript(unittest.TestCase):
    def test_ordinals(self):
        self.assertEqual([voice.ordinal_words(n) for n in (1, 2, 3, 11, 20, 21, 28, 30, 31)],
                         ["first", "second", "third", "eleventh", "twentieth", "twenty-first",
                          "twenty-eighth", "thirtieth", "thirty-first"])

    def test_segments(self):
        segs = voice.segments(issue())
        self.assertEqual(segs[0][1], "Good morning. This is your daily brief for Saturday the twenty-eighth of November. Rates hold.")
        self.assertEqual(segs[3], ("item", "Rates held. At four percent.", ("item", 0, 0)))
        self.assertEqual(segs[4][1], "FTSE flat? Unchanged.")
        self.assertEqual([s[0] for s in segs], ["intro", "summary", "section", "item", "item", "section", "item", "outro"])

    def test_lang(self):
        self.assertEqual(voice.lang_for("af_heart"), "en-us")
        self.assertEqual(voice.lang_for("bf_emma"), "en-gb")


class TestRender(unittest.TestCase):
    def test_timings_follow_samples(self):
        segs = voice.segments(issue())
        audio, rate, t = voice.render(segs, fake_synth())
        self.assertEqual(rate, 1000)
        self.assertEqual(t["duration_ms"], len(audio))  # 1 sample per ms at 1 kHz
        self.assertEqual(len(t["sections"]), 2)
        self.assertEqual([len(s["items"]) for s in t["sections"]], [2, 1])
        last = 0
        for s in t["sections"]:
            self.assertGreaterEqual(s["start_ms"], last)
            last = s["start_ms"]
            for it in s["items"]:
                self.assertGreaterEqual(it["start_ms"], last)
                self.assertGreater(it["end_ms"], it["start_ms"])
                last = it["end_ms"]
        self.assertLessEqual(last, t["duration_ms"])
        # The first item is read for exactly as long as its text.
        first = t["sections"][0]["items"][0]
        self.assertEqual(first["end_ms"] - first["start_ms"], 100 * len("Rates held. At four percent."))

    def test_rate_change_is_an_error(self):
        rates = iter([1000, 2000])
        with self.assertRaises(RuntimeError):
            voice.render(voice.segments(issue()), lambda text: ([0.0] * 10, next(rates)))


class TestTick(Base):
    def test_renders_once(self):
        self.publish(issue())
        today = datetime.date(2026, 11, 28)
        self.assertEqual(voice.tick(today, fake_synth), 0)
        self.assertTrue(os.path.isfile(self.audio(DATE, "episode.mp3")))
        with open(self.audio(DATE, "timings.json")) as f:
            self.assertEqual(len(json.load(f)["sections"]), 2)
        calls = []
        voice.tick(today, lambda: calls.append(1) or fake_synth())
        self.assertEqual(calls, [], "rendered a day that already has audio")
        self.assertFalse(os.path.exists(self.audio(f".{DATE}.tmp")))

    def test_nothing_published(self):
        self.assertEqual(voice.tick(datetime.date(2026, 11, 28), fake_synth), 0)
        self.assertEqual(sorted(os.listdir(voice.AUDIO_DIR)), [".voice.lock"])

    def test_refused_issue_writes_failed(self):
        self.publish(issue(title="a\tb"))
        voice.tick(datetime.date(2026, 11, 28), fake_synth)
        with open(self.audio(DATE, "failed.txt")) as f:
            self.assertIn("refused the issue", f.read())

    def test_gives_up_after_max_attempts(self):
        self.publish(issue())

        def broken():
            raise MemoryError("killed")

        today = datetime.date(2026, 11, 28)
        for _ in range(voice.MAX_ATTEMPTS):
            self.assertEqual(voice.tick(today, broken), 1)
            self.assertFalse(os.path.exists(self.audio(DATE)))
        voice.tick(today, broken)
        with open(self.audio(DATE, "failed.txt")) as f:
            self.assertIn("gave up", f.read())

    def test_retention(self):
        for d in ("2026-10-01", "2026-11-20", "notadate"):
            os.makedirs(self.audio(d))
            open(self.audio(d, "episode.mp3"), "wb").close()
            open(self.audio(d, "timings.json"), "w").close()
        voice.tick(datetime.date(2026, 11, 28), fake_synth)
        self.assertFalse(os.path.exists(self.audio("2026-10-01", "episode.mp3")))
        self.assertTrue(os.path.exists(self.audio("2026-10-01", "timings.json")))
        self.assertTrue(os.path.exists(self.audio("2026-11-20", "episode.mp3")))
        self.assertTrue(os.path.exists(self.audio("notadate", "episode.mp3")))


if __name__ == "__main__":
    unittest.main()
