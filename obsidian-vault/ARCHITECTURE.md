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

    web["The open web"]

    subgraph nas["QNAP — Container Station"]
        agent["vault-claude<br/>no web tools"]
        move["vault-mcp -stdio<br/>move_file, session-scoped"]
        research["vault-research<br/>WebSearch + WebFetch"]
        fetch["vault-mcp -stdio<br/>+ fetch_attachment"]
        sweep["vault-research-sweep"]
        sync["vault-sync"]
        cron["vault-cron"]
        vault[("/vault")]
        scratch[("/scratch")]
        snap[("/snapshots/vault.git")]
        bundles[("/backups<br/>vault-latest + hourly/daily/monthly")]
    end

    gdrive["Google Drive<br/>via Hybrid Backup Sync"]

    iphone -->|outbound| bridge
    agent -->|dials out| bridge
    research -->|dials out| bridge
    mac <--> syncsvc
    iphone <--> syncsvc
    syncsvc <--> sync
    agent --> vault
    agent -->|spawns from .mcp.json| move
    move --> vault
    research --> scratch
    research -->|spawns from .mcp.json| fetch
    research -->|reads pages| web
    fetch -->|downloads files| web
    fetch --> scratch
    agent -.->|reads findings| scratch
    sweep -.->|deletes folders by age| scratch
    sync --> vault
    agent -.->|SessionStart / Stop hooks| snap
    sync -.->|hourly backstop| snap
    cron --> snap
    cron --> bundles
    bundles --> gdrive
