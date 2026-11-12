# CI

Each stack has its own path-filtered workflow, so a change to one cannot
rebuild or republish another.

| Workflow | Triggers on | Publishes |
|---|---|---|
| `build-obsidian-vault.yml` | `obsidian-vault/**` (excluding markdown) | `ghcr.io/<owner>/homelab/obsidian-vault` |
| `build-vault-mcp.yml` | `vault-mcp/**` (excluding markdown) | `ghcr.io/<owner>/homelab/vault-mcp` |
| `build-gmail-mcp.yml` | `gmail-mcp/**` (excluding markdown) | `ghcr.io/<owner>/homelab/gmail-mcp` |
| `build-dashboard.yml` | `dashboard/**` (excluding markdown) | `ghcr.io/<owner>/homelab/dashboard` |
| `build-monitoring.yml` | `monitoring/**` (excluding markdown) | `ghcr.io/<owner>/homelab/monitoring` (the weekly reporter; Beszel's images are upstream, pinned by digest) |
| `e2e-monitoring.yml` | `monitoring/**` (excluding markdown), `bin/homelab` | nothing — runs `homelab setup monitoring` against the pinned images and asserts the trust-boundary tables |
| `shellcheck-bin.yml` | `bin/**` | nothing — lints, asserts ASCII-only and the executable bit |
| `check-mac.yml` | `mac/**` | nothing — evaluates the Mac Studio's nix-darwin config against `mac/flake.lock`, checks the lock is current, lints `mac/apply`. It cannot build the config, which needs a Mac |

Images are public, so the NAS needs no registry credential. Builds carry
provenance attestations; `obsidian-vault/scripts/deploy.sh` verifies them when `gh` is
available.
