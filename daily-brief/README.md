# daily-brief

A news brief written each morning by Claude Code on the NAS, read aloud by a
local voice model, uploaded to your own Spotify show and emailed to you with a
link to the episode.

What it covers is up to you: `interests.md`, beside `docker-compose.yml`,
names the sections and what you follow. It is gitignored, because this
repository is public. `interests.example.md` is a starting point.

## How it works

Three containers from one image, each running one job on a schedule with
supercronic:

1. **brief-agent** runs `scripts/write.sh` at 05:30 (and again at 08:30, which
   does nothing if 05:30 worked). That starts `claude -p` with web search and
   web fetch, which reads the news and writes `drafts/<date>.json`. The
   script checks it with `daily-brief validate`, gives the agent up to two
   fresh sessions to fix what was refused, and moves the passing copy into
   `published/<date>/script.json`.
2. **brief-voice** looks every five minutes for a published issue with no
   audio, reads it aloud with [Kokoro](https://huggingface.co/hexgrad/Kokoro-82M)
   and writes `episode.mp3` and `timings.json`: where each section and story
   starts and ends.
3. **brief-publisher** looks every five minutes too. It uploads the episode to
   your show with Spotify's [`save-to-spotify`](https://github.com/spotify/save-to-spotify)
   CLI, waits until Spotify has processed it, sets a chapter per section and an
   in-player link per story to its source, then sends the email through the
   Gmail API: the brief as text and HTML, with a "Listen on Spotify" link.

The email waits for the podcast up to `PODCAST_WAIT` (4 hours) after the
issue is written, then goes without it and says why. An episode that comes
later is still uploaded. Every step is recorded in `state/issues/<date>.json`
before the next starts, so a restart repeats nothing that reaches the
outside: no second email and no second upload.

A day that cannot be written still gets an email: a short notice with the
reason, such as the token having expired. Set `HC_PING_URL` to a
[healthchecks.io](https://healthchecks.io) check with a one-day period to hear
when no email went out at all.

Episodes older than `RETENTION_DAYS` (21) are deleted from the show, because
Spotify's free tier keeps 30 per show.

## Trust boundary

The agent reads pages written by strangers, which makes it the one part to
design around. Prompt injection needs three things together: private data,
untrusted content and a way out. The agent has the last two by design, so it
is kept away from the first.

| Container | Reads | Writes | Network | Holds |
|---|---|---|---|---|
| `brief-agent` | issues, `interests.md` | `issues/drafts/` only | yes | a model-only token |
| `brief-voice` | issues | audio | **none** | nothing |
| `brief-publisher` | issues, audio, state | state | yes | the Gmail grant, the Spotify login |

- **The agent** runs with `--restricted` and the tools Read, Glob, Grep, Write,
  Edit, WebSearch and WebFetch only: no Bash, no MCP servers, no subagents, and
  file tools confined to the issues directory. `agent-settings.json` narrows
  writes to `drafts/` and denies reading `/proc`, which holds its token.
  `write.sh` refuses to start if `/state` or `/audio` is mounted, gives each
  session a fresh empty `HOME`, and deletes any `CLAUDE.md`, `AGENTS.md` or
  `.claude/` before and after every session, so a page cannot leave
  instructions for the next one.
- **Its token** is from `claude setup-token`: it can make model requests on
  your subscription and nothing else. A leak would cost usage, not data.
- **The voice job** runs third-party ML code over text the web shaped, so it
  has no network at all and nothing to take. The model ships in the image.
- **The publisher** never trusts what it reads. It validates every issue
  itself before using it, puts only checked values into mail headers and
  command lines, escapes everything in the HTML, and passes the Spotify CLI a
  filtered environment. Its Gmail grant can only send (`gmail.send`), and is
  sealed on disk with `BRIEF_TOKEN_KEY`, which lives in `.env`.
- **The Spotify login** is the CLI's own `token.json`, in plain text under
  `state/home/.config/`, in a 0700 directory only the publisher mounts.
- **Its Google Cloud project is its own**, not gmail-mcp's. Revoking a token
  can revoke the grant for every client in the same project
  ([forum report](https://discuss.google.dev/t/multiple-oauth-clients-in-the-same-google-cloud-project-revoking-each-others-token/293608),
  not in Google's docs), and `gmail-login` revokes on any refusal.

## Setting up

### Google Cloud

1. Create a **new** project for this stack and enable the **Gmail API**.
2. **OAuth consent screen**: **Internal** if the mailbox is on Google
   Workspace. Internal apps skip verification and their refresh tokens do not
   expire on a schedule. A personal account can only be External, and in
   Testing status Google expires refresh tokens after 7 days.
3. **Credentials -> Create OAuth client -> Desktop app.** Copy the id and
   secret into `.env`.

### On the NAS

```sh
cd /share/CE_CACHEDEV4_DATA/homelab/daily-brief
```

```sh
cp .env.example .env && chmod 600 .env
```

Fill in `.env`. `BRIEF_TOKEN_KEY` comes from:

```sh
openssl rand -base64 32
```

Then write down what you follow:

```sh
cp interests.example.md interests.md
```

```sh
./scripts/preflight.sh --fix
```

```sh
docker compose pull
```

### The Claude Code token

On any machine where Claude Code is logged in to your subscription:

```sh
claude setup-token
```

Put what it prints in `.env` as `CLAUDE_CODE_OAUTH_TOKEN`.

### Gmail

```sh
docker compose run --rm brief-publisher daily-brief gmail-login
```

It prints a Google address. Open it on the Mac and consent as
`GMAIL_ACCOUNT`. Google then sends the browser to
`http://127.0.0.1:8766/callback`, which does not load: nothing listens
there. Copy that page's full address and paste it into the terminal. It
refuses, and revokes the grant, if you consent as another account or untick
the send scope.

### Spotify

```sh
docker compose run --rm brief-publisher save-to-spotify auth login --no-browser
```

The same paste-back: open the address it prints, approve, and paste the
`http://127.0.0.1:8085/callback?...` address back. Then make the show:

```sh
docker compose run --rm brief-publisher save-to-spotify --json shows create --title "Daily brief" --summary "My morning news brief"
```

If it asks for a cover image, put a square JPEG in the state directory and
add `--image /state/cover.jpg`.
Put the show's id in `.env` as `SPOTIFY_SHOW_ID`. To list it again:

```sh
docker compose run --rm brief-publisher save-to-spotify --json shows
```

Leave `SPOTIFY_SHOW_ID` blank to send the email with no podcast.

### First run

```sh
./scripts/preflight.sh
```

```sh
docker compose up -d
```

```sh
docker compose exec brief-agent /usr/local/lib/daily-brief/write.sh
```

That writes today's issue now instead of at 05:30. The voice job and the
publisher pick it up within five minutes of each step finishing:

```sh
docker compose logs -f
```

Or from the repo root: `bin/homelab deploy daily-brief` and
`bin/homelab status daily-brief`.

## When something goes wrong

- **The logs** say what each job did, day by day:
  `docker compose logs brief-agent` (and `brief-voice`, `brief-publisher`).
- **A notice email** carries the reason, from the writer's
  `issues/published/<date>/failed.txt` or from the publisher, which checks
  each issue again itself. A voice job that gives up writes
  `audio/<date>/failed.txt`, and that day's email goes without a podcast and
  says why.
- **To redo a day**, move aside what it left: `issues/published/<date>/` to
  write it again, `audio/<date>/` to read it aloud again, and
  `state/issues/<date>.json` to email and upload it again. The last one sends
  a second email and uploads a second episode.
- **`Google no longer accepts the stored token`** in the publisher's log
  means the grant was revoked or expired: run `gmail-login` again.

## What is not tested

- **No container has been built here.** The Go tests, the voice tests and the
  writer's tests pass, and a real Kokoro render was checked against the
  publisher's timeline code, but the image builds only in CI, where it also
  runs the voice tests and reads one sentence aloud with the real model.
- **`claude -p` has not run with a real token.** The flags exist in the
  pinned 2.1.308, and `test/test-write.sh` checks they are passed, but how
  `--restricted`, `dontAsk` and the policy behave together is unverified.
  The first manual run is that test.
- **No real Gmail or Spotify call has been made.** Both are tested against
  stand-ins: a fake Google server, and a fake CLI that records what it is
  asked.
- **How long a render takes on the NAS's Celeron is unknown.** It does not
  need to be fast: the email waits up to four hours for it.
