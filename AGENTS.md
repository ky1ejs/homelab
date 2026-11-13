# AGENTS.md

Instructions for agents working in this repository.

## Every code change must carry its documentation with it

Before finishing any change to code or configuration, **check what documentation
that change has just made wrong** and update it in the same commit. Stale docs
here are not cosmetic: this repo's prose is load-bearing operational knowledge,
and a runbook that no longer matches the scripts is discovered during a restore.

Treat this as part of the change, not a follow-up. A change is not complete while
a document still describes the old behaviour.

### Look in both places

**Markdown files** — the obvious half:

| File | Carries |
|---|---|
| `README.md` (root) | The landing page: what the homelab is, what each stack is for, and where every other document lives. Deliberately short, and deliberately silent on deploy state — see DECISIONS.md. Operational detail belongs in `docs/`, not here |
| `docs/host.md` | The NAS itself: Tailscale, the QPKG upgrade chore, the `PATH` and volume traps. Every line was checked against the host — correct it only against the host |
| `docs/shared-contract.md` | The rules every stack honours. Cited by name from `vault-mcp/vault.go`, `vault-mcp/stamp.go`, `obsidian-vault/scripts/hook-stamp.sh` and `bin/homelab`, so a row and its implementations change together |
| `docs/operating.md` | `bin/homelab`'s command set, and the initial clone onto a NAS with no host `git` |
| `docs/mac-studio.md` | The Mac Studio: what `mac/` sets and leaves out, the day-one runbook, its network and FileVault. Correct it only against the host, the same rule as `docs/host.md` |
| `docs/unifi.md` | The UniFi gateway: networks, zones, hand-made policies, and the read-first loop for changing them through the API. Correct it only against the gateway. Its policy table and `docs/mac-studio.md`'s The network section describe the same rules, so a policy change is a change to both |
| `dotfiles/profile.sh` | Login-shell setup that `homelab install` sources from the NAS profile: `PATH`, `hl`, `homelab cd`. POSIX sh, for busybox `ash` |
| `docs/ci.md` | The per-stack workflows and what each publishes. A new workflow or a new published image belongs in its table |
| `obsidian-vault/README.md` | Operating runbook — numbered setup steps, restore procedure, service table |
| `obsidian-vault/ARCHITECTURE.md` | Mermaid diagrams, component tables, data flow |
| `obsidian-vault/DECISIONS.md` | *Why* each choice was made, including rejected alternatives |
| `obsidian-vault/.env.example` | Every tunable, with the reasoning for its default |
| `obsidian-vault/research-CLAUDE.md` | The research agent's standing instructions, installed into the scratch volume |
| `vault-mcp/README.md` | MCP server behaviour, auth flow, tool surface |
| `gmail-mcp/README.md` | Gmail connector: tools, the send-as rule, auth, the `-login` flow |
| `dashboard/README.md` | Dashboard design, its trust boundary, and what it deliberately cannot do |
| `monitoring/README.md` | Monitoring stack: the setup steps, the report's thresholds table, the socket proxy's verified allowlist |
| `monitoring/.env.example` | Every monitoring tunable. Note that its compose file names each variable per service, so a new one does nothing until a service names it |

**Comments in code** — the half that gets missed. In this repo, script and Go
header comments carry the rationale, not just the mechanics:

- `obsidian-vault/scripts/*.sh` — header blocks explain why the approach was
  chosen over the alternative, and inline comments guard specific traps.
  `scratch-sweep.sh` is the only script here that deletes anything, so its
  guards are documented individually rather than as a group
- `obsidian-vault/vault-{claude,research}-settings.json` — the `_comment` array
  at the top of each is the real documentation for that agent's tool policy, and
  the two are deliberately asymmetric: one denies the web and reads the vault,
  the other has the web and cannot see the vault. A change that makes them look
  more alike is almost certainly wrong in one of them
- `vault-mcp/*.go`, `gmail-mcp/*.go`, `dashboard/*.go` — same convention. In `dashboard/` the
  tests are part of the documentation too: `agent_test.go` states, in executable
  form, exactly what the socket-holding process will refuse
