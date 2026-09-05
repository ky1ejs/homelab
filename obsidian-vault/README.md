# obsidian-remote

An always-on Claude Code session rooted in an Obsidian vault, running on a QNAP
NAS, drivable from an iPhone — with git version history and verified off-site
backups.

This file is the **operating manual** — how to set it up and run it.

- **What the system is** — [`ARCHITECTURE.md`](ARCHITECTURE.md): services,
  volumes, data flows, and the [invariants](ARCHITECTURE.md#invariants) that
  fail silently when broken.
- **Why it is this way** — [`DECISIONS.md`](DECISIONS.md): options rejected,
  costs, and the traps found building it.

---

## What runs

| Service | Command | Lifecycle |
|---|---|---|
| `vault-sync` | `ob sync --continuous` + hourly snapshot backstop | always on |
| `vault-claude` | `claude remote-control` inside tmux, rooted in the vault | always on, restarted often |
| `vault-research` | `claude remote-control` inside tmux, rooted in `/scratch` | always on, restarted often |
| `vault-mcp -stdio` | each agent's tools — `move_file`, and `fetch_attachment` for research — spawned by Claude Code from the project's `.mcp.json` | per session, not a container |
| `vault-research-sweep` | supercronic → `scratch-sweep.sh` on `SCRATCH_SWEEP_SCHEDULE` | always on |
| `vault-cron` | supercronic → `backup.sh` on `BACKUP_SCHEDULE` (hourly) | always on |
| `backup` | bundle → verify → encrypt → replace `vault-latest` | manual profile, ad-hoc runs |

No ports are published. Remote Control dials out and your phone connects through
Anthropic's bridge, so there is nothing to configure on the UniFi gateway.

### Two agents, and which one to open

You pair the phone to both and pick by what you are doing.

| | Use it for | It can | It cannot |
|---|---|---|---|
| `vault-claude` | notes, triage, filing, anything touching the vault | read and write the whole vault | reach the network at all |
| `vault-research` | looking things up, collecting sources, saving images | search and fetch the web, download files | see your vault |

Research output lands in `/scratch`, which `vault-claude` reads (read-only) and
files into the vault — notes with `Read` and `Write`, images and PDFs with
`import_attachment`, which is the only way one crosses. The split is what makes web access safe to have: the
session that can send things out has nothing of yours in it. Scratch folders
are deleted after `SCRATCH_RETENTION_DAYS` (7 by default), so finish a topic or
expect to redo it. See
[`ARCHITECTURE.md`](ARCHITECTURE.md#three-surfaces-three-different-mitigations).

```sh
homelab attach            # vault-claude, the default
homelab attach research   # vault-research
```

---

## Status

**Running.** Stood up and verified end to end on 2026-08-08.

Sync pulls the vault, the phone drives a Remote Control session rooted in it,
the tool policy enforces, the snapshot hooks bracket agent runs, and a note the
agent wrote on the NAS appeared in Obsidian on the phone. **Every previously
untested assumption held and no script needed changing** — `ob`'s CLI shape,
credential persistence, `remote-control` as a long-lived container process, and
the hooks' `jq` stdin parse.

Stack, snapshots, and hourly encrypted bundles reaching Google Drive via Hybrid
Backup Sync.

**The restore test passed, from the Drive copy rather than the NAS**, so the
whole chain is proven end to end: 1,233 notes, 80 attachments and the full audit
trail decrypted and cloned on the Mac.

### Outstanding

| Item | Notes |
|---|---|
| **Monitoring** | HBS's "job fails" notification covers the Drive leg. Nothing covers `vault-sync` dying or `vault-cron` stalling. An agent that cannot start is no longer *silent* — it says why in its own logs, the dashboard shows uptime rather than container age, and `preflight.sh` checks both logins ([DECISIONS.md](DECISIONS.md#saying-why-the-agent-stopped)) — but nothing pushes any of it to you |
| **Vault conventions** | `AGENTS.md` documents four of eleven top-level folders and no tag conventions. The `agent-*` stamp is the one frontmatter convention anything enforces, and it describes *who wrote a note*, not how notes should be written — most output quality still lives here, not in the infrastructure |
| **Agent write scope** | Undecided. `AGENTS.md` states the conservative half in prose; only the `AGENTS.md` write-deny is actually enforced in `settings.json` |
| **Skills in Obsidian** | Deferred design — see [`DECISIONS.md`](DECISIONS.md#deferred-skills-authored-in-obsidian) |
| **`age` key on paper** | It is in 1Password and off the Mac's disk. A paper copy is still owed; 1Password is otherwise a single point of failure for every bundle |

Three things a live run surfaced that the design did not predict:

- **`claude setup-token` cannot establish Remote Control sessions.** It was
  offered here as an equal alternative to the interactive login and would have
  silently cost the architecture.
- **`ob` reports the container hostname to Obsidian Sync as the device name**,
  and Docker defaults it to the container ID, so every recreate registered a new
  phantom device. Hostnames are now pinned in compose.
- **Remote Control pairing survives a container recreate and a NAS reboot.** The
  design assumed re-pairing after every deploy; it isn't needed. That removes a
  manual step and weakens the case for keeping deployment manual.

---

## Setup

### 1. Prerequisites

**An active Obsidian Sync subscription. This is a hard blocker, not a
preference.** `ob sync-setup` links an *empty local directory* to a remote vault
that already exists on Obsidian's servers, and the first `ob sync` pulls the
contents down. That is the **only** mechanism putting the vault on the NAS — no
subscription means no vault here at all, and the whole stack stalls rather than
just `vault-sync`.

**The vault does not need to be copied to the NAS.** You create an empty
directory; sync fills it.

**Use a real QNAP account's uid, and make the image match it.** Resolved for
this NAS on 2026-08-08: `kylejs` is **1002:100**, and the image is built with
those as `APP_UID`/`APP_GID`. If you are standing this up somewhere else:

```sh
id <your-qnap-username>       # -> uid=… gid=…
getent passwd 1000 1001 1002  # -> who already holds the low uids
```

**Do not reach for 1000 because it looks standard.** QNAP allocates uids
sequentially from 1000, so an unallocated 1000 is simply the *next* account
someone creates — which would then own the two 0700 credential directories, and
0700 protects against everyone except the owner. It would work perfectly until
the day it didn't.

`gid=100` is `everyone`, which every QNAP account belongs to, so the group bits
grant nothing here. The owner uid is doing all of the access control.

These are **build args**, so changing them means a rebuild — not just an `.env`
edit. Docker resolves `$HOME` from `/etc/passwd`, so overriding `user:` in
compose to a uid the image does not know makes `$HOME` resolve to `/`, and both
logins in step 5 appear to succeed and then fail to persist. The other mismatch
symptom is subtler: sync works, the agent appears to write, and your Mac sees
unreadable files.

### 2. Host directories

`vault/` is created **empty** — `ob sync` populates it in step 5.

QNAP volume names are not predictable. Find yours first — `CACHEDEV1_DATA` is
the common default and was *not* correct on this NAS:

```sh
ls -d /share/*/
```

```sh
mkdir -p /share/CE_CACHEDEV4_DATA/obsidian/{vault,snapshots,backups,scratch,home-sync,home-agent,home-research}
chown -R 1002:100 /share/CE_CACHEDEV4_DATA/obsidian
chmod 700 /share/CE_CACHEDEV4_DATA/obsidian/home-sync
chmod 700 /share/CE_CACHEDEV4_DATA/obsidian/home-agent
chmod 700 /share/CE_CACHEDEV4_DATA/obsidian/home-research
ls -n /share/CE_CACHEDEV4_DATA/obsidian    # every entry must read 1002 100
```

The three `home-*` directories are **split on purpose**: `home-sync` holds your
Obsidian credentials, `home-agent` and `home-research` each hold a live
Anthropic OAuth token, and no service can read another's. A prompt-injected
agent reading its own filesystem then yields one credential rather than all
three — see [`ARCHITECTURE.md`](ARCHITECTURE.md#volumes).

`home-research` is also **required for correctness**, not just isolation: two
Claude Code instances sharing one home directory corrupt `~/.claude.json`, so
pointing `vault-research` at `home-agent` would break both agents.

Do not export any of them over SMB/AFP, and never back one up to Drive.

**`scratch/` must not be inside `vault/`.** It is the research agent's working
directory, and keeping the vault out of that container is what makes its web
access safe. It is also the only directory anything here deletes from, on a
schedule. `preflight.sh` fails if the two are ever nested.

They live on an **encrypted volume** (`CE_` prefix), auto-unlock enabled. All
seven paths must stay on it — one on a plain volume drops out of the encrypted
set and nothing about the running system would look any different. See
[`ARCHITECTURE.md`](ARCHITECTURE.md#trust-boundary) for what that does and does not buy.

**Don't do any of the above by hand — use `preflight.sh`:**

```sh
./scripts/preflight.sh --fix    # create, chown, chmod, then verify
```

It reads `.env` for the paths and ids, so it cannot disagree with the compose
file. See [Preflight](#preflight) below.

### 3. Configure

**The NAS does not need this repo.** The compose file has no `build:` directive
and no relative paths — it pulls the image from GHCR and runs scripts baked into
it. Two files in one directory are sufficient:

```sh
mkdir -p /share/CE_CACHEDEV4_DATA/homelab/obsidian-vault
cd /share/CE_CACHEDEV4_DATA/homelab/obsidian-vault
curl -fsSLO https://raw.githubusercontent.com/ky1ejs/homelab/main/obsidian-vault/docker-compose.yml
```

Then write `.env` beside it — copy [`.env.example`](.env.example) and fill in
the paths, or paste directly:

```sh
chmod 600 .env
```

**Or clone the repo, if you would rather `git pull` compose changes than re-curl
them.** QNAP ships no `git`, but you already have Docker, so run it in a
container rather than installing anything into QTS:

```sh
docker run --rm -v /share/CE_CACHEDEV4_DATA:/w -w /w alpine/git \
  clone https://github.com/ky1ejs/homelab.git

# updates thereafter
docker run --rm -v /share/CE_CACHEDEV4_DATA/homelab:/w -w /w alpine/git pull
```

The vault image already contains `git`, so `--entrypoint git
ghcr.io/ky1ejs/homelab/obsidian-vault:latest` works too with no extra pull.

**`.env` is gitignored, so `git pull` never clobbers your config.** Tracked
files are the compose definitions; local state stays yours. With the monorepo,
one pull updates every stack's compose file at once.

**Cloning is now the better option**, because `scripts/preflight.sh` and
`vault-claude-settings.json` are both things you want on the NAS and neither
arrives with the compose file. `preflight.sh` is baked into the image, so you
*can* run it without a clone — but it can only compare the installed
`settings.json` against a checkout it can see, and that comparison is the point:
the agent installs its own copy from the image, so what preflight adds is
whether the file on the host, the one that gets committed and restored, matches
the source you are reading.

Cloning also brings `scripts/deploy.sh`, whose sole advantage over `docker
compose pull && docker compose up -d` is provenance verification — and that
needs the `gh` CLI, which QNAP does not ship, so it degrades to a warning.

**Alternative:** Container Station's Application UI accepts pasted compose YAML
and manages the stack from the GUI, with no files on disk. Functionally
identical.

### 4. The image

Already built and published by CI to `ghcr.io/ky1ejs/homelab/obsidian-vault`.
The package is **public**, so the NAS needs no registry credential — `docker
compose pull` just works.

Only build locally if you are changing the image:

```sh
docker compose build     # requires the repo
```

### 5. One-time interactive logins

These establish credentials in the `/home/app` volume. They are **login state,
not deploy-time configuration** — which is why neither ever enters GitHub
Actions.

They now happen in **two separate containers**, because the credential volumes
are split.

> ⚠️ **Never run a second Claude Code instance against `home-agent` while
> `vault-claude` is up.** Claude Code has multiple open issues about
> `~/.claude.json` being corrupted by concurrent instances — it has no file
> locking and its writes are not atomic
> ([#28847](https://github.com/anthropics/claude-code/issues/28847),
> [#29217](https://github.com/anthropics/claude-code/issues/29217)). One reported
> case cascaded into 165 corrupted backup files in 40 minutes and a login loop.
>
> Do these logins **before** `docker compose up -d`. If you need to re-auth
> later, `docker compose stop vault-claude` first.
>
> `vault-research` has its own `home-research` volume, so the two agents may run
> at the same time. The rule is one instance per credential volume, not one
> instance overall.

```sh
# Obsidian Sync credentials -> home-sync, and populate the empty vault
docker compose run --rm vault-sync bash
  ob login
  ob sync-list-remote                    # find the exact remote vault name
  ob sync-setup --vault "<Vault Name>"   # links /vault to that remote vault
  ob sync                                # first pull — this fills /vault
  ls /vault                              # confirm your notes are actually here
  exit

# Claude OAuth token -> home-agent
docker compose run --rm vault-claude bash
  claude          # then /login. NOT `claude setup-token` — see below
  exit

# A SECOND Claude OAuth token -> home-research, for the research agent.
# Separate volume, separate login. Sharing one breaks both agents.
docker compose run --rm vault-research bash
  claude          # then /login, same as above
  exit
```

> ⚠️ **Do not use `claude setup-token` here.** Per the
> [auth docs](https://code.claude.com/docs/en/iam#generate-a-long-lived-token),
> a `setup-token` credential *"can only make model requests, so it can't
> establish Remote Control sessions"* — which is the entire purpose of
> `vault-claude`. Use the interactive login.

The browser flow works fine in a container: Claude Code cannot reach its local
callback server, so it falls back to showing a login code in the browser which
you paste back at the `Paste code here if prompted` prompt. This is a documented,
anticipated path, not a workaround.

Credentials land in `~/.claude/.credentials.json` at mode `0600` — inside the
`home-agent` volume, which is exactly why that volume exists.

### 6. Install the hooks, tool policy and move tool

**The stack does this for you now — this step is optional.** `vault-claude`
runs `install-settings.sh` before starting the agent, which materialises two
files from copies baked into the image beside the hook scripts they point at:

| Installed | From | Carries |
|---|---|---|
| `<vault>/.claude/settings.json` | `vault-claude-settings.json` | the tool policy and the hooks |
| `<vault>/.mcp.json` | `vault-claude-mcp.json` | the agent's move tool |
| `<scratch>/.claude/settings.json` | `vault-research-settings.json` | the research agent's policy |
| `<scratch>/.mcp.json` | `vault-research-mcp.json` | its move and fetch tools |
| `<scratch>/CLAUDE.md` | `research-CLAUDE.md` | its standing instructions |

The last three are installed by `research.sh` in the research container, using
the same script pointed at different sources. Both agents' policies are security
files and neither is edited in place — see the warning at the end of this
step.

Run the commands below only if you want them in place before the first
`docker compose up -d`.

The file matters more than "hooks": it is where `Bash`, `WebFetch` and
`WebSearch` are denied, the only real mitigation for the prompt-injection risk
in [`ARCHITECTURE.md`](ARCHITECTURE.md#trust-boundary). It used to be hand-copied
here, and `agent.sh` would *warn* and start anyway — so a session that missed
the copy got no commit bracketing **and** an agent holding exactly the tools
that turn an injected note into an exfiltration. Installing it from the image
closed that gap, and `agent.sh` now **refuses to start** if no policy ends up in
the vault: with installation automatic, a missing file means the install failed,
and `restart: unless-stopped` makes that a visibly crash-looping container
rather than a quietly over-privileged agent.

If you do run it, it has to come after step 5: `ob sync` creates the vault
directory's contents, and the settings file needs to survive alongside them.

```sh
mkdir -p /share/CE_CACHEDEV4_DATA/obsidian/vault/.claude
cp vault-claude-settings.json \
   /share/CE_CACHEDEV4_DATA/obsidian/vault/.claude/settings.json
cp vault-claude-mcp.json \
   /share/CE_CACHEDEV4_DATA/obsidian/vault/.mcp.json
chown -R 1002:100 /share/CE_CACHEDEV4_DATA/obsidian/vault/.claude
chown 1002:100 /share/CE_CACHEDEV4_DATA/obsidian/vault/.mcp.json
```

**The second file is what lets the agent file things.** Claude Code has no move
tool and `Bash` is denied, so without it the agent can read, write and edit
notes but cannot move or rename one — and the failure is quiet: it writes a copy
at the new path and cannot delete the original. For an **attachment** it cannot
even do that; `Write` emits text, so it cannot author the bytes. It registers
`vault-mcp`'s own binary (built into this image) as a local MCP server serving
one tool, `move_file`, which moves notes and attachments alike and enforces the
same deny list as the policy above. A missing `.mcp.json` is a **warning** at
start, not a refusal: that agent is less capable, not unsafe. See
[`DECISIONS.md`](DECISIONS.md#giving-the-agent-a-move).

**To change a hook, a deny rule or the move tool: edit the file in this repo,
push, deploy.** CI rebuilds the image on any change under `obsidian-vault/` that
is not markdown — and on any change to `vault-mcp/*.go`, `go.mod` or `go.sum`,
because this image builds that Go source into the move tool. The next start
reinstalls both files. There is no `cp` to remember and no way for the policy the
agent obeys to lag the one in the repo.

`preflight.sh` still compares the host's copy of the tool policy against this
checkout, because that copy is the one that gets committed, bundled and restored.
A difference means either an image older than your checkout, or a file you pinned
deliberately with `VAULT_SETTINGS_MANAGED=0` — which pins both files, not just
the policy.

A running agent is not reconfigured by the reinstall — the file is read when a
session starts, so recreate `vault-claude` to pick up a change.

### 7. Start

Last gate — this is the run that has the image pulled, so it is the one that can
finally check the baked uid against `.env`:

```sh
./scripts/preflight.sh          # must exit 0
docker compose up -d
docker compose ps
docker compose logs vault-claude | grep -i warning   # should find nothing
```

### 8. Pair the phone

```sh
docker exec -it vault-claude tmux attach -t vault
```

Read the QR or URL, pair from the Claude app's Code tab. **Detach with
`Ctrl-b d`** — `Ctrl-c` kills the agent.

Then run a session and confirm the commit pair and attribution:

```sh
docker compose exec vault-sync \
  git --git-dir=/snapshots/vault.git log --oneline -5
```

You should see `pre-agent: <id>` and `agent: <id>` authored by `Claude Code`.

Also confirm nothing git-shaped leaked into the vault:

```sh
ls -a /share/CE_CACHEDEV4_DATA/obsidian/vault | grep -E '^\.git' || echo "clean"
```

Then, in **each** agent session, ask it two things.

**`/mcp`.** `vault-tools` should be the only server listed. If Gmail, Google
Drive, Notion or `kylejs Obsidian Vault` appears, the claude.ai connectors on
your account have been loaded into the container. That is the whole security
model gone: on `vault-claude` a connector like Gmail is the way out the deny
list exists to remove, and on `vault-research` the vault connector is the notes
themselves, arriving over HTTPS with no mount involved. Both policy files set
`disableClaudeAiConnectors` and compose sets `ENABLE_CLAUDEAI_MCP_SERVERS=false`,
so seeing one means a stale image or a policy that did not install. Stop the
container before doing anything else.
[DECISIONS.md](DECISIONS.md#the-connectors-nobody-configured).

**`/permissions`.** Every rule in the policy file should be listed. A rule that
is absent here was rejected or mis-spelled, and the file gives no other sign.

Neither check is something `preflight.sh` can do for you: it reads the
configuration on the host, and this reads what the running session actually got.

### 9. Backups

Run one by hand to check it:

```sh
docker compose --profile manual run --rm backup
```

A run whose `HEAD` has not moved since the last bundle exits immediately without
writing anything. To bundle anyway, set the environment variable — **not a
`--force` argument**, because `docker compose run <service> <args>` *replaces*
the service's command rather than appending to it, so `run --rm backup --force`
tries to execute `--force` and dies in tini:

```sh
docker compose --profile manual run --rm -e BACKUP_FORCE=1 backup
```

The `--force` flag works only when the script is invoked directly:

```sh
docker compose exec vault-cron /usr/local/lib/vault/backup.sh --force
```

**Scheduling lives in the stack, not on the NAS.** `vault-cron` runs supercronic
against `BACKUP_SCHEDULE` from `.env` (default `0 3 * * *`, UTC) and invokes
`backup.sh` directly.

QNAP's crontab was the obvious alternative and is worse in three specific ways:
it is untracked, so the schedule is not reviewable or restorable from git;
`crontab -e` alone does not survive a reboot, because QTS regenerates from
`/etc/config/crontab`; and firmware updates have been known to rewrite it. A
backup schedule that can silently vanish is the §11.6 failure mode aimed
squarely at the thing meant to protect you from it.

`vault-cron` runs `backup.sh` over the same volumes rather than `docker run`-ing
a sibling container. That avoids mounting `/var/run/docker.sock`, which would
hand this container effective root on the NAS to no benefit.

That reasoning is unchanged, but it is no longer true of the repo as a whole:
the [`dashboard`](../dashboard/) stack does hold the socket, because a deploy
button cannot exist without it. A cron job still gains nothing from it, so this
container still does not get it. The assessment is in
[`DECISIONS.md`](DECISIONS.md#the-dashboard-and-the-docker-socket).

```sh
docker compose logs vault-cron        # schedule on startup, then each run
```

Then point Hybrid Backup Sync at `BACKUP_HOST_PATH` → Google Drive, scheduled
**at least an hour after** `BACKUP_SCHEDULE` so it never uploads a bundle
mid-write.

**Set the sync job's Action to `Mirror`, not `Copy`.** This is not cosmetic.
`Copy` never deletes at the destination, so `backup.sh`'s pruning stops at the
NAS and Drive accumulates every bundle ever written — unbounded, and invisible
because the NAS side looks correct. Found 2026-08-12: a `weekly/` folder deleted
from the NAS four days earlier was still on Drive, along with a stray
`.vault-….bundle.tmp.lock` from a bug fixed in `b6463e1`.

Know what `Mirror` costs, though: it propagates *destruction*, not just deletion.
If `BACKUP_DIR` is ever emptied while the NAS keeps running — a volume that fails
to remount, a broken share path, a mistaken `rm` — the next run empties Drive
too, stamped copies and all. That is the failure the retention tiers exist to
survive, and this is the one configuration that defeats them. Drive's 30-day
trash is the last line; if HBS offers destination-side version retention, prefer
that over relying on it.

Note HBS can only select **shared folders**, not arbitrary directories. If the
picker shows nothing, create a shared folder in Control Panel and point its path
at the existing backups directory — QNAP allows an existing path, so nothing
needs moving. Create one for `backups` only; the bundles are age-encrypted, but
`snapshots`, `home-sync` and `home-agent` must never become shares.

**Never at `SNAPSHOT_HOST_PATH`** — a live repo is files under mutation and will
sync torn. **Never at `HOME_HOST_PATH`** — that is a live Anthropic token.

### 10. Restore-test once. Do not skip this.

Do it from the **Google Drive copy**, not the NAS — that exercises the whole
chain (bundle → verify → encrypt → HBS → Drive → download → decrypt → clone)
rather than just the last step.

```sh
age -d -i ~/vault-backup-key.txt vault-latest.bundle.age > /tmp/v.bundle
git bundle verify /tmp/v.bundle
git clone /tmp/v.bundle /tmp/restore-test
du -sh /tmp/restore-test
git -C /tmp/restore-test log --oneline | head
```

**Any point in time comes out of that one file** — the bundle carries the full
history, which is why `vault-latest` is replaced in place rather than kept as a
pile of stamped copies:

```sh
git -C /tmp/restore-test checkout 'HEAD@{2026-08-01}'
```

The stamped copies under `hourly/`, `daily/` and `monthly/` are for a different
job: reaching *past* a corrupted or rewritten repo that has already been bundled
over the top of `vault-latest`. Restore from one exactly as above — every bundle
is independently clone-able — picking the newest that predates the damage.

Delete the decrypted bundle and the clone afterwards; both are complete
plaintext copies of your vault.

An untested backup is a rumour.

---

## Operating

### Preflight

```sh
./scripts/preflight.sh          # check only, changes nothing
./scripts/preflight.sh --fix    # repair what it can, then re-check
```

Run as root on the NAS, from the directory holding `docker-compose.yml` and
`.env`. Idempotent by design — **re-run it after any QNAP change**: a new user
account, a new share, a volume migration, a restore. Most of what it catches is
drift you cannot see by looking at the directories:

| Check | The failure it prevents |
|---|---|
| `APP_UID` belongs to a real account | QNAP allocates uids sequentially from 1000, so an *unallocated* uid is the next account someone creates — which then owns the 0700 credential directories |
| image's baked uid == `.env` `APP_UID` | Docker resolves `$HOME` from `/etc/passwd`; an unknown uid gets `$HOME=/`, and **both interactive logins appear to succeed and then do not persist** |
| all five paths on the same `CE_` volume | one path on a plain volume silently leaves the encrypted set |
| no SMB share points at `home-sync` / `home-agent` / `snapshots` | `0700` is irrelevant if the directory is also exported over the network |
| `.claude/settings.json` denies `Bash`, and matches this checkout | A session without a policy has no commit bracketing **and** the tools that turn an injected note into an exfiltration. `vault-claude` installs the file and refuses to start without one, so absence is only a warning *here* — what this check adds is a *stale* policy: pinned with `VAULT_SETTINGS_MANAGED=0`, or an image older than your checkout |
| a Claude login exists in each agent's own `/home/app` volume | without one, `claude remote-control` exits at startup and the container crash-loops with the phone unable to connect and the dashboard showing it as running. An access token whose expiry has passed is only a *warning*: Claude Code renews it from the refresh token, so that is normal after a container has been stopped overnight, and only matters alongside a crash loop |
| the two agents hold **different** logins | compared byte for byte and by `device:inode`, not by path. Copying `.credentials.json` between the volumes to skip the second login leaves two containers refreshing one token with no lock between them, which is how a login gets *revoked* — and a path comparison passes it silently |
| ownership, modes, `.env` is `0600` | the ordinary drift |

`--fix` repairs directories, ownership, modes, `.env` permissions, and can install
`settings.json` if it can see a copy. It deliberately does **not** touch
anything requiring judgement — a wrong uid, an SMB export, a path on the wrong
volume — those it reports and leaves to you.

`--auth` additionally asks Claude Code itself whether each login still works
(`claude auth status`), which is the state the agent acts on rather than
something inferred from a file. It is opt-in and **refuses while that agent's
container is running**: the probe is a second Claude Code process against
credentials the agent is using, and two processes refreshing one token is the
documented way to revoke a login. Stop the agent first:

```sh
docker compose stop vault-claude
./scripts/preflight.sh --auth
docker compose start vault-claude
```

This is also the closest thing the stack currently has to the monitoring that
[`DECISIONS.md`](DECISIONS.md#open-questions) says is missing. It is a preflight,
not a monitor: it tells you nothing about whether sync is *running*.

### When an agent will not stay up

The symptom is the phone failing to connect while the dashboard shows the
container as running, and `homelab attach claude` finding no session. In the
logs it looks like this, repeating every few seconds:

```
[agent] INFO  starting claude remote-control in tmux session vault
[agent] INFO  attach with: docker exec -it $(hostname) tmux attach -t vault
[agent] WARN  tmux session ended within 0s of starting
```

That is a crash loop, not a session waiting to be paired: `restart:
unless-stopped` is restarting a container whose agent exits during startup. The
lines after it are the agent's own output, captured from the pane before tmux
took it away, and they say which of these it is:

```sh
homelab logs obsidian-vault vault-claude              # or vault-research
homelab logs obsidian-vault vault-claude | grep -E 'WARN|FATAL'
```

Every line is `[prefix] LEVEL message`, and `homelab logs` passes
`--timestamps`, so the two questions worth asking of a container log — *when*
and *did anything go wrong* — are both answerable without knowing which script
wrote which line. See [`scripts/log.sh`](scripts/log.sh).

The three usual causes, in the order they happen:

1. **The login expired or was lost.** Re-run the interactive login *in that
   agent's own container* — [setup step 5](#5-one-time-interactive-logins), or
   `claude auth login` as a one-liner. Stop the container first: a second Claude
   Code against a live credential volume corrupts `~/.claude.json`.
2. **Both agents on one login.** Either one home volume for both, or the same
   `.credentials.json` copied into each. `preflight.sh` fails on both by name.
3. **`mem_limit`.** An OOM-killed agent prints nothing at all before it goes,
   which the log says in those words.

`./scripts/preflight.sh` answers 1 and 2 without waiting for the loop, and
`--auth` settles 1 outright.

**If it recurs, check the clock.** Token validation depends on correct
timestamps, and the Claude Code troubleshooting docs name an inaccurate system
clock as the cause when re-authentication is needed *repeatedly*. `date -u` on
the NAS against real UTC costs nothing to rule out.

**There is no long-lived credential to switch to**, and this is worth knowing
before you go looking. `claude setup-token` and `CLAUDE_CODE_OAUTH_TOKEN` are
refused with *"Remote Control requires a full-scope login token"* — they can
only make model requests. `ANTHROPIC_API_KEY` is refused too: *"API key
authentication is not supported for Remote Control."* So do routing a session
to Bedrock, Vertex or a custom `ANTHROPIC_BASE_URL`. An interactive claude.ai
login is the only credential that establishes a Remote Control session, which
is why every recovery path here ends in one.

### Deploy a new version

```sh
./scripts/deploy.sh              # everything
./scripts/deploy.sh --sync-only  # leave the agent session alone
```

Deployment is manual on purpose — though the reason is narrower than it was.

**Pairing survives a container recreate** (verified 2026-08-08): the OAuth token
is in the `home-agent` volume, the new container starts already authenticated,
and the phone reconnects to the fresh `remote-control` session on its own. No
re-pairing, no `docker exec`, nothing interactive. Same after a NAS reboot.

What remains is narrower: an unattended `pull && up -d` can still interrupt a
session that is *mid-run*. That is an annoyance rather than the silent loss of
access it was assumed to be, so treat manual deployment as a preference now, not
a constraint.

### Audit what the agent did

```sh
docker compose exec vault-sync \
  git --git-dir=/snapshots/vault.git log --author="Claude Code" --since=1.week --stat
```

Given the agent writes unattended, this is arguably worth more than the undo.

### The agent stamp

The repo above is the better audit log, and it lives outside the vault — which
means it is invisible from Obsidian on the phone, where these notes are actually
read, and a note that leaves the vault through Sync carries none of it. So every
note the agent writes also carries the attribution in its own frontmatter:

```yaml
---
agent-created: 2026-08-20T09:11:03Z    # only when this session made the note
agent-modified: 2026-08-29T14:02:11Z   # last agent write of any kind
agent: claude-agent                     # who made that write
---
```

`scripts/hook-stamp.sh` writes it from a `PostToolUse` hook, on each `Write` or
`Edit`. Per-write rather than batched at `Stop`, because at `Stop` an agent edit
and a Mac edit that Sync landed mid-session are indistinguishable, and a wrong
attribution is worse than a missing one.

**`move_file` stamps notes itself**, because a `PostToolUse` matcher on
`Write|Edit` does not see MCP tool calls. It writes `agent-modified` and `agent`
into the note's own bytes before the rename, and never `agent-created` — filing a
note is not authoring it. This is the reason that surface serves exactly one
tool: a second write tool added there would fire no hook either, and would land
unstamped unless someone remembered this.

**An attachment moved by the agent carries no stamp at all**, because a PNG has
nowhere to put YAML. That is a real hole in the paragraph above and worth
knowing before you trust a dataview query over `agent-modified` to be complete:
it will silently miss every image the agent ever filed. What records those moves
instead is the snapshot commit and `vault-mcp`'s audit log — and if this vault
ever switches to `EXCLUDE_ATTACHMENTS=1`, attachments are outside git and the
audit log is the only record there is.

The names are not Claude-specific and the identity is the value: `vault-mcp`
writes the same three properties as `claude-voice` when it serves voice, and as
`VAULT_AGENT_NAME` — the variable this hook reads — when the same binary serves
the agent's move tool. That is a **shared
contract**, not a per-stack choice — see the root
[`README.md`](../README.md#shared-contract), and keep the two in step. From
Obsidian:

````
```dataview
TABLE agent, agent-modified
WHERE agent-modified
SORT agent-modified DESC
```
````

Two things it does not do. It never rewrites `agent-created`, so that property
means "an agent made this note", not "an agent touched it recently". And nothing
clears any of them when you edit the note by hand afterwards, so `agent-modified`
means *an agent last wrote this note* — never *this content is the agent's*.

The hook mirrors the deny list in `settings.json` itself rather than relying on
it: hooks run outside the permission system, so a hook that stamped `CLAUDE.md`
would be writing to a file the agent is denied. It stamps markdown only, nothing
under a dotted directory, and never `AGENTS.md` or `CLAUDE.md` at any depth.

It also checks its own work before the rename: a stamped note must be the note
the agent wrote plus the stamp lines, with `agent` and `agent-modified`
rewritten in place and every other line intact, byte for byte and in order.
Anything else and it writes nothing, leaving the note unstamped until the next
agent write. `vault-mcp` runs the same check — see
[`DECISIONS.md`](DECISIONS.md#agent-stamps-in-frontmatter).

**One trap.** Stamping changes the file after the agent wrote it, so the agent's
next `Edit` of that note can be refused as modified-since-read. The hook returns
`additionalContext` telling it to re-read, which is why that message exists; if
it turns out to be noisy in practice, the escalation is in
[`DECISIONS.md`](DECISIONS.md#agent-stamps-in-frontmatter).

### Roll back a bad agent run

```sh
docker compose exec vault-sync git --git-dir=/snapshots/vault.git \
  --work-tree=/vault checkout <pre-agent-sha> -- .
```

Sync propagates the restored state to your devices.

---

## Sharing the vault with other stacks

If another container ever writes into this vault, keep it a **separate repo,
image and compose stack** — deploying it must never restart `vault-claude`,
because that drops the phone's paired session.

Three rules protect *this* stack's integrity and are non-negotiable regardless
of what the other side does:

| Item | Rule |
|---|---|
| **Sync** | `vault-sync` owns it. **No other container runs `ob sync`.** Two headless clients on one vault means two sync engines fighting over the same local state, plus a second device against your Sync subscription. |
| **UID/GID** | Identical `APP_UID:APP_GID` everywhere. A mismatch gives the confusing symptom: writes appear to work, the Mac sees unreadable files. |
| **Writes** | **Atomic temp-then-rename, always** — write to a temp file on the same filesystem, then `rename()`. See [`ARCHITECTURE.md`](ARCHITECTURE.md#known-unresolved-risk) for why this matters more here than usual. |

Commits need no coordination: the hourly backstop picks up any subtree, and
`git log -- <subtree>/` gives attribution for free.

---

## Known risk

`ob sync --continuous` and Claude Code's file tools write to the same directory
concurrently, and Claude's writes are not guaranteed atomic. Partial writes can
propagate to other devices via Sync.

The snapshots make this **recoverable, not prevented**. If it bites, escalate in
the order given in [`ARCHITECTURE.md`](ARCHITECTURE.md#known-unresolved-risk) — temp-then-rename
wrapper, then serialising sync against agent sessions with a lock file, then
switching to an MCP server with atomic writes.

The agent's `move_file` already takes that third route: it is a `rename(2)` with
the concurrent-writer re-check, so moving a file is not exposed to this risk the
way `Write` and `Edit` still are. That is a property of one tool, not a fix — the
file tools are still the common path and still unguarded.

---

## Conventions

- All scripts are `set -euo pipefail` and shellcheck-clean in CI.
- Two bug classes are called out inline where they were originally hit:
  `[ cond ] && cmd` under `set -e`, and `pipefail` with a glob over an empty
  directory. Both fail silently. Do not reintroduce them.
- The git repo lives outside the vault. Exclusions belong in
  `$GIT_DIR/info/exclude`, written by `snapshot.sh` — never a `.gitignore` in
  the vault.
