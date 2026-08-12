# Architecture

What this system is and how the parts fit together.

- **Operating it** — [`README.md`](README.md)
- **Why it is this way** — [`DECISIONS.md`](DECISIONS.md)

> Headings here are stable anchors. Code comments link to them by name
> (`ARCHITECTURE.md#invariants`), not by section number — the previous document
> numbered its sections and the numbering drifted out of sync within a week.

---

## What it does

An always-on Claude Code session rooted in an Obsidian vault, running on a QNAP
NAS, drivable from an iPhone — with git version history and verified,
encrypted, off-site backups.

**No inbound connections anywhere.** The agent and the phone both dial *out* to
Anthropic's Remote Control bridge, which pairs them. That is why the UniFi
gateway needs no configuration: no port forwarding, no public endpoint, no
tunnel, and nothing to expose if the ISP uses CGNAT.

```mermaid
flowchart TB
    subgraph clients["Devices"]
        mac["MacBook<br/>Obsidian"]
        iphone["iPhone<br/>Obsidian + Claude"]
    end

    subgraph anth["Anthropic"]
        bridge["Remote Control bridge"]
    end

    subgraph obsc["Obsidian"]
        syncsvc["Obsidian Sync"]
    end

    subgraph nas["QNAP — Container Station"]
        agent["vault-claude"]
        sync["vault-sync"]
        cron["vault-cron"]
        vault[("/vault")]
        snap[("/snapshots/vault.git")]
        bundles[("/backups<br/>vault-latest + hourly/daily/monthly")]
    end

    gdrive["Google Drive<br/>via Hybrid Backup Sync"]

    iphone -->|outbound| bridge
    agent -->|dials out| bridge
    mac <--> syncsvc
    iphone <--> syncsvc
    syncsvc <--> sync
    agent --> vault
    sync --> vault
    agent -.->|SessionStart / Stop hooks| snap
    sync -.->|hourly backstop| snap
    cron --> snap
    cron --> bundles
    bundles --> gdrive
```

---

## Services

One image, four roles. Sync and the agent are separated because their
**lifecycles differ** — the agent gets restarted for version bumps and wedged
sessions, and that must not interrupt sync.

| Service | Command | Lifecycle | Mounts |
|---|---|---|---|
| `vault-sync` | `ob sync --continuous` + hourly snapshot backstop | always on | vault, snapshots, `home-sync` |
| `vault-claude` | `claude remote-control` in tmux | always on, restarted often | vault, snapshots, `home-agent` |
| `vault-cron` | supercronic → `backup.sh` on `BACKUP_SCHEDULE` | always on | snapshots, backups |
| `backup` | one `backup.sh` run | `profile: manual`, ad-hoc | snapshots, backups |

**Hostnames are pinned** (`nas-vault-sync` etc.). `ob` reports the hostname to
Obsidian Sync as the device name and Docker defaults it to the container ID, so
without pinning every recreate registers a new phantom device.

**`vault-cron` runs `backup.sh` directly**, over the same volumes. It does not
`docker run` a sibling container, which would require mounting
`/var/run/docker.sock` — effective root on the NAS for no benefit.

**tmux wraps the agent** because `claude remote-control` is a TTY application
that prints a pairing QR, not a daemon. tmux allows detach, reattach over
`docker exec`, and restarting the agent without recreating the container.

---

## Volumes

```
/share/CE_CACHEDEV4_DATA/obsidian/     encrypted volume (CE_ prefix), auto-unlock
├── vault/        the Obsidian vault      1002:100
├── snapshots/    vault.git + state       1002:100
├── backups/      bundles                 1002:100   <- the only one HBS touches
├── home-sync/    Obsidian credentials    1002:100  0700
└── home-agent/   Claude OAuth token      1002:100  0700
```

**The credential volumes are split on purpose.** `vault-sync` mounts only
`home-sync`, `vault-claude` mounts only `home-agent`, and the backup path mounts
neither. The agent reads a corpus it did not entirely author, so a prompt
injection can make it read its own filesystem; splitting means one compromise
yields one credential rather than both.

