# homelab

Container stacks running on the QNAP NAS. One repo so there's a single place
that says what's deployed; **separate images and separate compose stacks** so
they deploy independently.

## Stacks

| Stack | What it is | Status |
|---|---|---|
| [`obsidian-vault/`](obsidian-vault/) | Always-on Claude Code session rooted in the Obsidian vault, drivable from an iPhone. Git version history + verified off-site backups. | **Running**, restore-tested |
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

No stack publishes ports. Nothing needs configuration on the UniFi gateway.

## Shared contract

Anything that writes into the vault must honour these. They are not stylistic.

| Item | Rule |
|---|---|
| **Sync** | `vault-sync` owns it. **No other container runs `ob sync`.** Two headless Obsidian clients on one vault means two sync engines fighting over the same local state, plus a second device against the Sync subscription. |
| **UID/GID** | Identical `APP_UID:APP_GID` in every stack. A mismatch produces a confusing symptom: writes appear to work, the Mac sees unreadable files. |
| **Writes** | **Atomic temp-then-rename, always.** Write to a temp file on the same filesystem, then `rename()`. Claude Code's own writes are not atomic and this is a documented corruption source — see [`ARCHITECTURE.md`](obsidian-vault/ARCHITECTURE.md#known-unresolved-risk). |
| **Raw caches** | Stay **outside** `/vault`. Git keeps every version of every blob permanently, and those bundles go to Google Drive. Only derived notes belong in the vault. |
| **Deploys** | Never restart another stack's containers. Verified 2026-08-08 that Remote Control pairing *does* survive a recreate, so this is no longer about losing the phone's session — but a deploy can still interrupt an agent run in progress. |

Commits need no coordination: the hourly backstop in `vault-sync` picks up any
subtree, and `git log -- <subtree>/` gives attribution for free.

## CI

Each stack has its own path-filtered workflow, so a change to one cannot
rebuild or republish another.

| Workflow | Triggers on | Publishes |
|---|---|---|
| `build-obsidian-vault.yml` | `obsidian-vault/**` (excluding markdown) | `ghcr.io/<owner>/homelab/obsidian-vault` |

Images are public, so the NAS needs no registry credential. Builds carry
provenance attestations; `obsidian-vault/scripts/deploy.sh` verifies them when `gh` is
available.
