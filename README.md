# homelab

Container stacks running on the QNAP NAS. One repo so there's a single place
that says what's deployed; **separate images and separate compose stacks** so
they deploy independently.

## Stacks

| Stack | What it is | Status |
|---|---|---|
| [`obsidian-vault/`](obsidian-vault/) | Two always-on Claude Code sessions drivable from an iPhone: one rooted in the Obsidian vault with no network access, one with web search and fetch and no vault. Git version history + verified off-site backups. | **Running**, restore-tested |
| [`vault-mcp/`](vault-mcp/) | The vault as a remote MCP server, so Claude **voice mode** can search and capture hands-free. The one stack reachable from the internet. Its binary is also the vault agent's move tool, run locally over stdio. | **Running**, OAuth 2.1 |
| [`dashboard/`](dashboard/) | One page saying what is running, what is out of date, and the buttons to deploy and restart it. The one stack that publishes a port, and the one that holds the Docker socket. | **Running**, tailnet only |
| `fishing/` | Fishing/weather data collection, writing derived notes into the vault. | In progress, separate session |

**Not every container on this host is in this repo.** `home-assistant`, `esphome`
and `matter-server` also run under Container Station and are managed outside it.
They share nothing with the stacks above — no vault mount, no tailnet identity —
so the shared contract does not reach them, but the *"single place that says
what's deployed"* claim above is only true of the stacks listed above. `docker
ps` is still the ground truth for the host, which is why
[`dashboard/`](dashboard/) lists those containers too, read-only and with no
buttons.

For the vault stack: [`ARCHITECTURE.md`](obsidian-vault/ARCHITECTURE.md) is what
it is, [`DECISIONS.md`](obsidian-vault/DECISIONS.md) is why, and
[`README.md`](obsidian-vault/README.md) is how to operate it.

## The host

| | |
|---|---|
| Hardware | QNAP, Intel Celeron J4125, 4 cores, x86-64, **no AVX2** |
| Memory | 16 GB, ~11.7 GB free |
| OS | QTS 5.2.10 (build 20260722) |
| Runtime | Container Station |
| Access | Tailscale — the NAS is its own tailnet node (`kyles-nas`), not behind a subnet router |

### Reaching the host

**This row has been wrong in both directions in a single day.** It described a
tailnet node that had never been installed; that was corrected on 2026-08-12 to
"LAN SSH, not on the tailnet"; the node was then actually installed a few hours
later, making the correction stale in turn. It is now true as written — checked
against the host, not inferred from intent. Check before correcting it again.

Host Tailscale is the **App Center** package (Communications category), installed
2026-08-12, separate from the `vault-mcp` funnel sidecar — two nodes, two state
directories, no interaction. SSH in over the tailnet name rather than the LAN
address, so the same config works from anywhere:

```sh
ssh admin@kyles-nas.tail3df177.ts.net
```

**Tailscale SSH is off** (`RunSSH: false`) and staying off — access is by SSH key
through the 1Password agent. That is a decision, not a gap; the reasoning is in
[`DECISIONS.md`](obsidian-vault/DECISIONS.md#tailscale-ssh-considered-and-rejected),
and the short version is that QNAP's `sshd` has to keep running for the LAN
fallback anyway, so enabling it would add a second SSH server rather than replace
one.

The node is **untagged** and staying that way, so ACLs cannot target it by tag —
do not write anything that assumes ACL-governed access to it. Tagging would have
cost a re-authentication of the one node that cannot be re-authenticated
remotely, to buy things this tailnet does not use;
[`DECISIONS.md`](obsidian-vault/DECISIONS.md#tagging-rejected-and-the-expiry-hazard-it-was-hiding)
has the assessment.

**Key expiry is disabled on `kyles-nas`, and must stay that way** — confirmed
2026-08-12. Untagged nodes expire by default and this one was 179 days out. On
expiry the node leaves the tailnet, recoverable only by running `tailscale up` on
the box, which needs the access that just expired: a scheduled lockout, not an
inconvenience. Nothing in this repo enforces it, so verify rather than trust:

```sh
tailscale status --json | \
  python3 -c "import json,sys; d=json.load(sys.stdin); \
  print({v['HostName']: v.get('KeyExpiry','never') for v in d['Peer'].values()})"
