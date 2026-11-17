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
| `check-mac.yml` | `mac/**`, `monitoring/docker-compose.yml`, `.github/workflows/check-mac.yml` | nothing — checks `mac/flake.lock` matches `mac/flake.nix`, evaluates the Mac Studio's nix-darwin config against that lock, lints `mac/apply`, `mac/bootstrap`, `mac/lock-screen` and the scripts in `mac/claude` and `mac/ci`, runs `mac/usage`'s unit tests and `mac/cli/test-homelab`, asserts ASCII-only and the executable bit, checks `mac/beszel`'s agent version matches the hub's in `monitoring/docker-compose.yml`, and evaluates and lints again with a made-up hub key so `mac/beszel`'s daemon is checked too. It cannot build the config, which needs a Mac |

Images are public, so the NAS needs no registry credential. Builds carry
provenance attestations; `obsidian-vault/scripts/deploy.sh` verifies them when `gh` is
available.