```

**Read the arrows around `/scratch` as the whole security argument.**
`vault-research` touches the web and `/scratch`; `vault-claude` touches
`/vault` and reads `/scratch`. No process touches both the web and the vault.
The findings cross, the reach does not.

What this diagram does not show is time. Every arrow here is one session; the
vault is written by one and read by another later, which is a path of its own —
[The three-surface split is per session, not across time](#the-three-surface-split-is-per-session-not-across-time).

---

## Services

One image, six roles. Sync and the agents are separated because their
**lifecycles differ** — an agent gets restarted for version bumps and wedged
sessions, and that must not interrupt sync.

| Service | Command | Lifecycle | Mounts |
|---|---|---|---|
| `vault-sync` | `ob sync --continuous` + hourly snapshot backstop | always on | vault, snapshots, `home-sync` |
| `vault-claude` | `claude remote-control` in tmux | always on, restarted often | vault, snapshots, `home-agent` |
| `vault-research` | `claude remote-control` in tmux, rooted in scratch | always on, restarted often | **scratch, `home-research`** |
| `vault-research-sweep` | supercronic → `scratch-sweep.sh` on `SCRATCH_SWEEP_SCHEDULE` | always on | scratch |
| `vault-cron` | supercronic → `backup.sh` on `BACKUP_SCHEDULE` | always on | snapshots, backups |
| `backup` | one `backup.sh` run | `profile: manual`, ad-hoc | snapshots, backups |

**`vault-research`'s mount column is the security control**, not a detail of it.
It is the agent with `WebSearch` and `WebFetch`, and it is the only service here
that does not use the `*common` anchor — that anchor mounts the vault and the
snapshot repo, and inheriting them would put private data, untrusted web content
and a way out into one session. `research.sh` refuses to start if `/vault` or
`/snapshots` is present, so the rule is stated twice: once in the compose file
where it is easy to break, once at runtime where breaking it is caught.
See [DECISIONS.md](DECISIONS.md#a-third-surface-for-research).

**Hostnames are pinned** (`nas-vault-sync` etc.). `ob` reports the hostname to
Obsidian Sync as the device name and Docker defaults it to the container ID, so
without pinning every recreate registers a new phantom device.

**`vault-cron` runs `backup.sh` directly**, over the same volumes. It does not
`docker run` a sibling container, which would require mounting
`/var/run/docker.sock` — effective root on the NAS for no benefit. *For no
benefit* is the load-bearing half: the [`dashboard`](../dashboard/) stack mounts
that socket deliberately, because deploying from a browser needs it, and
[`DECISIONS.md`](DECISIONS.md#the-dashboard-and-the-docker-socket) records what
that costs. A cron job is still the case where it buys nothing.

**tmux wraps the agent** because `claude remote-control` is a TTY application
that prints a pairing QR, not a daemon. tmux allows detach, reattach over
`docker exec`, and restarting the agent without recreating the container.

**A further process runs inside each agent container, and nothing here starts
it.** Claude Code spawns `vault-mcp -stdio` from the project's `.mcp.json` as a
local MCP server, and it lives and dies with the session. It is the same binary
and the same Go source `vault-mcp` serves to voice from, built into this image —
which is why a change under `vault-mcp/` rebuilds this one.

The two agents get **different tools from that one binary**, selected by the
environment in their respective `.mcp.json`:

| | `VAULT_DIR` | Tools |
|---|---|---|
| `vault-claude` | `/vault` | `move_file`, `import_attachment` (`IMPORT_DIR=/scratch`) |
| `vault-research` | `/scratch` | `move_file`, `fetch_attachment` (`MCP_FETCH=1`) |

**The two extra tools are the two halves of one handoff, and never share a
surface.** `fetch_attachment` gets a file onto the NAS; `import_attachment` gets
it into the vault. `vault-mcp` refuses to start if one process is given both:
together they are "reach the open web, then write into the notes" in a single
session. Nothing else here creates a non-markdown file in the vault.

`move_file` exists because Claude Code has no move tool and `Bash` is denied, so
without it an agent cannot file or rename a note at all
([DECISIONS.md](DECISIONS.md#giving-the-agent-a-move)). `fetch_attachment`
exists because **nothing else can put an image or a PDF on disk**: WebFetch
returns text and never a file, and `Write` emits text so it cannot author the
bytes. (`Read` *does* open an image and show it to the model; that is why the
research policy denies `Read` on the fetchable extensions, and why it is not
what makes this tool necessary.) It is served only over stdio and only where
`MCP_FETCH=1`; setting
it on the HTTP surface is fatal at startup
([DECISIONS.md](DECISIONS.md#fetching-attachments)).

---

## Volumes

```
/share/CE_CACHEDEV4_DATA/obsidian/     encrypted volume (CE_ prefix), auto-unlock
├── vault/        the Obsidian vault      1002:100
├── snapshots/    vault.git + state       1002:100
├── backups/      bundles                 1002:100   <- the only one HBS touches
├── scratch/      research working dir    1002:100   <- swept on a schedule
├── home-sync/    Obsidian credentials    1002:100  0700
├── home-agent/   Claude OAuth token      1002:100  0700
└── home-research/ second Claude token    1002:100  0700
```

**The credential volumes are split on purpose.** `vault-sync` mounts only
`home-sync`, `vault-claude` mounts only `home-agent`, `vault-research` mounts
only `home-research`, and the backup path mounts none. The agents read corpora
they did not author, so a prompt injection can make one read its own
filesystem; splitting means one compromise yields one credential rather than
three.

`home-research` is additionally **required** rather than merely tidy: two Claude
Code instances against one home directory corrupt `~/.claude.json`, so pointing
both agents at `home-agent` breaks both. It needs its own one-time
`claude /login`.

**`scratch/` is outside the vault, and both halves of that matter.** It is
`vault-research`'s working directory, which is what keeps the vault out of the
one session that can reach the web. It is also the only directory anything here
deletes from — `scratch-sweep.sh` reaps folders older than
`SCRATCH_RETENTION_DAYS` — and a deleting script pointed inside a synced vault
would propagate its mistakes to every device. `preflight.sh` fails if `scratch`
and `vault` are ever nested, in either direction.
See [DECISIONS.md](DECISIONS.md#a-third-surface-for-research).

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

**The same attribution is also written into the note.** A `PostToolUse` hook
(`scripts/hook-stamp.sh`) stamps `agent-created`, `agent-modified` and `agent`
into the frontmatter of every note the agent writes. That is duplication on
purpose: this repository lives outside the vault, so it is invisible from
Obsidian on the phone, and a note that leaves the vault through Sync carries
none of its history. The repo answers *what changed*; the stamp answers *who
last wrote this note* from inside the note. `vault-mcp` writes the same three
properties as `claude-voice`, which makes it a shared contract rather than a
feature of this stack — see the root [`README.md`](../README.md#shared-contract)
and [`DECISIONS.md`](DECISIONS.md#agent-stamps-in-frontmatter).

**`.claude/settings.json` is committed** — it is inside the vault and not
excluded — so the agent's tool policy restores along with the notes.

It is also **installed from the image on every agent start**, by
`install-settings.sh`, from the copy of `vault-claude-settings.json` baked in
beside the hook scripts it references. The file in the repo is the source of
truth and the one in the vault is a materialisation of it, which is why the two
cannot be deployed out of step: the hooks and the file that wires them up travel
in the same image. Both properties hold at once — it is a real file in the
vault, so it is committed and restored, and it is regenerated on start, so it
cannot silently lag the repo. See
[`DECISIONS.md`](DECISIONS.md#shipping-the-tool-policy-with-the-image).

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
    latest -.->|first of hour/day/month| keep[("hourly/ x18<br/>daily/ x7<br/>monthly/ x3")]
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

It is sized to a waking day — 18 changed hours — so damage introduced and
noticed within the same day is recoverable to the hour. Past a day there is
nothing further to buy, since a single bundle already holds every point in time;
`daily/` takes over there. At ~112 MB a copy that is ~2 GB, the deliberate cost
of the window. See DECISIONS.md#backups-bundles-and-why-only-one.

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

`vault-claude` refuses to start when no tool policy is present, so "the agent
ran without one" is a crash-looping container rather than a silent condition.

The agent's tool policy lives in `<vault>/.claude/settings.json` and denies
`Bash`, `WebFetch`, `WebSearch`, reads/writes of the credential and snapshot
paths, `<vault>/.mcp.json` at any depth, and writes to `AGENTS.md`/`CLAUDE.md` at
any depth. Those denied tools are precisely the ones that turn a prompt injection
into a breach.

**A path rule's leading slash is not what it looks like.** A single `/` anchors
at the settings source — for this file, `<vault>` — so `Read(/snapshots/**)`
denied `/vault/snapshots`, not `/snapshots`, and protected nothing from
2026-08-08 to 2026-08-30. Absolute paths take `//`. Rules for `Write(path)` are
never consulted at all; `Read` and `Edit` are the only path rules the permission
check reads, and a `Read` deny covers writes to the same path.
[DECISIONS.md#the-snapshots-deny-that-was-not-one](DECISIONS.md#the-snapshots-deny-that-was-not-one).

**`.mcp.json` is denied for a stronger reason than the rest.** An entry added
there is an arbitrary command Claude Code executes at the start of every future
session — the `AGENTS.md` case, "persistent compromise, not a one-shot", with
the shell the agent does not otherwise have. It is denied at any depth, and the
deny list `vault-mcp` mirrors already refuses every dotted path, so the move tool
cannot be used to shuffle one into place either.

**The move tool does not widen this boundary.** It is not a shell: one tool that
relocates one file, enforcing the same deny list in Go — including on the
*source*, so a note cannot be moved out of the way to make `CLAUDE.md` stop being
`CLAUDE.md`. It opens no socket, makes no network call, and has no credential
mounted. Prompt injection's third leg, egress, is exactly as absent as it was.

It moves attachments as well as notes, and that does not change the paragraph
above: the file *types* it may touch are wider than the rest of the server's,
the *paths* are identical. A move also creates nothing — it relocates bytes
already in the vault — so there is no new content for an injected instruction to
author, only a file it could put somewhere unhelpful, which the snapshot repo
undoes.

**That policy binds `vault-claude` and nothing else.** Claude Code reads it;
`vault-mcp` does not, and the claude.ai client on the far end of `vault-mcp`
cannot be given one at all — it has web access this repo has no way to revoke.
So the surfaces are protected differently on purpose. See
[`../vault-mcp/README.md`](../vault-mcp/README.md#what-voice-cannot-see).

### Three surfaces, three different mitigations

Prompt injection needs three things at once: private data, untrusted content,
and a way out. No surface here has all three. Each is missing a different one,
and that is the whole design.

This holds **per session**. It does not hold across sessions, because all three
surfaces read and write one vault — see
[The three-surface split is per session, not across time](#the-three-surface-split-is-per-session-not-across-time).

| | Private data | Untrusted content | Egress |
|---|---|---|---|
| `vault-claude` | the whole vault | yes — the Inbox, and `/scratch` | **none** |
| `vault-mcp` (voice) | **minus `MCP_EXCLUDE`** | mostly kept out | claude.ai's, not ours to set |
| `vault-research` | **none** | yes — the open web | `WebSearch`, `WebFetch`, `fetch_attachment` |

`vault-research` is the surface added on 2026-08-31, and it inverts the trade
the other two make. It holds the web tools the other two are denied, and pays
for them by having nothing of yours to lose: its working directory is
`/scratch`, and **the vault is not mounted into its container.** Findings cross
to `vault-claude` through `/scratch`, read-only, in that direction only.

The enforcement is a missing mount, not a rule. That is deliberate: this repo
has already shipped a deny list that denied nothing for three weeks
([DECISIONS.md](DECISIONS.md#the-snapshots-deny-that-was-not-one)), and a
mount that is absent cannot be silently mis-spelled. `research.sh` refuses to
start if `/vault` or `/snapshots` appears, and `preflight.sh` fails if the
scratch and vault paths are ever nested.

**`fetch_attachment` is egress that returns no bytes to the model.** It
downloads a URL straight to a file and hands back a name, a size and a type.
That is why it is on the research surface and not on `vault-claude`: it is
still a way out, and a way out beside the whole vault is the thing this table
exists to prevent. [DECISIONS.md](DECISIONS.md#fetching-attachments).

---

## Invariants

**Every rule here fails silently when broken.** That is what makes them worth
listing separately from the reasoning.

| Invariant | What breaks if violated |
|---|---|
| Hybrid Backup Sync points at `backups/` **only** | `snapshots/` is a live repo and will sync torn; the credential volumes hold a live OAuth token and your Obsidian login |
| Never create a QNAP **shared folder** for `snapshots`, `home-sync` or `home-agent` | `0700` is irrelevant once a directory is exported over SMB |
| Never relocate agent state (`.claude.json`) into the synced vault | This is the only reason we are immune to the upstream OneDrive corruption cascade |
| Never run a second Claude Code instance against `home-agent` | Concurrent instances corrupt `~/.claude.json`; `docker compose stop vault-claude` before re-authenticating. This is why `vault-research` has its own `home-research` volume and its own login — sharing one would break both agents, not isolate them |
| **Never mount the vault, or the snapshot repo, into `vault-research`** | That container has `WebSearch`, `WebFetch` and `fetch_attachment`. With your notes also present it becomes one session holding private data, untrusted content and a way out — every mitigation in this document at once. `research.sh` refuses to start if it finds either, and `vault-research` deliberately does not use the `*common` compose anchor |
| **`/scratch` stays outside the vault, and `vault-claude` mounts it `:ro`** | `scratch-sweep.sh` is the only thing in this repo that deletes anything; pointed inside the vault it deletes notes and Obsidian Sync propagates that to every device. The `:ro` keeps the sweeper the only deleter by construction rather than by a rule |
| Never add `.claude` to `info/exclude` | The tool policy silently stops being backed up while every check still passes |
| An absolute path in a permission rule is spelled `//path`, and a path rule is written for `Read` or `Edit`, never `Write` | Both mistakes are accepted silently: `/snapshots/**` denies `<vault>/snapshots`, and a `Write(path)` rule is never consulted. The deny list keeps its shape while protecting nothing |
| Do not enable Obsidian config sync expecting it to carry `.claude/` | It will not, and expecting it to would widen who can edit the agent's own permissions |
| Keep `APP_UID` a **real, allocated** account | QNAP hands unallocated uids to the next account created, which would inherit the credential directories |
| All five paths stay on the same `CE_` volume | One on a plain volume silently leaves the encrypted set |
| `AGE_RECIPIENT` set ⇒ every published bundle is `.age` | A backup that reverted to plaintext looks identical to a working one |
| If skills move into the vault, keep them **write-denied** to the agent | Skills are instructions; an injected note authoring one is a persistent compromise |
| Every surface that writes into the vault stamps, under the **same** property names | Half the agent writes stop answering a query written against the other half, and the notes they touched read as human-authored |
| `hook-stamp.sh` mirrors the deny list rather than relying on `settings.json` | Hooks run outside the permission system, so the stamping hook becomes a writer that can reach `CLAUDE.md` |
| A stamped note is the note the agent wrote plus the stamp lines, and nothing else | A misread frontmatter block puts properties into prose, silently, one note at a time |
| Edit `vault-claude-settings.json` in the repo, never the vault's copy | The next agent start reinstalls it from the image and the edit is gone — with no error, and no sign it was ever applied |
| `research-CLAUDE.md` stays exempted in **both** the root `.dockerignore` and `build-obsidian-vault.yml`'s path filter | It is the only markdown file the image copies. Missing from `.dockerignore`'s exception it is absent from the build context and the build fails loudly; missing from the workflow filter it fails *silently* — editing it publishes no image, the running agent keeps the old instructions, and every check stays green |
| The same applies to `vault-claude-mcp.json` and `<vault>/.mcp.json` | Identical failure, and the symptom is worse: the agent silently has no move tool and reports notes filed that were not |
| `<vault>/.mcp.json` stays write-denied to the agent, at any depth | An entry added there is a command every future session executes — the `AGENTS.md` compromise, with a shell |
| Never add a second tool to the `-stdio` surface without deciding about the stamp | MCP tool calls fire no `PostToolUse` hook, so a second write tool reaches the vault unstamped while looking like any other edit. `fetch_attachment` is the one addition, and it clears this because it writes to `/scratch` rather than the vault — the same tool served with `VAULT_DIR=/vault` would not |
| Relocating and importing are the only things any surface does to a non-markdown file, and nothing anywhere may **read** one | A read or a create for attachments turns a note server into a general file server. Revised twice on 2026-08-31: `fetch_attachment` creates them in `/scratch`, and `import_attachment` copies one from there into the vault. Both are narrow on purpose — the import source is a configured root outside the vault, attachments only, extension unchanged, never overwriting. A *read* would still end the rule |
| `MCP_FETCH` and `IMPORT_DIR` are never set on the same process | Together they are "reach the open web, then write into the vault" in one session. `loadConfig` refuses to start; before that check, only the compose file and two `.mcp.json` files kept them apart, and a test asserting the split failed when it was written |
| An attachment move is unstamped **by necessity** — do not assume the frontmatter covers every agent write | A dataview query over `agent-modified` silently misses every image the agent ever filed; with `EXCLUDE_ATTACHMENTS=1` the audit log is the only record there is |

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

---

### The three-surface split is per session, not across time

Added 2026-09-01, while walking through what actually happens when research
results are filed. The table in
[Three surfaces, three different mitigations](#three-surfaces-three-different-mitigations)
describes the tools available to **one session**. It does not describe what is
reachable across sessions, and **the vault is a medium all three share**.

The path, concretely:

1. `vault-claude` reads research findings from `/scratch`. That is
   attacker-authored text, which is the point of the handoff and is safe on its
   own: this surface has no egress.
2. An injection in it tells `vault-claude` to copy something out of a folder
   named in `MCP_EXCLUDE` — `Clippings` today — into an ordinary note.
   `vault-claude` can read the whole vault and write nearly all of it, so this
   is within its powers.
3. Later, a voice session reads that ordinary note. `MCP_EXCLUDE` does not hide
   it, because it is no longer in an excluded folder.
4. The note is attacker-authored by then, so it can carry instructions, and
   claude.ai has web access this repo cannot revoke.

That completes injection to exfiltration from a single starting point, using no
surface that ever held all three legs at once. Each step is something its
surface is allowed to do.

**Why it is not treated as blocking.** It needs a later voice session to read
that particular note, which the user drives. It moves data rather than
credentials. And each step leaves a record: the write is a snapshot commit with
an `agent-modified` stamp naming the writer, so *"how did this appear in a note
I did not write"* is a `git log` rather than a mystery. Detection is not
prevention, and this is recorded as unresolved for that reason.

**What would actually close it** is per-folder write rules on `vault-claude` —
it may read `Clippings` but not copy out of it — which Claude Code's permission
system cannot express, since a `Read` deny is on the destination path and not on
the provenance of the bytes. Short of that, the honest mitigations are to keep
`MCP_EXCLUDE` small enough that little depends on it, and to read the snapshot
diff after a session that ingested research.

**Do not paper over this by widening `MCP_EXCLUDE`.** The exclusion is what
keeps unvetted material out of the conversation that has egress; growing it to
cover everything sensitive would make voice useless without closing the copy
path, which starts from a folder the agent may legitimately read.
