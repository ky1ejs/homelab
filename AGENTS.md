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
| `docs/host.md` | The NAS itself: Tailscale, the QPKG upgrade chore, the `PATH`, login-profile and volume traps. Every line was checked against the host — correct it only against the host |
| `docs/shared-contract.md` | The rules every stack honours. Cited by name from `vault-mcp/vault.go`, `vault-mcp/stamp.go`, `obsidian-vault/scripts/hook-stamp.sh` and `bin/homelab`, so a row and its implementations change together |
| `docs/operating.md` | `bin/homelab`'s command set, `homelab autorun` and what it leaves untested, and the initial clone onto a NAS with no host `git` |
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
- `monitoring/hosts.go` — each machine's threshold constants and advice are
  the report's judgement, and `monitoring/README.md` repeats them in tables
  (the NAS's, then the mac studio's). Change one, change both.
  `monitoring/alerts.json` must match their ACT lines too, per host, and
  `alerts_test.go` enforces that one. The email's wording lives in
  `evaluate()` in `report.go`, the per-host sentences in `hosts.go`, and its
  layout in `render.go` and `report.html`; all
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
  `sed -n '3,9p'`, so a new argument means moving that range too. The
  activation script runs with an empty environment, so apply passes it
  settings through a root-only file in `/var/run`, not variables. It
  reads `--pull` in a first pass, pulls, and runs the pulled copy of itself
  with the other arguments before it checks them, so only that copy's parser
  needs to know a new one; `mac/test-apply` checks this. `hl apply`
  (`mac/cli/homelab`) adds `--pull` unless given `--no-pull`
- `mac/bootstrap` — day one fetches it from `main` by URL, so a merge
  changes what a fresh Mac runs. It runs under macOS's `/bin/bash` 3.2,
  before Nix or Homebrew exist, and every part must be safe to repeat when
  already done, because the runbook says to run it again after a failure
- `mac/claude/` — the `worker` user's Remote Control servers, heartbeat and
  sign-in scripts. nix-darwin installs launchd agents only for kylejs, so its
  activation script installs worker's itself. Its scripts run under macOS's
  `/bin/bash` 3.2, and a new one goes in `check-mac.yml`'s shellcheck and
  executable-bit lists. Servers are only for repos with an iOS app. No Bash
  sandbox: it blocks CoreSimulator, which Cut needs. No worker GitHub token
  with write access may reach this repo or kylejs-macOS-bootstrap, which the
  Studio and the NAS deploy as root. How `github-token` picks a repo's token
  is described in its header, `docs/mac-studio.md` and `worker-CLAUDE.md`'s
  "GitHub"; a change to one is a change to all three. CI runs
  `mac/claude/test-github-token`, and `mac/claude/test-worker-tmux`, which
  types mouse events at worker-shell's tmux settings
- `mac/ci/` — Cut's iOS CI: worker's runner slots, the VM image script and
  the CI heartbeat. Its agents go in `homelab.worker.agents`, which
  `mac/claude`'s activation script installs. A slot's restart drains it
  first through `homelab.worker.drain` (`mac/ci/drain`): a change to the
  runner must keep writing its phase file, or applies wait out their
  timeout. The slot count stays at 2 or
  below: Apple's licence allows two macOS VMs at once. A VM reaches only the
  internet, through Softnet's default; never add `--net-softnet-allow` for a
  private range, which would let a pull request's code reach the LAN or the
  tailnet. Its scripts go in `check-mac.yml`'s two lists, like `mac/claude`'s
