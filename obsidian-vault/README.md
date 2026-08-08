# obsidian-remote

An always-on Claude Code session rooted in an Obsidian vault, running on a QNAP
NAS, drivable from an iPhone — with git version history and verified off-site
backups.

**Design, rationale and costs live in [`INITIAL_PLAN.md`](INITIAL_PLAN.md).**
This file is the operating manual.

---

## What runs

| Service | Command | Lifecycle |
|---|---|---|
| `vault-sync` | `ob sync --continuous` + hourly snapshot backstop | always on |
| `vault-claude` | `claude remote-control` inside tmux | always on, restarted often |
| `backup` | bundle → verify → GFS rotate → prune | manual profile, host-scheduled |

No ports are published. Remote Control dials out and your phone connects through
Anthropic's bridge, so there is nothing to configure on the UniFi gateway.

---

## Status

Written, **not yet built or run**. Expect iteration on the first `docker compose
build`. Specifically unverified:

- the image build itself
- `DISABLE_AUTOUPDATER` — the env var name has not been checked against
  `claude config`
- where exactly `ob` persists credentials in `$HOME` (the whole `/home/app` is
  volumed as a hedge)
- whether `claude remote-control` is well-behaved as a long-lived container
  process, and whether the pairing QR renders usably over `docker exec`
- the hooks firing in a real session

Three things are **pinned with placeholders** and should be fixed before this is
trusted with a real vault:

```sh
docker buildx imagetools inspect node:22-bookworm-slim   # -> NODE_IMAGE digest
npm view @anthropic-ai/claude-code version               # -> CLAUDE_CODE_VERSION
npm view obsidian-headless version                       # -> OBSIDIAN_HEADLESS_VERSION
```

`CLAUDE_CODE_VERSION` currently defaults to `latest`, which contradicts the
pinning rule in §4 of the plan. Pin it once a build succeeds.

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

**You choose the UID/GID.** Since these directories are created fresh, nothing
needs discovering — but the value must match the image's `APP_UID`/`APP_GID`
build args (default `1000:100`), because those are baked in at build time and
changing them means a rebuild. If you want to browse the vault over SMB as your
personal QNAP account, use that account's ids instead and rebuild with them:

```sh
id <your-qnap-username>      # -> uid=… gid=…
```

A mismatch produces a confusing symptom: sync works, the agent appears to write,
and your Mac sees unreadable files.

### 2. Host directories

`vault/` is created **empty** — `ob sync` populates it in step 5.

```sh
mkdir -p /share/CACHEDEV1_DATA/obsidian/{vault,snapshots,backups,home-sync,home-agent}
chown -R 1000:100 /share/CACHEDEV1_DATA/obsidian
chmod 700 /share/CACHEDEV1_DATA/obsidian/home-sync
chmod 700 /share/CACHEDEV1_DATA/obsidian/home-agent
```

The two `home-*` directories are **split on purpose**: `home-sync` holds your
Obsidian credentials, `home-agent` holds a live Anthropic OAuth token, and
neither service can read the other's. A prompt-injected agent reading its own
filesystem then yields one credential rather than both — see
[`INITIAL_PLAN.md`](INITIAL_PLAN.md) §10.

Do not export either over SMB/AFP, and never back either up to Drive.

### 3. Configure

```sh
cp .env.example .env
chmod 600 .env
$EDITOR .env
```

### 4. Build

Either let CI build and push to GHCR, or locally:

```sh
docker compose build
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
  claude          # or: claude setup-token
  exit
```

### 6. Start

```sh
docker compose up -d
docker compose ps
```

### 7. Pair the phone

```sh
docker exec -it vault-claude tmux attach -t vault
```

Read the QR or URL, pair from the Claude app's Code tab. **Detach with
`Ctrl-b d`** — `Ctrl-c` kills the agent.

### 8. Install the hooks

```sh
mkdir -p /share/CACHEDEV1_DATA/obsidian/vault/.claude
cp vault-claude-settings.json \
   /share/CACHEDEV1_DATA/obsidian/vault/.claude/settings.json
```

Run a session, then confirm the commit pair and attribution:

```sh
docker compose exec vault-sync \
  git --git-dir=/snapshots/vault.git log --oneline -5
```

You should see `pre-agent: <id>` and `agent: <id>` authored by `Claude Code`.

Also confirm nothing git-shaped leaked into the vault:

```sh
ls -a /share/CACHEDEV1_DATA/obsidian/vault | grep -E '^\.git' || echo "clean"
```

### 9. Backups

```sh
docker compose --profile manual run --rm backup
```

Schedule it daily on the host (QNAP crontab or a Container Station job), then
point Hybrid Backup Sync at `BACKUP_HOST_PATH` → Google Drive.

**Never at `SNAPSHOT_HOST_PATH`** — a live repo is files under mutation and will
sync torn. **Never at `HOME_HOST_PATH`** — that is a live Anthropic token.

### 10. Restore-test once. Do not skip this.

```sh
git clone /share/CACHEDEV1_DATA/obsidian/backups/daily/vault-<stamp>.bundle /tmp/restore-test
ls /tmp/restore-test
git -C /tmp/restore-test log --oneline | head
```

Encrypted bundles need decrypting first:

```sh
age -d -i ~/age-key.txt vault-<stamp>.bundle.age > /tmp/v.bundle
git clone /tmp/v.bundle /tmp/restore-test
```

An untested backup is a rumour.

---

## Operating

### Deploy a new version

```sh
./scripts/deploy.sh              # everything; re-pair the phone afterwards
./scripts/deploy.sh --sync-only  # leave the agent session alone
```

Deployment is manual on purpose. `vault-claude` holds a live tmux session your
phone is paired to, and an unattended `pull && up -d` would eventually tear it
down mid-conversation.

### Audit what the agent did

```sh
docker compose exec vault-sync \
  git --git-dir=/snapshots/vault.git log --author="Claude Code" --since=1.week --stat
```

Given the agent writes unattended, this is arguably worth more than the undo.

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
| **Writes** | **Atomic temp-then-rename, always** — write to a temp file on the same filesystem, then `rename()`. See [`INITIAL_PLAN.md`](INITIAL_PLAN.md) §8 for why this matters more here than usual. |

Commits need no coordination: the hourly backstop picks up any subtree, and
`git log -- <subtree>/` gives attribution for free.

---

## Known risk

`ob sync --continuous` and Claude Code's file tools write to the same directory
concurrently, and Claude's writes are not guaranteed atomic. Partial writes can
propagate to other devices via Sync.

The snapshots make this **recoverable, not prevented**. If it bites, escalate in
the order given in [`INITIAL_PLAN.md`](INITIAL_PLAN.md) §8 — temp-then-rename
wrapper, then serialising sync against agent sessions with a lock file, then
switching to an MCP server with atomic writes.

---

## Conventions

- All scripts are `set -euo pipefail` and shellcheck-clean in CI.
- Two bug classes are called out inline where they were originally hit:
  `[ cond ] && cmd` under `set -e`, and `pipefail` with a glob over an empty
  directory. Both fail silently. Do not reintroduce them.
- The git repo lives outside the vault. Exclusions belong in
  `$GIT_DIR/info/exclude`, written by `snapshot.sh` — never a `.gitignore` in
  the vault.
