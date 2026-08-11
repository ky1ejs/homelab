# homelab

Container stacks running on the QNAP NAS. One repo so there's a single place
that says what's deployed; **separate images and separate compose stacks** so
they deploy independently.

## Stacks

| Stack | What it is | Status |
|---|---|---|
| [`obsidian-vault/`](obsidian-vault/) | Always-on Claude Code session rooted in the Obsidian vault, drivable from an iPhone. Git version history + verified off-site backups. | **Running**, restore-tested |
| [`vault-mcp/`](vault-mcp/) | The vault as a remote MCP server, so Claude **voice mode** can search and capture hands-free. The one stack reachable from the internet. | Built, not yet deployed |
| `fishing/` | Fishing/weather data collection, writing derived notes into the vault. | In progress, separate session |

For the vault stack: [`ARCHITECTURE.md`](obsidian-vault/ARCHITECTURE.md) is what
it is, [`DECISIONS.md`](obsidian-vault/DECISIONS.md) is why, and
[`README.md`](obsidian-vault/README.md) is how to operate it.

## The host

| | |
|---|---|
| Hardware | QNAP, Intel Celeron J4125, 4 cores, x86-64, **no AVX2** |
| Memory | 16 GB, ~11.7 GB free |
| Runtime | Container Station |
| Access | Tailscale — the NAS is its own tailnet node, not behind a subnet router |

No stack publishes ports and nothing needs configuration on the UniFi gateway —
still true with `vault-mcp`, because its Tailscale sidecar dials *out* and
reaches the server over a compose network rather than a host port. The gateway
stays a bystander; what changed is that one service is now **reachable from the
internet**, over a Funnel whose TLS terminates on the NAS rather than at a
third party. See [`vault-mcp/README.md`](vault-mcp/README.md#trust-boundary).

## Shared contract

Anything that writes into the vault must honour these. They are not stylistic.

| Item | Rule |
|---|---|
| **Sync** | `vault-sync` owns it. **No other container runs `ob sync`.** Two headless Obsidian clients on one vault means two sync engines fighting over the same local state, plus a second device against the Sync subscription. |
| **UID/GID** | Identical `APP_UID:APP_GID` in every stack. A mismatch produces a confusing symptom: writes appear to work, the Mac sees unreadable files. |
| **Writes** | **Atomic temp-then-rename, always.** Write to a temp file on the same filesystem, then `rename()`. Claude Code's own writes are not atomic and this is a documented corruption source — see [`ARCHITECTURE.md`](obsidian-vault/ARCHITECTURE.md#known-unresolved-risk). |
| **Raw caches** | Stay **outside** `/vault`. Git keeps every version of every blob permanently, and those bundles go to Google Drive. Only derived notes belong in the vault. |
| **Agent policy** | Anything that writes into the vault on Claude's behalf must enforce the deny list in `obsidian-vault/vault-claude-settings.json` — no `.claude/`, no `AGENTS.md`/`CLAUDE.md` at any depth. A second writer that skips it *is* the bypass around the agent's own permissions. `vault-mcp` reimplements it in `vault.go`, asserted by tests. It additionally hides the folders unvetted material lands in, which is deliberately *not* mirrored back — [why](vault-mcp/README.md#what-voice-cannot-see). |
| **Deploys** | Never restart another stack's containers. Verified 2026-08-08 that Remote Control pairing *does* survive a recreate, so this is no longer about losing the phone's session — but a deploy can still interrupt an agent run in progress. |

Commits need no coordination: the hourly backstop in `vault-sync` picks up any
subtree, and `git log -- <subtree>/` gives attribution for free.

## Operating it

[`bin/homelab`](bin/homelab) is the entry point when you are SSH'd into the NAS.
It wraps the per-stack scripts rather than replacing them — `deploy` delegates to
a stack's own `scripts/deploy.sh` where one exists, so `--sync-only` and the
re-pair warning keep working.

```sh
bin/homelab status                 # both stacks: containers, images, snapshot and voice freshness
bin/homelab deploy vault-mcp       # pull, verify provenance, recreate
bin/homelab env check vault-mcp    # .env mode and any key left blank
bin/homelab token rotate           # new MCP_TOKEN, recreate, print the header to paste into Claude
bin/homelab url                    # the connector URL the Funnel is actually serving
bin/homelab attach                 # the vault-claude tmux session

bin/homelab update                 # git pull this checkout
bin/homelab git status             # git against this checkout
bin/homelab snapshots log --author='Claude Voice'   # the vault's own history
```

**Every command takes an explicit stack, and there is no `--all`** — the shared
contract below says never restart another stack's containers, and the cheapest
way to honour that is to make it unexpressible.

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
| `shellcheck-bin.yml` | `bin/**` | nothing — lints, asserts ASCII-only and the executable bit |

Images are public, so the NAS needs no registry credential. Builds carry
provenance attestations; `obsidian-vault/scripts/deploy.sh` verifies them when `gh` is
available.