- `mac/cli/` — `homelab` (`hl`) on the mac studio. It wraps the other
  modules' commands. `default.nix` fills its user names, paths, CI slots and
  drain minutes from the config. It copies a few facts by hand, each marked
  at its source: the 50 GB the runners stop at (`mac/ci`'s `minFreeGB`), the
  heartbeat's 150 GB and 8 GB swap (`mac/claude/heartbeat`), the heartbeats'
  5-minute interval, and the root daemons and their `/var/log` names
  (`status_daemons()`, `ROOT_LOGS`). A change to one is a change here. A new command
  goes in its `usage()` heredoc, its `main()` and the command table in
  `docs/mac-studio.md`. `hl status` parses the files `mac/ci/runner`,
  `mac/ci/drain` and both heartbeats write, so a change to one of those
  formats is a change to `status_ci()` or `status_heartbeats()` and to
  `test-homelab`'s fixtures, which copy them. Runs under macOS's
  `/bin/bash` 3.2, ASCII only
- `mac/usage/` — the usage collector and cost report. Python standard
  library only, because nothing installs packages for it. Its prices and the
  purchase are constants at the top of `mac-usage`, each with its source and
  the date it was read; update both together. It never stores command lines
  or transcript text, so a change that reads more needs a reason in its
  header. Run its tests with `python3 -m unittest discover -s mac/usage`
- `mac/diagnose/` — `mac-diagnose`, the root daemon that mails what takes
  the disk to the owner's inbox, and `ROUTINE.md`, the standing instructions
  for the scheduled agent that turns each report into a PR here. Python
  standard library only, like `mac/usage`. Its categories and ROUTINE.md's
  table are one list, and its tests fail when a category has no row. It
  names only folders whose names are not session or branch names, so a new
  category that lists children needs that check. Its `LOW_FREE_GB` and
  `CRITICAL_FREE_GB` copy the heartbeat's lines. The SMTP host is a constant,
  and the mail settings stay in a root-only file outside this repo. Its
  daemon is in `mac/cli`'s daemon list. Run its tests with
  `python3 -m unittest discover -s mac/diagnose`
- `mac/app-store/` — `app-store`, which signs and ships the sessions' apps
  with an App Store Connect key in worker's home. The key and its IDs never
  go in this repo, which is public; the script holds only their path. Its
  usage block, `docs/mac-studio.md`'s command table and
  `mac/claude/worker-CLAUDE.md`'s "Shipping apps" describe the same commands,
  so a new one changes all three. The key's folder is set in
  `mac/app-store/default.nix` and named in `mac/claude/default.nix`'s
  autoMode, so a move changes both. The API host and the paths of curl,
  xcodebuild and plutil are fixed lines in the script, never read from the
  environment or PATH, so no caller can redirect the token; the test
  rewrites exactly those lines and fails if one changes. Its script runs
  under macOS's `/bin/bash` 3.2 and goes in `check-mac.yml`'s two lists. CI
  runs `mac/app-store/test-app-store`
- `mac/builds/` — the ad hoc builds server: `builds`, its launchd agents,
  and a Compose project of a read-only nginx and a Tailscale sidecar,
  `tag:builds`, that serves `app-store adhoc`'s builds to the tailnet with
  `tailscale serve`, never Funnel. `settings.nix` holds the folder, the
  node's name, the tailnet and the days a build is kept, and both
  `mac/builds/default.nix` and `mac/app-store/default.nix` read it, so the
  link `app-store adhoc` prints and the name the sidecar logs in as change
  together. The auth key never goes in this repo or on disk: `builds login`
  takes it by hand. A change that publishes a port, adds Funnel or gives
  `tag:builds` a grant is a change to the trust boundary in
  `docs/mac-studio.md`'s "Ad hoc builds" and to the tailnet policy's tests.
  Its sessions' rules are in `mac/claude/worker-CLAUDE.md`'s "Shipping apps"
  and `mac/claude/default.nix`'s autoMode. Its scripts run under macOS's
  `/bin/bash` 3.2 and go in `check-mac.yml`'s two lists. CI runs
  `mac/builds/test-builds`, under bash 3.2 too
- `mac/beszel/` — the Beszel agent the NAS's monitoring hub pulls from.
  `release.nix` pins it, and `check-mac.yml` fails until that version matches
  the hub's tag in `monitoring/docker-compose.yml`, so bump them together.
  It listens on the LAN address alone and runs as `nobody`; a change that
  widens either is a change to the trust boundary in `monitoring/README.md`
  too. `mac/keys/beszel-hub.pub` holds the hub's public key, and nothing is
  installed while it holds none
- `mac/simslim/` — simslim, which slims worker's simulators, its watch
  agent and `sim-full`. `release.nix` pins the release by hand. `sim-full`
  reads the agent's events file and gets its wait from the agent's scan
  interval, both set in `default.nix`, so a change to either is a change to
  the script. The
  categories the agent leaves on, the services a slim simulator loses and the
  `sim-full` usage are in `mac/claude/worker-CLAUDE.md` for the sessions and
  in `docs/mac-studio.md`'s "Slim simulators"; a change to one is a change
  to both. Its scripts run under macOS's `/bin/bash` 3.2 and go in
  `check-mac.yml`'s two lists. CI runs `mac/simslim/test-sim-full`
- `mac/wallpaper/mac-studio-dark.png` — generated by the scripts beside it
  from kylejs-site's tokens, marks and fonts. Change it by changing them and
  remaking it, as its README says, not by editing the image
- `docs/mac-studio.md` — the tailnet policy snippet carries `tests` and
  `sshTests`. A change to its grants or `ssh` rule changes those too
- `mac/` holds no dotfiles. The Studio links kylejs-macOS-bootstrap's, the
  same files the laptop uses, so a shell or git change belongs in that repo.
  A copy here would drift from the laptop's
- `bin/homelab` — the usage block at the top is parsed by `usage()` with
  `sed -n '3,26p'`, so adding a command means editing that block AND moving
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
