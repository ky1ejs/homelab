# homelab

Container stacks running on the QNAP NAS. One repo so there's a single place
that says what's deployed; **separate images and separate compose stacks** so
they deploy independently.

## Stacks

| Stack | What it is | Status |
|---|---|---|
| [`vault/`](vault/) | Always-on Claude Code session rooted in the Obsidian vault, drivable from an iPhone. Git version history + verified off-site backups. | Built, not yet deployed |
| `fishing/` | Fishing/weather data collection, writing derived notes into the vault. | In progress, separate session |

Design, rationale, costs and risk assessment for the vault stack live in
[`vault/INITIAL_PLAN.md`](vault/INITIAL_PLAN.md). Operating instructions are in
[`vault/README.md`](vault/README.md).

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
| **Writes** | **Atomic temp-then-rename, always.** Write to a temp file on the same filesystem, then `rename()`. Claude Code's own writes are not atomic and this is a documented corruption source — see `vault/INITIAL_PLAN.md` §8. |
| **Raw caches** | Stay **outside** `/vault`. Git keeps every version of every blob permanently, and those bundles go to Google Drive. Only derived notes belong in the vault. |
| **Deploys** | Never restart another stack's containers. `vault-claude` holds a live tmux session paired to a phone; restarting it drops that session silently. |

Commits need no coordination: the hourly backstop in `vault-sync` picks up any
subtree, and `git log -- <subtree>/` gives attribution for free.

## CI

Each stack has its own path-filtered workflow, so a change to one cannot
rebuild or republish another.

| Workflow | Triggers on | Publishes |
|---|---|---|
| `build-vault.yml` | `vault/**` (excluding markdown) | `ghcr.io/<owner>/homelab/vault` |

Images are public, so the NAS needs no registry credential. Builds carry
provenance attestations; `vault/scripts/deploy.sh` verifies them when `gh` is
available.