```

**Do not install Tailscale from the App Center.** The listing is a curated shelf
that QNAP has to republish for each release, and it has stalled: it shipped
**1.40.0** — from 2023 — long after 1.102.2 was stable. This is not a stale cache
on your NAS; everyone installing from App Center gets 1.40.0
([tailscale-qpkg#130](https://github.com/tailscale/tailscale-qpkg/issues/130)).
`tailscale update` is no help either; it reports *"not supported on this
platform"* on QNAP.

The host was upgraded to **1.102.2** on 2026-08-12 by downloading the QPKG from
[pkgs.tailscale.com](https://pkgs.tailscale.com/stable/) and using *App Center →
Install Manually*. That registers properly — `qpkg.conf` shows the real version
and build — and **preserves node state**: same tailnet IP, no re-auth, prefs
intact. So the same route works for the next upgrade.

**This is now a recurring manual chore.** Nothing will notify you. The marker is
`Build` in `qpkg.conf` (currently `20260804`); when `pkgs.tailscale.com` is newer
than that, it is time for another pass.

**Find QPKG paths per package, never by guessing the volume.** The two packages
that matter here sit on *different* volumes, and neither is the volume the repo
is checked out on (`/share/CE_CACHEDEV4_DATA`), so any hardcoded path is wrong
somewhere:

```sh
getcfg Tailscale         Install_Path -f /etc/config/qpkg.conf  # /share/CACHEDEV1_DATA/.qpkg/Tailscale
getcfg container-station Install_Path -f /etc/config/qpkg.conf  # /share/CE_CACHEDEV2_DATA/.qpkg/container-station
```

Tailscale's own docs suggest `getcfg SHARE_DEF defVolMP -f /etc/config/def_share.info`,
but that returns the *default* volume — correct for Tailscale here and wrong for
Container Station. Ask `qpkg.conf` about the specific package instead. This is
the same trap [`DECISIONS.md`](obsidian-vault/DECISIONS.md#traps-found-while-building)
records about volume names not being predictable.

**`docker` is not on the PATH of a non-interactive SSH session** — `ssh nas
docker ps` fails with `command not found` even though an interactive login works.
Anything scripted has to put it there first:

```sh
export PATH=$PATH:$(getcfg container-station Install_Path -f /etc/config/qpkg.conf)/bin
```

**Nothing needs configuration on the UniFi gateway**, and that has survived both
of the stacks added since it was written. `vault-mcp`'s Tailscale sidecar dials
*out* and reaches the server over a compose network rather than a host port;
`dashboard` publishes a port, but on loopback only, reached through `tailscale
serve`. The gateway stays a bystander either way.

Two things about that have changed, in opposite directions:

- One service is **reachable from the internet** — `vault-mcp`, over a Funnel
  whose TLS terminates on the NAS rather than at a third party. See
  [`vault-mcp/README.md`](vault-mcp/README.md#trust-boundary).
- **One stack now publishes a host port**, which was previously true of none.
  `dashboard` binds `127.0.0.1:${DASH_PORT:-8088}`, and `tailscale serve` on the
  NAS's own tailnet node puts it at `https://kyles-nas.tail3df177.ts.net/` with a
  real certificate. **No LAN path**, and that is a security control rather than
  an omission: the dashboard authenticates by the tailnet identity header Serve
  adds, so a listener the LAN could reach would be a deploy button the LAN could
  press. It is deliberately not on a Funnel. That reversal, and the Docker socket
  that comes with it, are assessed in
  [`DECISIONS.md`](obsidian-vault/DECISIONS.md#the-dashboard-and-the-docker-socket);
  the move from a pasted token to tailnet identity is in
  [`DECISIONS.md`](obsidian-vault/DECISIONS.md#updating-services-identity-at-the-door-digests-in-git).

## Shared contract

Anything that writes into the vault must honour these. They are not stylistic.

| Item | Rule |
|---|---|
| **Sync** | `vault-sync` owns it. **No other container runs `ob sync`.** Two headless Obsidian clients on one vault means two sync engines fighting over the same local state, plus a second device against the Sync subscription. |
| **UID/GID** | Identical `APP_UID:APP_GID` in every stack. A mismatch produces a confusing symptom: writes appear to work, the Mac sees unreadable files. |
| **Writes** | **Atomic temp-then-rename, always.** Write to a temp file on the same filesystem, then `rename()`. Claude Code's own writes are not atomic and this is a documented corruption source — see [`ARCHITECTURE.md`](obsidian-vault/ARCHITECTURE.md#known-unresolved-risk). |
| **Raw caches** | Stay **outside** `/vault`. Git keeps every version of every blob permanently, and those bundles go to Google Drive. Only derived notes belong in the vault. |
| **Stamps** | Every write made on an agent's behalf sets `agent-modified` and `agent` in the note's YAML frontmatter, plus `agent-created` when that write created the note. A move is a write and sets the first two, never the third — filing a note is not authoring it. The keys are deliberately generic and the identity is the *value* — `claude-voice` for `vault-mcp` serving voice, `claude-agent` for `vault-claude` and for the move tool it runs — so a non-Claude writer fills in the same three properties instead of inventing a fourth schema. A writer that skips this does not merely omit a label: it makes every query over the vault wrong in the direction that matters, because the notes it touched come back looking human-authored. Implemented in `vault-mcp/stamp.go` and `obsidian-vault/scripts/hook-stamp.sh`. |
| **Stamps: the one exception** | **An attachment moved by an agent carries no stamp, because a PNG has nowhere to put YAML.** This is a genuine hole in the rule above, not an oversight, and it is stated here rather than left for someone to discover from a dataview query that misses files. What records an attachment move instead: the snapshot commit, and the `move_file` line in `vault-mcp`'s audit log. If the vault runs `EXCLUDE_ATTACHMENTS=1` the commit is not one of them either, and the audit log is the only trace. A sidecar `.md` per attachment was considered and rejected — it doubles the file count in the vault to describe files Obsidian already shows you, and Sync would carry the sidecar and the image as unrelated objects. |
| **Agent policy** | Anything that writes into the vault on Claude's behalf must enforce the deny list in `obsidian-vault/vault-claude-settings.json` — no `.claude/`, no `.mcp.json`, no `AGENTS.md`/`CLAUDE.md` at any depth, and *moving* one of those out of the way counts as writing to it. A second writer that skips it *is* the bypass around the agent's own permissions. `vault-mcp` reimplements it in `vault.go`, asserted by tests. It additionally hides the folders unvetted material lands in, which is deliberately *not* mirrored back — [why](vault-mcp/README.md#what-voice-cannot-see). |
| **Web access and the vault never share a session** | Three surfaces, each missing a different leg of private data / untrusted content / egress. `vault-claude` has the vault and no network. `vault-mcp` serves a client that has the network, so the folders unvetted material lands in are hidden from it. `vault-research` has the network and no vault — enforced by a **missing mount**, not a rule, so it cannot be silently mis-spelled. Anything new that reads the vault must state which leg it gives up. [Why](obsidian-vault/DECISIONS.md#a-third-surface-for-research). |
| **Move, never copy-and-delete** | Renaming or refiling a note or an attachment is `rename(2)`. A copy doubles it on disk until the delete lands, and `ob sync` reads the directory continuously — it propagates the duplicate and then the deletion, which is a sync conflict wearing a move's clothes. Nothing here deletes, so a half-finished copy is also a mess only a human can clear up. For attachments the copy would be the whole file: a move is one syscall and the bytes never enter the mover's memory. |
| **Non-markdown files** | Moving one is the **only** thing any surface may do to a file that is not a note **inside the vault** — no create, no read, no edit, no delete. A move creates nothing: it relocates bytes a human already put in the vault, which is why it is the one safe exception. The movable set is an allow list of media, PDFs and canvases (`attachmentExts` in `vault-mcp/vault.go`), never code- or configuration-shaped files, and a move may never change a file's extension. Every path denial applies unchanged, at both ends. |
| **Non-markdown files: the 2026-08-31 exceptions** | Two, added together, and both narrow. `fetch_attachment` **creates** them, confined to `/scratch` — never the vault. `import_attachment` **copies** one from there into the vault, which is the only place any surface creates a non-markdown file in it: source confined to a configured root outside the vault, attachments only, extension unchanged, never overwriting, and a copy rather than a rename so the scratch sweeper stays the only deleter. No single process may hold both — `vault-mcp` refuses to start. They exist because nothing else can put an image on disk or move it between the two agents: WebFetch returns text and never a file, `Write` emits text so it cannot author bytes, and `Bash` is denied everywhere here. The rule above is unchanged for the vault, and *reading* an attachment is still refused on every surface. The fetchable set is deliberately narrower than the movable one — no SVG, no `.canvas`, nothing whose type cannot be sniffed — because this is the one tool bringing bytes in from outside. `vault-mcp/fetch.go`. |
| **Deploys** | Never restart another stack's containers *as a side effect of your own*. Verified 2026-08-08 that Remote Control pairing *does* survive a recreate, so this is no longer about losing the phone's session — but a deploy can still interrupt an agent run in progress. `dashboard` is the one stack that acts on the others, and only ever because a human pressed a button for one named stack; it has no `--all` and cannot act on itself. See [`dashboard/README.md`](dashboard/README.md#trust-boundary). |

Commits need no coordination: the hourly backstop in `vault-sync` picks up any
subtree, and `git log -- <subtree>/` gives attribution for free.

## Operating it

[`bin/homelab`](bin/homelab) is the entry point when you are SSH'd into the NAS.
It wraps the per-stack scripts rather than replacing them — `deploy` delegates to
a stack's own `scripts/deploy.sh` where one exists, so `--sync-only` and the
re-pair warning keep working.

```sh
bin/homelab stacks                 # the stacks this CLI can act on
bin/homelab status                 # every stack: containers, images, snapshot and voice freshness
bin/homelab deploy vault-mcp       # pull, verify provenance, recreate
bin/homelab env check vault-mcp    # .env mode and any key left blank
bin/homelab token rotate           # new MCP_TOKEN, recreate, print the header to paste into Claude
bin/homelab url                    # the connector URL the Funnel is actually serving
bin/homelab attach                 # the vault-claude tmux session
bin/homelab attach research        # the vault-research tmux session

bin/homelab update                 # git pull this checkout
bin/homelab git status             # git against this checkout
bin/homelab snapshots log --author='Claude Voice'   # the vault's own history
```

**Every command takes an explicit stack, and there is no `--all`** — the shared
contract below says never restart another stack's containers, and the cheapest
way to honour that is to make it unexpressible.

### The login banner

[`qnap/`](qnap/) is what puts `homelab` on your `PATH` in the first place, and
prints a short status banner — host, repo state, and a generated index of small
read-only helpers (`containers`, `disks`, `stacks`) — when you SSH in.

It exists as a directory rather than a dotfile because **QTS rebuilds `/etc`,
`/root` and `/usr` from firmware on every boot**, so the setup has to be
reinstated at each boot from the config partition. [`qnap/README.md`](qnap/README.md)
is how that works and how to restore it on a NAS that has lost its OS;
[`qnap/TESTING.md`](qnap/TESTING.md) is the checklist to run after changing it.
Nothing there needs Entware or host git, for the reason immediately below.

### Getting the repo onto the NAS without host git

Container Station gives QTS a `docker` binary but no `git`, and **installing one
via Entware is a bad trade**: Entware is a well-documented way to break the SSH
`PATH`, and the `PATH` is how you reach `docker` in the first place.

The CLI handles this itself — `homelab git`, `homelab update` and
`homelab snapshots` use host git when it exists and a pinned `alpine/git`
container when it does not. **So the NAS never needs git installed.**

The one thing the CLI cannot do is the *initial* clone, because it lives inside
the repo it would be cloning. That one stays a one-liner (the repo is public, so
no credential is involved):

```sh
cd /share/CE_CACHEDEV4_DATA
docker run --rm -v "$PWD:/git" -w /git -u "$(id -u):$(id -g)" -e HOME=/tmp \
    alpine/git clone https://github.com/ky1ejs/homelab.git
```

`-u` matters: without it the clone lands owned by root and the stack's own files
end up unreadable to the account everything else runs as. After that,
`bin/homelab update` does the pulling.

**Nothing else needs host git either.** Every snapshot commit happens inside a
container whose image already has it, and `homelab status` and
`homelab snapshots` borrow git from any running container that mounts
`/snapshots` when the host has none.

Output is ASCII-only and colour is dropped when stdout is not a terminal, so it
stays readable on a QTS session with no UTF-8 locale and pastes cleanly into a
chat window. Both properties are asserted in CI.

## CI

Each stack has its own path-filtered workflow, so a change to one cannot
rebuild or republish another.

| Workflow | Triggers on | Publishes |
|---|---|---|
| `build-obsidian-vault.yml` | `obsidian-vault/**` (excluding markdown) | `ghcr.io/<owner>/homelab/obsidian-vault` |
| `build-vault-mcp.yml` | `vault-mcp/**` (excluding markdown) | `ghcr.io/<owner>/homelab/vault-mcp` |
| `build-dashboard.yml` | `dashboard/**` (excluding markdown) | `ghcr.io/<owner>/homelab/dashboard` |
| `shellcheck-bin.yml` | `bin/**` | nothing — lints, asserts ASCII-only and the executable bit |

Images are public, so the NAS needs no registry credential. Builds carry
provenance attestations; `obsidian-vault/scripts/deploy.sh` verifies them when `gh` is
available.