- `monitoring/report.go` — the threshold constants are the report's judgement,
  and `monitoring/README.md` repeats them in a table. Change one, change both.
  `monitoring/alerts.json` must match their ACT lines too, and
  `alerts_test.go` enforces that one. The email's wording lives in
  `evaluate()` there and its layout in `render.go` and `report.html`; both
  are pinned by the sample emails in `monitoring/testdata/`, which
  `go test -run TestSamples -update` rewrites. `monitoring/beszel.go`'s struct tags and
  `monitoring/setup.go`'s endpoints are Beszel's internals at the pinned
  version, so a Beszel upgrade in `monitoring/docker-compose.yml` is a change
  to both
- `monitoring/docker-compose.yml` — the socket proxy's `-allowGET` regex is the
  security boundary for the third-party agent, and the verified table in
  `monitoring/README.md` is its evidence. `monitoring/scripts/e2e.sh` asserts
  every row of that table in CI, so a change to the regex or to the table is
  a change to the script too
- `mac/hosts/mac-studio.nix` — its header lists what the Mac's config deliberately
  leaves out, and the "Left out, on purpose" table in `docs/mac-studio.md` says the
  same with reasons. Setting one of those (FileVault, auto-login, an Apple
  Account) is a decision recorded in both, not a one-line addition. GUI apps
  are Homebrew casks, never `environment.systemPackages` entries: nix-darwin
  puts those in `/Applications/Nix Apps`, and its check of that folder then
  stops every apply over SSH
- `mac/apply` — like `bin/homelab`, its usage block is printed with
  `sed -n '3,6p'`, so a new argument means moving that range too
- `mac/bootstrap` — day one fetches it from `main` by URL, so a merge
  changes what a fresh Mac runs. It runs under macOS's `/bin/bash` 3.2,
  before Nix or Homebrew exist, and every part must be safe to repeat when
  already done, because the runbook says to run it again after a failure
- `mac/wallpaper/mac-studio-dark.png` — generated by the scripts beside it
  from kylejs-site's tokens, marks and fonts. Change it by changing them and
  remaking it, as its README says, not by editing the image
- `docs/mac-studio.md` — the tailnet policy snippet carries `tests` and
  `sshTests`. A change to its grants or `ssh` rule changes those too
- `mac/` holds no dotfiles. The Studio links kylejs-macOS-bootstrap's, the
  same files the laptop uses, so a shell or git change belongs in that repo.
  A copy here would drift from the laptop's
- `bin/homelab` — the usage block at the top is parsed by `usage()` with
  `sed -n '3,25p'`, so adding a command means editing that block AND moving
  the range, which is easy to forget and silently truncates `--help`
- `obsidian-vault/docker-compose.yml`, `Dockerfile` — comments justify mounts,
  pins, and the absence of published ports
- `dashboard/docker-compose.yml` — justifies the two mounts that are exceptions
  to this repo's usual posture: the Docker socket, and the checkout bound at its
  own host path

### What to check for

1. **Contradiction.** Does any document now state something the code no longer
   does? Diagrams and tables date fastest — they encode counts, names and
   retention numbers that a one-line change can falsify.
2. **Invalidated rationale.** A comment saying "X because Y" is wrong when Y
   stops being true, even if X is still what the code does. This is the most
   damaging kind of stale doc, because it reads as verified reasoning. If a
   change undercuts a recorded decision, correct the record rather than leaving
   two claims standing.
3. **Defaults.** Changing a default means updating `.env.example` *and* any
   prose quoting the old value.
4. **New tunables.** A new environment variable is undocumented until it appears
   in `.env.example` with its reasoning, not just its name.
5. **Reversals.** When a change undoes an earlier decision, say so in
   `DECISIONS.md` with the date and what changed the answer. Do not silently
   delete the old rationale — the rejected alternatives are the point.

### Scope

Update what the change actually touched. This is not licence for a documentation
sweep: unrelated staleness found along the way should be mentioned, not fixed
silently inside someone else's change.
