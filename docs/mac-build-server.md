# Mac build server (plan)

A Mac Studio joins the homelab to do three jobs the QNAP can't: run Cut's iOS
CI, host Claude Remote Control sessions that build and test iOS, and run local
models. This page is the plan. **Nothing in it is built yet**, and no Mac has
been bought. When a phase is done, its steps move into a runbook in a `mac/`
directory and this page shrinks to the decisions.

The decision itself (why this Mac, what it costs against renting, what could go
wrong) is in the vault: *DR 2026-11-10 Mac build server for homelab*.

| | |
|---|---|
| Hardware | Mac Studio, M5 Max with 40-core GPU, 64 GB, 1 TB, 10Gb Ethernet. $3,800 |
| Power | 7 W idle, 200 W max (Apple's figures) |
| OS | macOS 26 Tahoe, managed by nix-darwin from this repo |
| Access | Tailscale, its own tailnet node. SSH by key through the 1Password agent, as on the NAS |
| Jobs | Two ephemeral CI VMs (Tart), Claude Remote Control, Linux containers (Colima), local models |

## Why it is shaped like this

- **Two VMs is the ceiling, whatever the spec.** Apple's macOS licence allows two
  macOS VMs per Mac. The Studio buys faster jobs and room for local models, not
  more parallel jobs.
- **Every CI job gets a fresh VM.** Tart clones a VM for each job and deletes it
  after, so a job can't leave state, fill the disk, or see the last job's
  secrets. Tartelet runs one GitHub runner per VM, two at most.
- **CI must never stall on the Mac.** The Mac shares the flat's power and
  internet. If it is down, jobs fall back to GitHub's runners (see
  [Fallback](#fallback-to-githubs-runners)), and I get an email (see
  [Offline alerts](#offline-alerts)).
- **The host is code.** nix-darwin declares packages, launchd agents and system
  settings, so a reinstall is "install Nix, clone, switch". Xcode is the
  exception: it is pinned by version with `xcodes`, because nixpkgs can't ship it.

## Memory budget

64 GB, on a busy day (my estimates, not measurements):

| Load | GB |
|---|---|
| macOS host | 4 |
| Two CI VMs at 8 GB (GitHub's hosted runner has 7) | 16 |
| Two Claude sessions building Cut, with a simulator each | 14 |
| Colima | 4 |
| **Busy day total** | **38** |
| Left for a local model | ~26 |

26 GB fits the models people actually run on 64 GB Macs: Qwen 3.6 27B dense
(about 17 GB at 4-bit) or Qwen 3.6 35B MoE (about 21 GB). A worst day (bigger
VMs, three builds) is about 53 GB, which leaves room only for a small model.

## Layout in this repo

```
mac/
  flake.nix              # nix-darwin flake, one host: studio
  hosts/studio.nix       # hostname, users, which modules
  modules/
    base.nix             # power, SSH, Tailscale, auto-login, software update policy
    ci.nix               # Tart, Tartelet, the VM image name, the heartbeat agent
    claude.nix           # Remote Control as a launchd agent
    containers.nix       # Colima as a launchd agent
    models.nix           # the local model server (phase 5)
  scripts/
    heartbeat.sh         # the checks behind the offline alerts
  README.md              # the runbook, once phase 1 is done
```

Apply a change on the Mac with `darwin-rebuild switch --flake ~/homelab/mac#studio`.
`bin/homelab` is written for the NAS's busybox shell and stays the NAS's tool.
The Mac gets its own short `mac/bin/apply` that pulls and switches, so the two
hosts don't share a script that has to work on both.

## Phases

Apple takes custom-configuration Macs back within 14 days of delivery. Phases 1
and 2 are sized to fit inside that window, because they answer whether it was
the right Mac.

### 1. Base host

1. Set up macOS with one admin user. Plug in wired Ethernet.
2. Install Tailscale and add the Mac to the tailnet. Add my SSH key.
3. Install Nix, then clone this repo and switch to the flake.
4. `base.nix` sets: never sleep, restart after a power failure, SSH on, and
   automatic macOS updates **off** (an update that bumps the Xcode toolchain
   mid-week breaks CI). Updates become a monthly chore.
5. Turn on auto-login for the admin user. Simulators and Tartelet both need a
   logged-in GUI session.

**FileVault and auto-login don't mix.** macOS turns auto-login off when FileVault
is on. Tahoe can unlock FileVault over SSH at boot, but I haven't confirmed that
this also starts the GUI session that simulators need. Test it in the return
window. If it doesn't, the choice is FileVault plus a manual unlock after every
power cut, or no FileVault on a box that holds signing certificates. My lean is
FileVault on, with the offline alert telling me when it needs unlocking.

Done when: the Mac survives a pulled power cable and comes back on the tailnet
without anyone touching it (or, with FileVault, after one SSH unlock).

### 2. iOS CI for Cut

1. Install Tart and Tartelet through `ci.nix`.
2. Build the CI VM image with Xcode 27.1 at `/Applications/Xcode_27.1.app`, the
   path Cut's workflows set as `XCODE_PATH`. Start from a Cirrus Labs macOS
   image and add Xcode with `xcodes`.
3. Create an org runner group in cut-watch that only `cut-watch/cut` can use.
   Register Tartelet against it, with the label `cut-mac`. Cut is private, so
   no outside fork PRs reach these runners.
4. Run the VMs on Tart's softnet networking, so they reach the internet but not
   each other, the host, or the LAN. **Check this on day one** by having a job
   try to reach the NAS. If the router supports VLANs, the Mac also goes on its
   own.
5. Add the fallback and the CI alert (below), then switch Cut's iOS jobs over.

Tartelet is set up in its app's settings window, not in a file. Setting it through
nix-darwin's `system.defaults.CustomUserPreferences` should work but is
untested. If it doesn't, the alternative is a short launchd script that clones
a VM, asks GitHub for a just-in-time runner config, runs one job and deletes the
VM. That is more code to own, but all of it is in this repo.

Done when: a Cut PR runs build and tests on the Mac, a merge deploys to
TestFlight from it, and both are faster than on `xcode-27`.

### 3. Claude Remote Control

`claude.nix` runs `claude remote-control --spawn worktree --capacity 2` as a
launchd agent for the admin user, with `KeepAlive`. Remote Control exits after
about 10 minutes without internet, so launchd restarting it is what makes it
survive an outage. It signs in once with my claude.ai account.

Capacity 2 matches the memory budget. A third session is possible on a quiet
day, but not alongside two CI jobs.

Done when: from my phone, a session can build Cut and run its tests on a
simulator.

### 4. Linux containers

`containers.nix` runs Colima as a launchd agent. Colima runs without a GUI and
speaks the Docker API, so the compose files in this repo work unchanged.
Apple's `container` 1.0 has no Compose support yet, so it isn't used.

Nothing moves off the NAS in this phase. Which stacks would gain from moving is
a separate decision, after a few weeks of both running.

### 5. Local models

`models.nix` runs a model server bound to the tailnet address only, starting
with Qwen 3.6 27B dense at 4-bit. Inside the return window, measure its replies
per second. The Studio was chosen partly for twice the mini's memory bandwidth,
which should show as about twice the speed on dense models.

The server and its port are open until phase 5 starts. Ollama and an MLX server
are the candidates.

## Fallback to GitHub's runners

A job sent to a self-hosted label waits in the queue until a runner with that
label appears, for up to a day. Nothing falls back on its own. So Cut's iOS
workflows choose their runner before they start:

1. A `pick-runner` job runs first on `ubuntu-latest`. It asks Healthchecks.io
   for the status of the **Mac CI** check (see below), with a read-only API key
   stored as a Cut secret.
2. If the check is `up`, the iOS jobs run on `[self-hosted, macOS, cut-mac]`.
   For any other answer (`grace`, `down`, `paused`, or no answer), they run on
   `xcode-27`.
3. When on the fallback, PR iOS jobs go back to running only on labelled PRs,
   as they do today. Main-branch builds and deploys still run. So an outage
   costs at most what CI costs now.
4. A repository variable `IOS_RUNNER` overrides the choice: `mac`, `github`, or
   unset for automatic. That handles planned maintenance.

**Why the heartbeat, and not GitHub's runner list.** Listing a cut-watch org's
self-hosted runners needs a token with the org's self-hosted runners
permission, which I'd rather not put in Cut's secrets. Ephemeral runners also disappear from that list
between jobs, so "no runner online" doesn't reliably mean "Mac down". The
heartbeat is one signal that drives both the fallback and the alert.

**Known gap:** the heartbeat takes up to 15 minutes to go `down`. A job routed
to the Mac in that window waits until the Mac comes back. Re-running it after
the switch moves it to GitHub.

These are changes to Cut's workflows (`pr-tests.yml`, `main.yml`, and the
reusable `ios-build`, `ios-test` and `ios-deploy` workflows), made in a Cut PR once phase 2's runners exist.
Nothing in Cut changes before then.

## Offline alerts

The monitoring stack's dead-man's switch already uses Healthchecks.io, so the
Mac uses it too. Every 5
minutes a launchd agent runs `mac/scripts/heartbeat.sh`, which pings two checks:

| Check | Pings success when | Period | Grace | Also drives |
|---|---|---|---|---|
| Mac CI | Tartelet is running, at least one runner VM is up, and the disk has more than 50 GB free | 5 min | 10 min | the CI fallback |
| Mac Claude | the Remote Control process is running | 5 min | 30 min | nothing |

When a check fails, the script pings the check's `/fail` URL with a line saying
which test failed, so the email says what is wrong. When the Mac is off, has no
internet, or is waiting for a FileVault unlock, the pings stop and
Healthchecks.io emails after the grace period, from the same account as the
NAS's checks.

Mac Claude has a longer grace because Remote Control restarts itself after an
outage, and that shouldn't page me.

A Beszel agent on the Mac, reporting to the NAS's hub for CPU, memory and disk
history, is worth adding later. It isn't in this plan, because
`monitoring/alerts.json` is tuned to the NAS's J4125 and the hub applies it to
every system. The Mac would need its own alert list first.

## Risks this plan accepts

- **A power cut takes CI and Claude down together.** Fallback covers CI. A small
  UPS covers short cuts. It is not in this plan, but worth buying with the Mac.
- **Upkeep is mine.** macOS, Xcode and the VM image need updating by hand, about
  monthly, and Xcode betas need a second image.
- **Tart's long-term upkeep.** Its maker joined OpenAI in April 2026 and relicensed
  it permissively. If it stalls, the image and runner steps are the parts to
  replace.
- **Moving house in late 2027** means a stretch of downtime, which the fallback
  covers.
