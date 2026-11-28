"""brief-voice: read each published issue aloud with Kokoro, offline.

One tick of the voice job, run by supercronic every few minutes in the
brief-voice container. That container has no network and no credentials: it
runs third-party ML code (onnxruntime, phonemizer, espeak-ng) over text that
the open web influenced, so it is given nothing worth taking and no way to
send anything anywhere. Its only output is /audio/<date>/, which the
publisher checks again before it trusts any of it.

For each recent day with a published issue and no audio yet, it writes:

  episode.mp3    the brief, read aloud
  timings.json   where each section and item starts and ends, in
                 milliseconds, from sample counts; the publisher turns this
                 into Spotify chapters and in-player links

into /audio/.<date>.tmp/ and renames that to /audio/<date>/, so the publisher
never sees half an episode. A day that fails MAX_ATTEMPTS times gets
/audio/<date>/failed.txt instead, and the email says there is no podcast.

Run it with `python -I`: isolated mode, so nothing in the working directory or
the environment can put a module ahead of the real ones.
"""

import datetime
import fcntl
import json
import os
import re
import shutil
import sys
import tempfile
import time

ISSUES_DIR = os.environ.get("ISSUES_DIR", "/issues")
AUDIO_DIR = os.environ.get("AUDIO_DIR", "/audio")
MODEL_DIR = os.environ.get("KOKORO_DIR", "/opt/kokoro")
VOICE = os.environ.get("BRIEF_VOICE", "af_heart")
SPEED = float(os.environ.get("BRIEF_VOICE_SPEED", "1.0"))
LOOKBACK_DAYS = int(os.environ.get("LOOKBACK_DAYS", "2"))
RETENTION_DAYS = int(os.environ.get("AUDIO_RETENTION_DAYS", "30"))

MAX_ATTEMPTS = 3

ISSUE_KEYS = {"date", "title", "summary", "sections"}
SECTION_KEYS = {"title", "items"}
ITEM_KEYS = {"headline", "text", "source", "url"}
MAX_ISSUE_BYTES = 256 << 10
# A little above issue.go's maxSpoken (18,000), so that the Go side is the one
# that refuses an over-long issue and says why. This one is only the bound on
# how long a render can run.
MAX_SPOKEN = 20000

DATE_RE = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}$")

# Pauses, in milliseconds, after each kind of line.
PAUSE = {"intro": 700, "summary": 1200, "section": 600, "item": 900, "outro": 0}


def log(msg):
    print(f"brief-voice: {msg}", file=sys.stderr, flush=True)


def valid_date(s):
    if not isinstance(s, str) or not DATE_RE.match(s):
        return False
    try:
        datetime.date.fromisoformat(s)
    except ValueError:
        return False
    return True


class BadIssue(Exception):
    pass


def _str(obj, key, limit):
    v = obj.get(key)
    if not isinstance(v, str) or not v or len(v) > limit:
        raise BadIssue(f"{key} is missing, not text, or over {limit} characters")
    if any(c < " " or c == "\x7f" for c in v):
        raise BadIssue(f"{key} holds a control character")
    return v


def load_issue(path, date):
    """Read and check an issue, defensively and independently of issue.go.

    This is not a second copy of the Go validator. It checks only what this
    job relies on: the shape it walks, plain text, and a bound on how much it
    will read aloud. Links are not read aloud and not checked here.
    """
    with open(path, "rb") as f:
        raw = f.read(MAX_ISSUE_BYTES + 1)
    if len(raw) > MAX_ISSUE_BYTES:
        raise BadIssue("file too large")
    try:
        issue = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise BadIssue("not UTF-8 JSON")
    # Exact key sets at every level, as issue.go's exactKeys checks: the Go
    # reader matches keys without regard to case, so a "Text" beside a "text"
    # would otherwise be read as one thing there and another here.
    if not isinstance(issue, dict) or set(issue) != ISSUE_KEYS:
        raise BadIssue("not an issue")
    if issue["date"] != date:
        raise BadIssue("issue is for another day")
    spoken = len(_str(issue, "title", 200)) + len(_str(issue, "summary", 1000))
    sections = issue["sections"]
    if not isinstance(sections, list) or not 1 <= len(sections) <= 12:
        raise BadIssue("sections")
    for s in sections:
        if not isinstance(s, dict) or set(s) != SECTION_KEYS or not isinstance(s["items"], list) \
                or not 1 <= len(s["items"]) <= 12:
            raise BadIssue("section shape")
        spoken += len(_str(s, "title", 100))
        for it in s["items"]:
            if not isinstance(it, dict) or set(it) != ITEM_KEYS:
                raise BadIssue("item shape")
            spoken += len(_str(it, "headline", 200)) + len(_str(it, "text", 2000))
    if spoken > MAX_SPOKEN:
        raise BadIssue(f"{spoken} characters to read, the most is {MAX_SPOKEN}")
    return issue


