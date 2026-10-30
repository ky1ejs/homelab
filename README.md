# homelab

Everything running on my QNAP NAS, in one repo: a couple of always-on Claude
Code agents I drive from my phone, two MCP servers that put my notes and my
email in front of Claude, a deploy dashboard, and the monitoring that tells me
when any of it falls over.

One repo so there is a single place that says what each stack is and how to
operate it. **Separate images and separate compose stacks** so they deploy
independently — a change to one cannot rebuild or restart another.

## What's in here

| Stack | What it is |
|---|---|
| [`obsidian-vault/`](obsidian-vault/) | Two always-on Claude Code sessions drivable from an iPhone: one rooted in the Obsidian vault with no network access, one with web search and fetch and no vault. Git version history + verified off-site backups. |
| [`vault-mcp/`](vault-mcp/) | The vault as a remote MCP server, so Claude **voice mode** can search and capture hands-free. Reachable from the internet, over a Funnel, behind OAuth 2.1. Its binary is also the vault agent's move tool, run locally over stdio. |
| [`gmail-mcp/`](gmail-mcp/) | One Gmail account as a claude.ai connector that can **send from a verified Send mail as alias**, which the hosted Gmail connector cannot. Reachable from the internet, over a Funnel, on the same WorkOS tenant as `vault-mcp`. |
| [`dashboard/`](dashboard/) | One page saying what is running, what is out of date, and the buttons to deploy and restart it. Tailnet only. The one stack that can act through the Docker socket. |
| [`monitoring/`](monitoring/) | How the host is doing: a Beszel dashboard with history, threshold alerts by email, a weekly report saying whether to upgrade or change approach, and an off-box dead-man's switch. Tailnet only. Reads the Docker socket through a four-route, GET-only proxy. |
| `fishing/` | Fishing/weather data collection, writing derived notes into the vault. No directory here yet. |

**This table does not say what is deployed.** It says what the repo contains.
Whether a stack is up, and how far it has been verified, is a property of the
host, not of this file: [`dashboard/`](dashboard/) and `docker ps` are the
ground truth, and each stack's own README records how far it has been taken.

**Not every container on this host is in this repo.** `home-assistant`, `esphome`
and `matter-server` also run under Container Station and are managed outside it.
They share nothing with the stacks above — no vault mount, no tailnet identity —
so the shared contract does not reach them. That is the other half of why
`docker ps` is the ground truth, and why [`dashboard/`](dashboard/) lists those
containers too, read-only and with no buttons.

## Running it

The host is a QNAP (Celeron J4125, 16 GB, Container Station) on its own
Tailscale node. SSH in over the tailnet name, then drive everything through one
CLI:

```sh
ssh admin@kyles-nas.tail3df177.ts.net
```

```sh
bin/homelab status            # every stack: containers, images, snapshot freshness
bin/homelab deploy vault-mcp  # pull, verify provenance, recreate
bin/homelab attach            # the vault-claude tmux session
```

[`docs/operating.md`](docs/operating.md) has the full command set, and
[`docs/host.md`](docs/host.md) has the host itself — including the QNAP traps
that cost an afternoon each the first time round.

## Docs

| Where | What |
|---|---|
| [`docs/host.md`](docs/host.md) | The NAS: how to reach it, the Tailscale setup, the QPKG and `PATH` traps |
| [`docs/shared-contract.md`](docs/shared-contract.md) | The rules every stack honours. Read before writing anything that touches the vault |
| [`docs/operating.md`](docs/operating.md) | `bin/homelab`, and getting the repo onto a NAS with no `git` |
| [`docs/ci.md`](docs/ci.md) | The per-stack workflows and what each publishes |
| [`AGENTS.md`](AGENTS.md) | For agents working in this repo: the rule that a change carries its own documentation |

Each stack has its own README. The vault stack splits three ways:
[`ARCHITECTURE.md`](obsidian-vault/ARCHITECTURE.md) is what it is,
[`DECISIONS.md`](obsidian-vault/DECISIONS.md) is why — including the
alternatives that were rejected and the reasoning — and
[`README.md`](obsidian-vault/README.md) is how to operate it.

## A note on the prose

The documents here are longer than a homelab repo normally warrants, and that
is deliberate. The failure this repo is built against is a restore at 2am
against a runbook that no longer matches the scripts, so the *why* is recorded
alongside the *what*, rejected options included. If a document contradicts the
code, the document is the bug — see [`AGENTS.md`](AGENTS.md).