**Identity is `1002:100`** — a real QNAP account (`kylejs`), not the
conventional-looking 1000. See [DECISIONS.md](DECISIONS.md#identity-1002-not-1000).

`APP_UID`/`APP_GID` are **build args**, so changing them means rebuilding the
image, not editing `.env`. Docker resolves `$HOME` from `/etc/passwd`; a uid the
image does not know gets `$HOME=/`, and both interactive logins then appear to
succeed and silently fail to persist.

---

## Snapshots

The repository lives **outside** the vault: `GIT_DIR=/snapshots/vault.git`,
`GIT_WORK_TREE=/vault`. No `.git` or `.gitignore` ever appears inside the vault,
so Obsidian's indexer and the sync client never see them and nothing git-shaped
propagates to the phone. Exclusions live in `$GIT_DIR/info/exclude`, rewritten
on every run so config changes take effect without recreating the repo.

```mermaid
sequenceDiagram
    participant P as iPhone
    participant A as vault-claude
    participant V as /vault
    participant G as vault.git
    participant S as vault-sync

    P->>A: start a session
    A->>G: SessionStart hook — commit "pre-agent: id"
    Note over G: author: Claude Code agent@vault.local
    A->>V: reads and writes notes
    A->>G: Stop hook — commit "agent: id"

    loop every hour
        S->>G: commit "snapshot: ts" if dirty
        Note over G: author: human — catches Mac/phone edits
    end
```

Commits **bracket agent runs via hooks, not a timer** — a timer can fire mid-run
and capture a half-finished state. The hourly loop is only a backstop for human
edits, which hooks never observe. `flock` serialises the two.

Agent writes are attributed to `Claude Code <agent@vault.local>`, which makes the
repo an audit log:

```sh
git --git-dir=/snapshots/vault.git log --author="Claude Code" --since=1.week --stat
```

Given the agent writes unattended, that audit trail is arguably worth more than
the undo capability.

**`.claude/settings.json` is committed** — it is inside the vault and not
excluded — so the agent's tool policy restores along with the notes.

---

## Backups

```mermaid
flowchart LR
    g[("vault.git")] -->|HEAD moved?| ch{changed}
    ch -->|no| skip["skip — no write, no upload"]
    ch -->|yes| b["git bundle --all"]
    b --> v{"git bundle verify"}
    v -->|no| fail["abort, keep last good"]
    v -->|yes| enc["age encrypt"]
    enc -->|atomic mv| latest[("vault-latest.bundle.age")]
    latest -.->|first of hour/day/month| keep[("hourly/ x2<br/>daily/ x7<br/>monthly/ x3")]
    latest --> gd["Google Drive<br/>Hybrid Backup Sync"]
```

**One bundle, replaced in place.** A bundle contains the *complete history*, so
a single current file already provides every point in time:

```sh
age -d -i key.txt vault-latest.bundle.age > v.bundle
git clone v.bundle restored
git -C restored checkout 'HEAD@{2026-08-01}'
```

**The stamped copies are corruption insurance, not history.** A corrupted,
ransomwared or history-rewritten repo gets faithfully bundled straight over the
top of the good copy. Retention is sized by *how long until you would notice*,
not by how far back you want to read.

The tiers are that noticing window at three granularities. `hourly/` bounds how
much *work* a bad bundle can cost: without it, the newest copy predating the
damage is the day's first bundle, so a bad afternoon costs the whole day.

It is kept deliberately shallow — two changed hours — because attachments are in
git and a bundle is ~112 MB. Deeper hourly retention buys no recovery ability a
single bundle lacks, and rebuilds the pile removed on 2026-08-08.

**Runs where `HEAD` has not moved are skipped**, so an idle vault costs no
storage and no upload. Frequency therefore controls staleness, not cost.

**Backups are bundles, not a file-sync of the live repo.** A live repo is files
under mutation — packfiles, refs, `index.lock` — and a sync client will
eventually capture a torn state, producing a clone that fails `fsck` silently.

Two state files under `/snapshots`:

| File | Meaning |
|---|---|
| `.snapshot.lock` | `flock` target serialising hook and backstop commits |
| `.last-bundled-sha` | last successfully bundled `HEAD`; **mtime = when the job last ran**, touched even on skipped runs |

Those are two different questions — *did the job run* vs *did the vault change* —
and `preflight.sh` checks them separately. Conflating them makes an idle week
look identical to `vault-cron` being dead.

---

## Trust boundary

```mermaid
flowchart TB
    subgraph ci["GitHub Actions — low trust"]
        tok["GITHUB_TOKEN<br/>automatic, job-scoped, expires"]
    end

    subgraph qnap["QNAP — high trust"]
        home[("home-sync + home-agent<br/>login state")]
        env[("/.env — 0600")]
        agepub["age recipient<br/>PUBLIC key only"]
    end

    subgraph off["Off-NAS"]
        agepriv["age PRIVATE key<br/>1Password + paper"]
    end

    ci -.->|cannot reach| qnap
    home -->|established by| login["one-time interactive<br/>ob login / claude /login"]
```

The credentials that matter are **login state, not deploy-time configuration**.
They are established once by an interactive login *inside the container* and
persist in the volumes. They never enter GitHub Actions, which keeps CI's entire
secret inventory to `GITHUB_TOKEN`. A full compromise of the workflow gets an
attacker the ability to push a bad image tag and nothing else.

`age` is asymmetric so the NAS holds only the **encrypting** half. A NAS
compromise cannot decrypt existing backups; a passphrase scheme would have to
store the secret on the box and would hand over everything.

**Volume encryption defends drives that leave the building** — theft, RMA,
resale, disposal — and nothing at all against a running NAS. It is no help
against a QTS compromise, and no substitute for `AGE_RECIPIENT`, because Hybrid
Backup Sync reads the decrypted file and would upload plaintext.

The agent's tool policy lives in `<vault>/.claude/settings.json` and denies
`Bash`, `WebFetch`, `WebSearch`, reads/writes of the credential and snapshot
paths, and writes to `AGENTS.md`/`CLAUDE.md` at any depth. Those denied tools are
precisely the ones that turn a prompt injection into a breach.

**That policy binds `vault-claude` and nothing else.** Claude Code reads it;
`vault-mcp` does not, and the claude.ai client on the far end of `vault-mcp`
cannot be given one at all — it has web access this repo has no way to revoke.
So the two surfaces are protected differently on purpose: `vault-claude` reads
the whole vault with no way to send anything out, and `vault-mcp` keeps the
folders unvetted material lands in out of a conversation that does have one. See
[`../vault-mcp/README.md`](../vault-mcp/README.md#what-voice-cannot-see).

---

## Invariants

**Every rule here fails silently when broken.** That is what makes them worth
listing separately from the reasoning.

| Invariant | What breaks if violated |
|---|---|
| Hybrid Backup Sync points at `backups/` **only** | `snapshots/` is a live repo and will sync torn; the credential volumes hold a live OAuth token and your Obsidian login |
| Never create a QNAP **shared folder** for `snapshots`, `home-sync` or `home-agent` | `0700` is irrelevant once a directory is exported over SMB |
| Never relocate agent state (`.claude.json`) into the synced vault | This is the only reason we are immune to the upstream OneDrive corruption cascade |
| Never run a second Claude Code instance against `home-agent` | Concurrent instances corrupt `~/.claude.json`; `docker compose stop vault-claude` before re-authenticating |
| Never add `.claude` to `info/exclude` | The tool policy silently stops being backed up while every check still passes |
| Do not enable Obsidian config sync expecting it to carry `.claude/` | It will not, and expecting it to would widen who can edit the agent's own permissions |
| Keep `APP_UID` a **real, allocated** account | QNAP hands unallocated uids to the next account created, which would inherit the credential directories |
| All five paths stay on the same `CE_` volume | One on a plain volume silently leaves the encrypted set |
| `AGE_RECIPIENT` set ⇒ every published bundle is `.age` | A backup that reverted to plaintext looks identical to a working one |
| If skills move into the vault, keep them **write-denied** to the agent | Skills are instructions; an injected note authoring one is a persistent compromise |

`scripts/preflight.sh` asserts most of these and repairs the mechanical ones with
`--fix`. Run it after any QNAP change.

---

## Known unresolved risk

`ob sync --continuous` and Claude Code's file tools write to the same directory
concurrently, and Claude's writes are not guaranteed atomic. Partial writes can
propagate to other devices via Sync.

This is **not theoretical**. Claude Code has open issues about `~/.claude.json`
being corrupted by concurrent instances — no file locking, non-atomic writes
([#28847](https://github.com/anthropics/claude-code/issues/28847),
[#29217](https://github.com/anthropics/claude-code/issues/29217)). The
cautionary case is [#29153](https://github.com/anthropics/claude-code/issues/29153):
a home directory on OneDrive, so a sync client and Claude Code writing one tree.
It cascaded into 165 corrupted files in 40 minutes and a login loop, each
recovery attempt corrupted by the competing process.

We are immune to *that* cascade by construction — the credential volumes are
outside the synced vault — but the vault-side race remains. Snapshots make it
**recoverable, not prevented**. If it bites, escalate in order:

1. A temp-then-rename write wrapper. *Unreliable — agents do not consistently use
   a prescribed script.*
2. Serialise: pause sync while an agent session is active, via a lock file the
   sync loop respects.
3. Switch the agent to an MCP server with atomic writes, or take the Obsidian
   plugin route, where writes go through Obsidian's own vault API and the race
   disappears — both at the cost of Claude Code semantics.