_ORDINALS = (
    "first second third fourth fifth sixth seventh eighth ninth tenth eleventh twelfth "
    "thirteenth fourteenth fifteenth sixteenth seventeenth eighteenth nineteenth twentieth"
).split()


def ordinal_words(n):
    """1 -> "first", 28 -> "twenty-eighth". Days of the month only."""
    if 1 <= n <= 20:
        return _ORDINALS[n - 1]
    tens = {2: "twenty", 3: "thirty"}[n // 10]
    if n % 10 == 0:
        return {20: "twentieth", 30: "thirtieth"}[n]
    return f"{tens}-{_ORDINALS[n % 10 - 1]}"


def sentence(s):
    """End a line with a full stop if it has no closing punctuation, so the
    voice falls at the end of a headline rather than running on."""
    return s if s[-1] in ".!?…\"'”’" else s + "."


def segments(issue):
    """The script, as (kind, text, marker) in reading order. marker is
    ("section", i), ("item", i, j) or None."""
    day = datetime.date.fromisoformat(issue["date"])
    when = f"{day.strftime('%A')} the {ordinal_words(day.day)} of {day.strftime('%B')}"
    out = [
        ("intro", f"Good morning. This is your daily brief for {when}. {sentence(issue['title'])}", None),
        ("summary", issue["summary"], None),
    ]
    for i, s in enumerate(issue["sections"]):
        out.append(("section", sentence(s["title"]), ("section", i)))
        for j, it in enumerate(s["items"]):
            out.append(("item", f"{sentence(it['headline'])} {it['text']}", ("item", i, j)))
    out.append(("outro", "That's the brief for today.", None))
    return out


def render(segs, synth):
    """Speak each segment with synth(text) -> (samples, rate) and join them
    with pauses. Returns (samples, rate, timings) with timings in the shape
    timings.go reads."""
    import numpy as np  # here, so the unit tests need no numpy

    chunks, n, rate = [], 0, None
    sections = []
    for kind, text, marker in segs:
        samples, sr = synth(text)
        if rate is None:
            rate = sr
        elif sr != rate:
            raise RuntimeError("sample rate changed mid-episode")
        ms = lambda k: k * 1000 // rate  # noqa: E731
        start = n
        chunks.append(np.asarray(samples, dtype=np.float32))
        n += len(samples)
        if marker and marker[0] == "section":
            sections.append({"start_ms": ms(start), "items": []})
        elif marker and marker[0] == "item":
            sections[marker[1]]["items"].append({"start_ms": ms(start), "end_ms": ms(n)})
        pause = rate * PAUSE[kind] // 1000
        if pause:
            chunks.append(np.zeros(pause, dtype=np.float32))
            n += pause
    audio = np.concatenate(chunks) if chunks else np.zeros(0, dtype=np.float32)
    return audio, rate, {"duration_ms": n * 1000 // rate, "sections": sections}


def lang_for(voice):
    # Kokoro voice names start with a language letter: a is American English,
    # b is British English (bf_emma, bm_george).
    return "en-gb" if voice.startswith("b") else "en-us"


def kokoro_synth():
    from kokoro_onnx import Kokoro

    k = Kokoro(os.path.join(MODEL_DIR, "kokoro-v1.0.onnx"), os.path.join(MODEL_DIR, "voices-v1.0.bin"))
    lang = lang_for(VOICE)
    return lambda text: k.create(text, voice=VOICE, speed=SPEED, lang=lang)


def write_mp3(path, samples, rate):
    import soundfile as sf

    sf.write(path, samples, rate, format="MP3", subtype="MPEG_LAYER_III")


def publish_dir(date, files):
    """Write files ({name: bytes}) as AUDIO_DIR/<date>/ in one rename.

    Either outcome ends the day's attempt count, so moving AUDIO_DIR/<date>/
    aside renders the day again from a count of zero."""
    final = os.path.join(AUDIO_DIR, date)
    tmp = os.path.join(AUDIO_DIR, f".{date}.tmp")
    shutil.rmtree(tmp, ignore_errors=True)
    os.makedirs(tmp)
    for name, data in files.items():
        with open(os.path.join(tmp, name), "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
    os.rename(tmp, final)
    try:
        os.remove(os.path.join(AUDIO_DIR, ".attempts", date))
    except FileNotFoundError:
        pass


def bump_attempts(date):
    """Count this attempt before it starts, so a render the kernel kills for
    memory still counts and the job cannot loop on one issue forever."""
    d = os.path.join(AUDIO_DIR, ".attempts")
    os.makedirs(d, exist_ok=True)
    path = os.path.join(d, date)
    try:
        with open(path) as f:
            n = int(f.read().strip() or 0)
    except (FileNotFoundError, ValueError):
        n = 0
    n += 1
    with open(path + ".tmp", "w") as f:
        f.write(str(n))
    os.replace(path + ".tmp", path)
    return n


def voice_day(date, make_synth):
    if os.path.exists(os.path.join(AUDIO_DIR, date)):
        return
    script = os.path.join(ISSUES_DIR, "published", date, "script.json")
    if not os.path.isfile(script):
        return  # not written yet, or the writer failed (failed.txt)
    attempt = bump_attempts(date)
    if attempt > MAX_ATTEMPTS:
        publish_dir(date, {"failed.txt": f"gave up after {MAX_ATTEMPTS} attempts\n".encode()})
        log(f"{date}: gave up after {MAX_ATTEMPTS} attempts")
        return
    try:
        issue = load_issue(script, date)
    except BadIssue as e:
        publish_dir(date, {"failed.txt": f"refused the issue: {e}\n".encode()})
        log(f"{date}: refused the issue: {e}")
        return
    log(f"{date}: rendering (attempt {attempt})")
    t0 = time.monotonic()
    samples, rate, timings = render(segments(issue), make_synth())
    tmp_mp3 = os.path.join(AUDIO_DIR, f".{date}.mp3.tmp")
    write_mp3(tmp_mp3, samples, rate)
    try:
        with open(tmp_mp3, "rb") as f:
            mp3 = f.read()
    finally:
        os.remove(tmp_mp3)
    publish_dir(date, {"episode.mp3": mp3, "timings.json": json.dumps(timings).encode()})
    log(f"{date}: {timings['duration_ms'] // 1000}s of audio in {time.monotonic() - t0:.0f}s")


def retention(today):
    """Delete old episodes' MP3s. Only directories named as dates are
    touched, and only their episode.mp3: the small files stay as a record."""
    if RETENTION_DAYS <= 0:
        return
    cutoff = (today - datetime.timedelta(days=RETENTION_DAYS)).isoformat()
    for name in os.listdir(AUDIO_DIR):
        if valid_date(name) and name < cutoff:
            mp3 = os.path.join(AUDIO_DIR, name, "episode.mp3")
            if os.path.isfile(mp3) and not os.path.islink(mp3):
                os.remove(mp3)
                log(f"{name}: deleted the MP3 (older than {RETENTION_DAYS} days)")


def tick(today, make_synth=kokoro_synth):
    os.makedirs(AUDIO_DIR, exist_ok=True)
    with open(os.path.join(AUDIO_DIR, ".voice.lock"), "w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            log("a render is still running; skipping this tick")
            return 0
        failed = 0
        for back in range(LOOKBACK_DAYS - 1, -1, -1):
            date = (today - datetime.timedelta(days=back)).isoformat()
            try:
                voice_day(date, make_synth)
            except Exception as e:  # noqa: BLE001 - one day's failure must not stop the next
                log(f"{date}: render failed: {type(e).__name__}: {e}")
                failed += 1
        retention(today)
        return 1 if failed else 0


def self_test():
    """Read one sentence with the real model and write it as MP3. The image
    build runs this, so a model, a wheel and espeak-ng that do not work
    together fail the build rather than the first morning."""
    samples, rate = kokoro_synth()("This is a test of the daily brief.")
    fd, path = tempfile.mkstemp(suffix=".mp3")
    os.close(fd)
    try:
        write_mp3(path, samples, rate)
        size = os.path.getsize(path)
    finally:
        os.remove(path)
    if len(samples) < rate or size < 4096:
        log(f"self-test FAILED: {len(samples)} samples at {rate} Hz, {size} bytes of MP3")
        return 1
    log(f"self-test: {len(samples) / rate:.1f}s of audio, {size} bytes of MP3")
    return 0


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        sys.exit(self_test())
    sys.exit(tick(datetime.date.today()))
