# Decisions

Why the system is shaped the way it is, what was rejected, and the traps found
building it. Read this before re-opening a settled question.

- **What it is** — [`ARCHITECTURE.md`](ARCHITECTURE.md)
- **Operating it** — [`README.md`](README.md)

Assessment 2026-08-05. Built and verified 2026-08-08. This file supersedes
`INITIAL_PLAN.md`, which is in git history if the original planning form is ever
wanted.

---

## The constraint that drove everything

| Constraint | Consequence |
|---|---|
| iOS cannot run Claude Code — no arbitrary long-lived processes | The phone is a **control surface**, never a host |
| No spare Mac; a laptop that sleeps is not always-on | Rules out the MacBook as a permanent host |
| Claude Desktop and the Claude app route remote MCP traffic through Anthropic's cloud | Any MCP server they consume must be **publicly reachable**. Claude Code connects directly and does not. |

That third row decided the architecture. Remote Control dials *out*, so it needs
no public endpoint; the MCP route would have needed one.

**Amended 2026-08-09.** A fourth constraint appeared that the assessment did not
anticipate: **voice mode cannot drive Remote Control**, and voice is the reason
to want the vault reachable while driving or walking. Voice mode *can* call
custom connectors — verified against a public authless server before any of this
was built — so the MCP route was adopted as an addition rather than a
replacement, and its public endpoint was accepted as a scoped exception rather
than a reversal. [`../vault-mcp/README.md`](../vault-mcp/README.md#trust-boundary)
is where that exception is argued.

---

## Options assessed

| Option | Verdict | Why |
|---|---|---|
| **Claude Code Remote Control, agent on NAS** | **CHOSEN** | A real Claude Code session rooted in the vault, reachable from the phone. No inbound ports, no tunnel. Uses hardware already owned. |
| Obsidian plugin as thin client + self-hosted relay | **Deferred — first alternative** | See below. Works on iOS, needs no public endpoint, exists off the shelf. Costs Claude Code semantics. |
| Remote MCP server + Claude app | **Adopted alongside, 2026-08-09** | Needs a publicly reachable HTTPS endpoint. Loses grep, file tools, `CLAUDE.md`, skills — but only *as a replacement*. Built as a **second surface** for voice mode, which cannot drive Remote Control; `vault-claude` keeps all of the above. See [`../vault-mcp/`](../vault-mcp/). |
| SSH from an iOS terminal | Viable | Simpler, no research-preview dependency. Worse ergonomics. |
| Claude Code on the web | Rejected | Requires GitHub for clone/push; unit of work is a branch + PR, wrong for notes. |
| Claude Dispatch / Cowork | Rejected | Pairs the mobile app with the **desktop** app, which must stay awake. The "no spare Mac" constraint again. |
| Local stdio Obsidian MCP servers | Rejected | Only work when the client is on the same machine. Useless for the phone. |
| Git-as-sync, replacing Obsidian Sync | Rejected | Commit discipline, poor iOS ergonomics, prose merge conflicts. Git is an undo log here, not transport. |
| Keep the MacBook awake | Rejected as permanent | Fine for a trial. Fails the moment the laptop leaves the desk. |
| Cheap VPS instead of the NAS | Closed | NAS has 16 GB / ~11.7 GB free. The real cost of a VPS is a full replica of personal notes on someone else's hardware. |

**Also set aside:** Ignis (solves *human* remote access, not agent access);
self-hosted LiveSync + CouchDB (cannot interoperate with official Sync);
SilverBullet / Basic Memory / Khoj (different substrates entirely).

### The Obsidian plugin route — the documented alternative

**Embedding Claude Code in an iOS plugin is impossible.** On desktop Obsidian is
Electron and plugins get Node APIs; **on iOS it is a WebView app** — no Node
runtime, no process spawning. Both existing plugins confirm it: `claude-code-ide`
is desktop-only and read-only; `Claudian` is desktop-only and needs a provider
CLI locally.

**The thin-client split does work**, and removes the constraint that killed MCP:
a plugin's HTTP requests originate *on the device*, so with Tailscale on the
phone it reaches the NAS directly — no public endpoint. `Vault Companion for
Claude` already implements this, with either a direct API key (per-token
billing) or a self-hosted Agent SDK relay (existing subscription).

**Cost of switching:** Claude Code semantics — Bash, skills, subagents, and the
`SessionStart`/`Stop` hooks the whole snapshot design rests on. Commit bracketing
would move into the relay.

**What survives:** snapshots, backups, compose scaffolding, CI and the secrets
boundary are all independent of what talks to the vault. Swap `vault-claude` for
`vault-relay` and move the commit calls.

**Cheapest evaluation:** install the plugin with the API-key backend. Five
minutes, no infrastructure — and it answers the ergonomics question directly.

---

## Runtime: Node, not bun or Go

The image is `node:22-bookworm-slim`. **Not a language preference** —
`@anthropic-ai/claude-code` and `obsidian-headless` are both npm packages tested
against Node, and neither has a build for another toolchain. The runtime is a
property of the dependencies.

**bun — evaluated, rejected.** It would be a substitute runtime for packages that
do not support it, in an image containing no JavaScript application for bun's
strengths to act on. Four concrete costs against zero benefit:

1. **AVX2.** The NAS is a Celeron J4125 (Goldmont Plus). Verified on the box:
   `grep -o avx2 /proc/cpuinfo` returns nothing. The standard bun x64 build dies
   with `Illegal instruction`; it needs the `-baseline` build. Node has no such
   requirement.
2. **Silent CI mis-detection.** `bun.sh/install` picks its build by probing the
   CPU it runs on. A GitHub runner *has* AVX2, so the image builds green and
   crashes on first run on the NAS. Build-time detection asks the wrong machine.
3. **`$HOME` shadowing.** `bun install -g` targets `~/.bun/bin` — exactly the
   failure the curl installer was rejected for.
4. **No `node` for shebangs.** npm CLIs ship `#!/usr/bin/env node` and bun does
   not rewrite it.

Trap 4 turned out to be real, not hypothetical: **`ob`'s shebang is
`#!/usr/bin/env node`**. A bun-based image without Node would have failed at
first `ob` invocation, after a green build.

**Go — considered for the glue scripts, rejected.** Both bugs found in early
testing were silent bash failures, a class Go eliminates. But the scripts are
50–150 lines whose job is running `git` and checking exit codes; in Go you shell
out via `exec.Command` anyway, inheriting the error handling without escaping the
subprocess model. The proportionate fix was **`shellcheck` in CI** plus
`set -euo pipefail` as a standing convention.

Go *would* be right for building something new — an atomic-write MCP server, or
custom vault tooling. Static binary, trivial cross-compile, no runtime in the
image.

**That last paragraph came true twice.** `vault-mcp` is that server, and since
2026-08-30 its binary also ships *inside this image*, as the agent's move tool
(see [Giving the agent a move](#giving-the-agent-a-move)). The decision above is
unchanged — the image is still Node because `claude` and `ob` are npm packages,
and the glue scripts are still bash — but "the agent image contains no Go" is no
longer true, and the build now has a Go stage. The binary is static, so the
runtime image gained a file and no toolchain.

---

## Image and packaging

- **npm install, not the curl installer.** The curl installer writes to
  `$HOME/.local/bin`, and `$HOME` is a volume at runtime, so the mount would
  shadow the install. npm's global prefix is `/usr/local`, which survives.
- **Pin everything.** Base image by digest, exact versions for
  `@anthropic-ai/claude-code`, `obsidian-headless` and supercronic (the last by
  sha256 via `ADD --checksum`, so a tampered download fails the build). This
  image runs an agent with unattended write access to every note you own; a
  floating install means an unreviewed binary reaches your vault on the next
  rebuild.
- **Pin the tooling too, not just the runtime.** CI once installed Ubuntu's
  packaged `shellcheck` while local testing used a container image. The versions
  disagreed about which code to emit for trap-invoked functions, so a push failed
  on scripts that passed locally. Both now run the identical pinned container.
- **Assert in the build, not at first run.** `claude --version`, `command -v ob`,
  `supercronic -test`, `vault-mcp -version`. A failing `RUN` is much cheaper to
  debug than a container that starts and silently does nothing — and an MCP
  server that fails to launch is quieter still: a line in a log nobody reads,
  and an agent that just has no move tool.

---

## Identity: 1002, not 1000

`id kylejs` → `uid=1002 gid=100(everyone)`, and the image is built with those.

**The conventional-looking 1000 was a latent hole.** `getent passwd 1000` returns
nothing on this NAS, so 1000 is the *next* uid QNAP hands out — a new account
would silently inherit ownership of `home-sync` and `home-agent`, and `0700`
protects against everyone except the owner. It would have worked perfectly until
the day someone created an account.

`gid 100` is `everyone`, which every QNAP account belongs to, so group bits grant
nothing. The owner uid is the entire access-control story.

---

## Backups: bundles, and why only one

**Bundles, not a file-sync of the live repo.** A live repo is files under
mutation; a sync client will eventually capture a torn state and produce a clone
that fails `fsck`, silently, discovered only when you need it. A bundle is one
file, written atomically, verified before publishing, restored with a plain
`git clone`.

**One bundle, replaced in place — corrected 2026-08-08 after building the wrong
thing.** A grandfather-father-son scheme was implemented first, with
hourly/daily/weekly/monthly tiers and 47 retained copies. That is the right shape
for backups that each capture **one** point in time. A bundle captures **every**
point in time, so one current file already does the job, and 47 copies at 112 MB
were ~5.3 GB of storage and 2.7 GB/day of upload buying essentially nothing.

Worth being precise about the cause: this is *not* a git dedup failure. Git dedups
within a repository, and `git bundle --all` emits a self-contained pack — so 47
bundles are 47 unrelated files as far as Drive is concerned.

**Retention still exists, for a different reason than it appears.** Not to read
old notes — any single bundle does that — but because a corrupted, ransomwared or
history-rewritten repo is faithfully bundled straight over the top of the only
good copy. It is sized by *how long until you would notice*: first bundle of each
hour (keep 18), of each day (keep 7), of each month (keep 3).

**An hourly tier, added 2026-08-12.** `daily/` keeps the *first* bundle of the
day, so the newest copy predating a corruption is one from just after midnight —
recovery discards everything written since. `hourly/` bounds that loss without
changing what any single bundle can already do.

Kept at **18**: a waking day. The window is chosen so that damage introduced and
noticed on the same day is recoverable to the hour, which is the realistic case —
you spot something wrong in the evening about a note edited that morning. Beyond
a day the tier stops earning its keep, because one bundle already contains every
point in time and `daily/` covers the slower cases.

The cost is accepted, not overlooked. Attachments are in git, so a copy is
~112 MB and 18 of them is ~2 GB, taking the whole set to 29 files and ~3.2 GB —
against the 47 files and ~5.3 GB this section removed on 2026-08-08. The earlier
decision stands where it was aimed: at *deep* retention across four tiers as a
substitute for history. This is a single shallow tier bounding work-loss, sized
to a day rather than open-ended, and `weekly/` is still gone.

Because runs with an unmoved `HEAD` write nothing, `hourly/` holds the last 18
hours that *changed*. On a quiet vault that reaches back much further than 18
hours of clock time, which is the behaviour you want from a noticing window —
and it means the tier costs nothing while the vault is idle.

`BACKUP_KEEP_HOURLY` is the dial; `0` disables the tier without writing to it.

**Skip when `HEAD` has not moved.** `snapshot.sh` only commits when the vault is
dirty, so most hourly runs would otherwise write a byte-identical 112 MB file and
re-upload it.

**Attachments are in git**, chosen deliberately: 112 MB of the 119 MB vault.
Markdown-only bundles would be ~7 MB. Note this is hard to reverse — git keeps
every version of every blob permanently and the repo cannot be shrunk without
rewriting history and re-issuing every bundle.

**The HBS job must be `Mirror`, and that is a trade — corrected 2026-08-12.**
The job had been `Copy`, which never deletes at the destination. Every retention
number in this section therefore described the NAS only: `prune_dir` removed
bundles locally while Drive kept all of them, growing without bound. It stayed
invisible because the NAS side looked exactly right. Caught by a `weekly/` folder
still on Drive four days after the tier was removed and the local directory
deleted, alongside a stray `.vault-….bundle.tmp.lock` from the bug `b6463e1`
fixed — neither of which anything would ever have cleaned up.

The gap that allowed it was documentary: the setup step said where to point HBS
and when to schedule it, and nothing about how to configure it, so there was no
stated expectation for Drive's contents to be measured against.

`Mirror` bounds Drive at the NAS footprint, and the cost is that it propagates
destruction rather than only deletion. An emptied `BACKUP_DIR` on a still-running
NAS — failed volume remount, broken share path, mistaken `rm` — is mirrored
faithfully, taking every stamped copy with it. That is precisely the scenario the
tiers exist for, so the protection they offer now stops at anything that can
empty the source directory. Drive's 30-day trash is what remains. Accepted
because unbounded silent growth is the worse failure, but it is a real reduction
in what the off-site copy guarantees.

---

## Costs

> As understood at assessment. Approximate, USD.

| Item | Cost | Notes |
|---|---|---|
| **Claude subscription** | $20/mo (Pro) → $100/mo (Max 5×) | **Dominant cost**, and paid regardless of where the agent runs |
| **Obsidian Sync** | ~$4–5/mo | Hard requirement: `obsidian-headless` does not work without it |
| QNAP electricity (marginal) | ~$0–1/mo | Already always-on |
| Tailscale / GitHub + GHCR / Google Drive / age | $0 | Free tiers ample |
| **Marginal cost of this project** | **≈ $0–5/mo** | |

**Almost none of the cost is infrastructure.** Paying for Sync is the right call:
self-hosted LiveSync saves ~$50/year and costs you a CouchDB to operate and back
up — a bad trade for a system whose value proposition is reliability.

**Cost risk to watch:** before scheduling any recurring headless `claude -p` job,
run one and check the usage dashboard. There were reports mid-2026 of print-mode
runs billing against separate Agent SDK credit rather than a subscription.

---

## Networking

UniFi is a bystander. Its entire relevant surface — port forwarding, WAN rules,
hairpin NAT, dynamic DNS — is machinery for accepting inbound connections *from
the internet*, and nothing here needs one: `vault-mcp` is reachable from outside
over a Tailscale Funnel, which dials out rather than being forwarded to.
Tailscale is otherwise only for *your own* admin access.

**Amended 2026-08-29:** "never accepts an inbound connection" was already only
true of the WAN, and is now not true of the LAN either. The
[`dashboard`](../dashboard/) stack listens on a host port so a phone browser can
reach it, on the LAN and — via the host's own tailnet node — on the tailnet. The
conclusion above survives, because none of that involves the gateway; the
sentence needed narrowing, not reversing.

**Keep the QNAP as its own tailnet node.** UniFi OS can advertise the LAN as a
subnet route; do not put the QNAP behind it. Tailscale ACLs can target a real
node, but behind a subnet router the QNAP is just an IP in a CIDR.

This argument used to lean on Tailscale SSH as well — *"it only works to a real
node, and that is the deploy path"*. Tailscale SSH was [considered and
rejected](#tailscale-ssh-considered-and-rejected), so that clause is gone and
**the conclusion now rests on ACL targeting alone.** That is a thinner argument
than it was, and worth saying so rather than leaving the extra reasons implied.
It is still the right call — a node you can name in a policy beats an address in
a CIDR — but do not reach for VLAN isolation to shore it up: item 3 below is that
Tailscale bypasses VLAN semantics whether or not a subnet router is involved.

**Done 2026-08-12.** This section had described the
above as settled for months while Tailscale was never installed on the host at
all — the only node in the system was the `vault-mcp` Funnel sidecar, which is
container-scoped and gives the host nothing. That gap was caught earlier the
same day and recorded here as *"never actually done"*; the QNAP was then
genuinely added to the tailnet a few hours later, which is what this replaces.
The lesson outlives both notes: **this file describes intent, so verify against
the host before trusting a networking claim in it.**

The QNAP is now a real tailnet node (`kyles-nas`), running the current Tailscale
client. **The deploy path is SSH over the tailnet, authenticated by key through
the 1Password agent** — not Tailscale SSH, which is rejected below.

### Tagging: rejected, and the expiry hazard it was hiding

The node stays **untagged and user-owned**, so ACLs cannot target it by tag. Do
not write anything that assumes ACL-governed access to it.

Tagging was assessed on its merits rather than waved away, and it does buy real
things. Tailscale's own docs: *"the tagged device's key expiry is disabled by
default"*, and *"applying a tag to a device removes any user-based
authentication"* — a node owned by the tailnet rather than by a person, which is
exactly the isolation argument `vault-mcp`'s compose file makes for the Funnel
sidecar. It also gives a stable ACL handle that survives a rebuild.

None of it lands here:

| Benefit | Why not |
|---|---|
| Key expiry disabled | The per-node **Disable key expiry** toggle does this alone, with no re-auth |
| ACL targeting | Tailscale SSH was the thing that wanted it, and it is rejected above |
| Stable handle across rebuilds | Only matters under a deny-by-default ACL, which this tailnet does not have |
| `autoApprovers` | Accepts users as well as tags, and only matters if this node advertises routes |

Against that, tagging costs a **re-authentication of the one node you cannot
re-authenticate remotely**, and removing the user association takes user-scoped
features with it — Taildrop among them, so test before committing if you use it.

**The real finding was underneath the question.** Untagged nodes keep key expiry,
and `kyles-nas` was set to expire **2027-02-08, 179 days out**. On expiry the
node drops off the tailnet and recovering it means running `tailscale up` on the
box — which needs the access that just expired. Expiry is harmless on the Mac and
the phone, where re-auth is a browser tab. It is a scheduled lockout on the one
node you would need access to fix. `vault-mcp` never had this problem, and not
by design: it is tagged, so expiry was off as a side effect.

So the resolution is **disable key expiry on `kyles-nas`, do not tag it** — the
cheap fix for the hazard that mattered, and none of the risk of the one that did
not. Done and confirmed 2026-08-12: `KeyExpiry` on that node reads `never`.

**That is console state, which this file cannot enforce** — a later change there
would silently make this paragraph false, which is the exact failure this
section keeps finding. Check it rather than trusting the text:

```sh
tailscale status --json | \
  python3 -c "import json,sys; d=json.load(sys.stdin); \
  print({v['HostName']: v.get('KeyExpiry','never') for v in d['Peer'].values()})"
```

**Revisit tagging if** the NAS becomes an exit node or subnet router, or a
deny-by-default ACL gets written. Both are real triggers, and both are jobs done
standing at the NAS anyway — which is precisely when the re-auth stops being a
risk. Declare `tagOwners` before re-authenticating, or the tag locks you out of
the node you are holding.

### Tailscale SSH: considered and rejected

Rejected 2026-08-12, having been assumed for months. The question that settled
it: what does it actually buy *here*?

- **It does not reduce attack surface, it adds to it.** QNAP's `sshd` has to keep
  running regardless, because the LAN is the fallback path when Tailscale is
  down. Enabling Tailscale SSH means two SSH servers on one host, not one.
- **Revocation is the real benefit, and it is worth little at this scale.**
  Cutting access from the admin console instead of editing `authorized_keys`
  matters when there are many users or many keys. There is one of each, held in
  1Password.
- **It breaks the client config that makes `ssh nas` work everywhere.** That
  alias pins the tailnet path and the LAN path to a single `known_hosts` entry
  via `HostKeyAlias`, on the grounds that they are one machine with one host key.
  Tailscale SSH ends that: `tailscaled` answers port 22 on the tailnet interface
  with its *own* host key while OpenSSH answers on the LAN, so one alias would
  present two keys and warn on every path switch.
- **It drags in tagging.** The governed-access story only pays off with
  tag-based ACLs, and tagging is the one item here with genuine lockout risk.

What it does *not* cost is the alias itself — `ssh nas` would keep working, since
the connection is intercepted by `tailscaled` rather than redirected to a
different name. That was the initial objection and it was wrong; the real cost is
the host key split above.

**Revisit if** a second person or a second admin device ever needs access, or if
session recording becomes a requirement. Both are the conditions under which
console-side revocation stops being ceremony.

Three things that could bite:

1. **Threat Management in Detect-and-Block** occasionally interferes with
   sustained UDP flows. If `tailscale status` shows `relay` rather than `direct`,
   disable it as a test — **but rule out the address family first**, because for
   this network that is not a hypothetical. Measured 2026-08-12: the NAS has
   IPv4 and **no** IPv6; a client on an IPv6-only carrier network (Three 5G,
   NAT64) had IPv6 and **no** IPv4. Zero overlap, so a direct connection was not
   blocked, it was impossible, and every path showed `relay "lhr"` at ~123 ms.

   Generalised: **while home has no IPv6, any IPv6-only client will relay, always,
   and no UniFi setting can change it.** Only suspect Threat Management when you
   see relay from a client that shares an address family with the NAS.
2. **Direct vs relayed** — Tailscale wants UDP 41641 outbound; relay over 443
   works but adds latency.
3. **VLAN semantics get bypassed** — if the QNAP sits on an isolated VLAN,
   Tailscale routes around that entirely.

### IPv6 at home: considered and rejected

The obvious response to item 1 is "so give home IPv6". Rejected 2026-08-12.

**Current state, measured.** IPv6 is switched off on the NAS itself in QTS:
`disable_ipv6` is `1`, `eth0` holds a link-local `fe80::` address and nothing
else, and there is no IPv6 default route. Whether UniFi or the ISP offer IPv6 is
therefore **unknown** — the NAS would not process a router advertisement even if
one arrived. Enabling IPv6 in *Network & Virtual Switch* and re-checking for a
global address is the cheap way to find out, and is the first step if this is
ever revisited.

Three reasons not to:

1. **It is not a NAS setting.** IPv6 arrives by router advertisement, so once
   UniFi advertises a prefix *every* device on the LAN gets a globally routable
   address. There is no enabling it for one host.
2. **It changes the exposure model of the whole network.** The claim at the top
   of this section — UniFi is a bystander, nothing accepts inbound — currently
   rests on NAT: hosts here are not addressable from the internet at all. Under
   IPv6 they are addressable and default-deny firewall rules are the only thing
   in the way. That default is correct out of the box, and it is still a
   different posture for the machine holding every note you own.
3. **Almost nothing here would benefit.** Remote Control dials *out* through
   Anthropic's cloud and the voice connector arrives over Funnel; neither uses
   the Tailscale peer link. The only traffic that would go direct instead of
   relayed is your own admin SSH. The observed cost of relaying was ~100 ms of
   latency, once, at a place stayed at rarely.

Relaying is the system behaving correctly: it degraded to a slower path across a
network where Teleport could not connect at all, which is the argument for
Tailscale made below.

**Revisit if** the home ISP moves to IPv6-only — at which point this stops being
optional — or if a network used *regularly* turns out to be IPv6-only. Treat it
as a network project with the UniFi firewall reviewed deliberately, not as a fix
for a relay.

**Do not swap Tailscale for UniFi Teleport** — no ephemeral auth keys, no
per-device ACLs, no SSH. Run it alongside if you like.

There is now a harder reason than preference. **Observed 2026-08-12** on an
IPv6-only carrier network (Three 5G, NAT64, no IPv4 path whatsoever): Teleport
could not connect, while web browsing was completely unaffected and Tailscale
reached `kyles-nas` without trouble.

The explanation — inferred from Teleport being WireGuard to the gateway's WAN
address, not verified at packet level — is that the endpoint is a **raw IPv4
literal**. A literal address never passes through DNS64, so nothing synthesises
an IPv6 form and NAT64 has nothing to translate; browsing works precisely
because it resolves *hostnames*, which do get synthesised into `64:ff9b::/96`.
No gateway configuration can fix that from the client side. Whatever the exact
mechanism, the operational rule holds: **anything that must work from an
arbitrary network cannot depend on an IPv4 literal.**

The same asymmetry explains a support-call symptom worth recognising: WiFiman and
iStat Menus both reported "no internet" on that network while everything actually
worked, because their reachability checks ping IPv4 literals. The check was
measuring the one thing the network genuinely lacked.

---

## Traps found while building

**Five bugs, all silent.** Do not reintroduce:

1. `[ cond ] && cmd` under `set -e` aborts the script when the condition is
   false — killing the run before pruning.
2. `set -o pipefail` plus `ls` over an empty glob returns 2 and takes the script
   down; rotation appeared to work while never running.
3. `git init --bare <path>` refuses to run while `GIT_WORK_TREE` is exported. The
   init must happen in a subshell with both unset.
4. `backup.sh` exited **0 on a corrupted repo**, reporting success while
   producing nothing — a scheduled job would have looked healthy forever. The fix
   must inspect the filesystem for refs, not ask git: a damaged `HEAD` makes git
   treat the directory as not-a-repository, so `git show-ref` fails identically
   to the legitimate fresh-repo case. **git cannot diagnose its own corruption.**
5. Unpinned `shellcheck` in CI vs a pinned container locally — see *Image and
   packaging*.

**Three findings only a live run could surface:**

1. **`claude setup-token` cannot establish Remote Control sessions.** It was
   documented as an equal alternative to the interactive login and would have
   silently cost the architecture. Use `claude` → `/login`; in a container the
   browser callback fails and you paste a code back, which is a documented path.
2. **`ob` reports the container hostname to Obsidian Sync as the device name**,
   and Docker defaults it to the container ID — so every recreate registered a
   new phantom device. Hostnames now pinned in compose.
3. **Remote Control pairing survives a container recreate and a NAS reboot.** The
   design assumed re-pairing after every deploy; it is not needed. This also
   weakens the original case for manual deployment, which was that a deploy would
   silently drop the phone's session. What remains is only that a deploy can
   interrupt a run in progress. The flip side: the durability that makes deploys
   painless would also make a leaked pairing URL persistent.

**Verification notes worth keeping:**

- `/permissions` is an interactive CLI panel and is **not reachable from a Remote
  Control session**. Do not start a second `claude` in the container to inspect
  it — that is two instances against one credentials volume. Verify
  **behaviourally** instead: it tests what the agent can actually do rather than
  what the config claims.
- Permission-rule syntax: the docs demonstrate only `./relative` and `~/home`
  forms, and do not say whether an unrecognised rule is rejected or **silently
  ignored**. Both forms are listed for the home directory as belt and braces. The
  `./**/AGENTS.md` form was confirmed working by behavioural test.
- QNAP volume names are not predictable — `CACHEDEV1_DATA` is the common default
  and was *not* correct here. Find yours with `ls -d /share/*/`.
- HBS can only select **shared folders**, not arbitrary directories — but a
  shared folder can be pointed at an *existing* path, so exposing `backups/`
  needed no data movement.

---

## Deferred: skills authored in Obsidian

Goal: write a Claude Code skill on the phone in Obsidian and have the NAS agent
pick it up, with no deploy step.

**Obsidian Sync cannot carry `.claude/`, and no toggle changes that** — it
handles `.obsidian` only. Verified: `.claude/settings.json` sat on the NAS for
hours without appearing on the Mac. So skills cannot simply live where Claude
Code looks for them. Proposed shape:

```
<vault>/Extras/Claude Skills/<name>/SKILL.md      ordinary notes — sync normally
<vault>/.claude/skills -> ../Extras/Claude Skills  symlink, created once per host
```

`SKILL.md` is markdown with YAML frontmatter, exactly what Sync and Obsidian
handle natively. The symlink sits in the unsynced `.claude/`, so `preflight.sh`
would create and verify it — once per host, never touched as skills change.

**The trade-off is why this is a decision, not a task.** Skills are instructions
the agent follows, and `./.claude/**` is write-denied precisely so an injected
note cannot rewrite the agent's own instructions. An ordinary vault folder
reopens that: a hostile clipping could have the agent author a skill, inherited
by every later session. Mitigation is to deny the agent writes there:

```json
"Edit(./Extras/Claude Skills/**)"
```

One rule, not two, and `Edit` rather than `Write` — see
[the snapshots deny](#the-snapshots-deny-that-was-not-one) for why a
`Write(path)` rule here would have been decoration. A `Read` deny would also
block writes, but it is the wrong tool: the agent must be able to *read* a
skill to follow it.

Skills then flow *in* from a human over Sync while the agent may read and use but
never author them. **That asymmetry is the whole reason the approach is safe.**

**Settle first:** does the existing `Read(./.claude/**)` deny interfere with
skill *loading*? Loading is believed to happen in the harness rather than the
permission layer, but this is untested — and if wrong, the deny silently disables
every skill written. One throwaway skill emitting something identifiable,
requested from the phone, answers it.

**Smaller caveats:** Sync carries markdown plus `image, audio, pdf, video`, so a
skill bundling `.sh`/`.py`/`.json` will not fully arrive — acceptable since
`Bash` is denied and NAS skills are instructional. Skill files also appear in
Obsidian search and the graph as ordinary notes.

---

## The snapshots deny that was not one

Added 2026-08-30, correcting the syntax note recorded on 2026-08-08. This is a
reversal of a *belief*, not of a choice: the intent was always to deny these
paths, and for three weeks the rules that expressed it did nothing.

The 2026-08-08 note in `vault-claude-settings.json` said the docs demonstrated
only the `./relative` and `~/home` forms, that a rule like `Read(/home/app/**)`
was undocumented, and that it might be rejected or silently ignored. It hedged by
listing both forms for the home directory, reasoning that "a redundant rule costs
nothing, a silently-dropped one costs the token."

[The permissions
reference](https://code.claude.com/docs/en/permissions) now documents four forms,
and the hedge turns out to have been load-bearing:

| Form | Resolves to |
|---|---|
| `//path` | `/path` — absolute, from the filesystem root |
| `~/path` | from the home directory |
| `/path` | **relative to the settings source** — for this file, `<vault>/path` |
| `path`, `./path` | relative to the current directory |

The single leading slash is not an absolute path. `Read(/snapshots/**)` never
denied `/snapshots`; it denied `/vault/snapshots`, which does not exist. The same
was true of `/home/app/**` — and *that* one was saved by the redundant `~/**`
rule the note added for the wrong reason. The snapshot rules had no such twin, so
they protected nothing while every check still passed. Precisely the failure mode
[Invariants](ARCHITECTURE.md#invariants) exists to catalogue, in the file that
catalogues it.

**Why it mattered even with no egress.** `/snapshots` is a git repo of the whole
vault, so its `.git` holds every past version of every note — including notes
since deleted, and, when `MCP_EXCLUDE` grows, folders deliberately hidden from
the other surface. Reading it was never a breach on its own: `vault-claude` has
no way to send anything out, which is the entire design. What it was is a
*staged* one. Any future decision to give a vault-reading surface web access
would have silently inherited a readable archive of everything the vault has ever
contained, and nothing in the rules would have flagged it.

**What changed.** Every rule meaning a real absolute path is now spelled `//`.
The home directory keeps both `//home/app/**` and `~/**`, now on purpose: they
are two different claims — the mount, and wherever `$HOME` actually points — and
either could drift without the other.

**`Write(path)` rules went with them.** The same reference states that file
permissions are checked against `Edit(path)` and `Read(path)` rules *only*; a
path rule written for `Write` is accepted, never consulted, and warned about at
startup. Ten of them were in the deny list. Nothing is lost by removing them,
because a `Read` deny already blocks Edit and Write on the same path, including
creating a new file there. `AGENTS.md` and `CLAUDE.md` are the exception, and the
reason is the one that has always applied: they must stay readable, so they carry
no `Read` deny for a write block to come from, and their explicit `Edit` denies
are doing the work. `Edit` also covers `NotebookEdit`, which a `Read` deny does
not.

**The lesson worth keeping.** The hedge was right to exist and wrong in its
reasoning, and it protected the wrong path by luck. A permission rule that is
silently inert is indistinguishable from one that works, from inside the box.
Confirm the rules loaded with `/permissions` in a live session; do not infer it
from the absence of a complaint.

---

## Two surfaces, two different mitigations

Added 2026-08-11, after noticing that the deny list and the MCP server look like
they contradict each other and finding that they do not — but that the gap next
to them is real.

Denying `WebFetch`/`WebSearch` in `vault-claude` closes an **outbound** channel.
`vault-mcp` opens **inbound** reachability, and serves no tool that fetches
anything. Different directions, different threats, no contradiction.

The real problem is one layer down: **a deny list is a property of the client,
not of the vault**, and the entire point of `vault-mcp` is to hand the vault to a
client this repo does not administer. In a claude.ai conversation the vault
connector sits alongside web search, web fetch and every other connector on the
account — so the injection-to-exfiltration path the deny list was written to
break is reassembled there, out of reach.

Nothing on this side can revoke claude.ai's tools. What it can do is decide what
that conversation is allowed to see, which is `MCP_EXCLUDE`: the folders where
material the user did not write arrives are invisible to the voice surface. Each
surface then breaks a different leg of the same three-part risk — `vault-claude`
has the untrusted content but no egress, `vault-mcp` has the egress but not the
content. Argued in full at
[`../vault-mcp/README.md`](../vault-mcp/README.md#what-voice-cannot-see).

Deliberately **not** mirrored into `vault-claude-settings.json`. The agent needs
to read the Inbox — triaging it is most of what a notes agent is for — and it is
the surface that cannot leak. Every *other* denial in `vault.go` must stay in
lockstep with that file; this one must not, and the invariant tables say so on
both sides.

> **Superseded in part, 2026-08-31.** There are now three surfaces, not two.
> Everything above still holds — `vault-claude` still denies the web tools, and
> `MCP_EXCLUDE` still narrows what voice can see. What this entry got wrong was
> treating "no web tools anywhere near the vault" as the end of the argument
> rather than one surface's share of it. A third agent, `vault-research`, has
> the web tools and no vault, breaking the *first* leg where these two break the
> second and third. See [A third surface for
> research](#a-third-surface-for-research).

---

## A third surface for research

Added 2026-08-31. This **reverses** the absolute stated on 2026-08-11 under
[Two surfaces, two different mitigations](#two-surfaces-two-different-mitigations),
which said the web tools were denied and left it there. They still are, on
`vault-claude`. What changed is that a third agent now has them.

### What forced it

The vault agent cannot research. That was the intended trade, and it held until
the task was "collect fly patterns and save pictures of the flies", where it
fails twice over.

The first failure is the expected one: no web search, so no finding anything.
The second was not expected and is the more interesting of the two. **Even with
`WebFetch` allowed, the picture could not have been saved.** WebFetch fetches a
page, converts it to Markdown and runs a prompt against it with a small model,
so what returns is text; Claude never receives the response body. `Write` emits
text, so it cannot author the bytes even when handed them, and `Bash` is denied
everywhere here so there is no `curl`. No setting in
`vault-claude-settings.json` would have produced a file.

So the request that looked like "relax the deny list" was two separate things: a
missing capability that no permission grants, and a policy question. They are
answered separately below and by different mechanisms.

### The rule that scales, and the one that did not

The first design here put `WebFetch` behind a domain allowlist shared with the
download tool: research confined to sites you had approved, everything else
prompting on the phone. It is a real control and it is what the containment
literature recommends. It was rejected for a plain reason — **a list per topic
does not survive contact with actual research.** Flies this week, something
unrelated next week, and either the list grows until it means nothing or the
agent stops halfway through every task.

What scales is not restricting where the agent can reach. It is restricting
**what is in the room when it reaches there.**

| | reads | can reach |
|---|---|---|
| `vault-claude` | the whole vault | nothing |
| `vault-mcp` (voice) | the vault minus `MCP_EXCLUDE` | claude.ai's tools, not ours to set |
| `vault-research` | a scratch volume | the open web |

Each surface is missing a different leg of the same three-part risk, which is
the pattern the 2026-08-11 entry established. This one is missing the first leg
rather than the third: the research agent has untrusted content and a way out,
and nothing of yours to send.

The enforcement is the **working directory**, not a deny list. `research.sh`
starts Claude Code with cwd set to `/scratch`, and the vault is not mounted into
that container at all. There is no rule to get wrong, and no rule to
accidentally delete — which matters, given that this repo has already shipped
one deny list that denied nothing for three weeks
([the snapshots deny](#the-snapshots-deny-that-was-not-one)).

`FETCH_ALLOW_HOSTS` still exists and defaults to empty. The knob is there for a
deployment that wants both controls; it is not how this one is run.

### What this costs, stated plainly

**Research cannot consult your notes.** This is the real price and it is not
small: "research in accordance with my data" becomes two steps, with a human
deciding what crosses. Copying a note into the scratch volume is allowed and is
exactly the point — that copy is a deliberate act, per task, rather than a
standing grant over everything.

**A second always-on Claude Code process.** `RESEARCH_MEM_LIMIT` is separate
from `AGENT_MEM_LIMIT` because the two have to fit in RAM together; two 6 GB
caps did not fit the ~11.7 GB free before the memory upgrade. Worth being clear
that this is RAM, not disk: adding drives does not help.

**A second credential volume and a second login.** Not tidiness. Two Claude Code
instances against one home directory corrupt `~/.claude.json`, which is already
in [Invariants](ARCHITECTURE.md#invariants), so sharing `home-agent` would break
both agents rather than one.

**A third writer near the vault.** It writes only to the scratch volume, so it
does not add to the `ob sync` race in
[Known unresolved risk](ARCHITECTURE.md#known-unresolved-risk). `vault-claude`
copying findings across is a normal vault write, stamped and snapshotted like
any other.

**The residual risk is real and is accepted.** A fetched page can carry
instructions, and the research agent will read them. It can be talked into
fetching things, writing nonsense, or wasting an afternoon. What it cannot do is
send your notes anywhere, because it does not have them. Damage is confined to a
volume that is deleted on a schedule. This is containment, not prevention, and
it is the same call already made for `vault-mcp`, whose README says of
`MCP_EXCLUDE`: *"This narrows the surface; it does not eliminate it."*

### Why the scratch volume is not inside the vault

The first sketch put it at `<vault>/Research/_scratch/`, so Obsidian Sync would
carry results to the phone and `vault-claude` would read them with no extra
mount. Both are genuine benefits and both were given up.

Retention is why. Folders are deleted after `SCRATCH_RETENTION_DAYS`, and
`scratch-sweep.sh` is **the only thing in this repository that deletes
anything** — `vault-mcp` has no delete tool, `move_file` refuses to overwrite,
and the snapshot repo exists so nothing is lost. Putting that script's target
inside the synced vault means a bad glob deletes notes and Sync propagates the
deletion to every device before anyone notices. A separate volume costs one
mount. `preflight.sh` fails if the two are ever nested, in either direction.

### Rejected: Claude Cowork with the vault connector

Considered because Cowork has a real workspace and would sidestep the file
problem. Rejected twice over.

MCP has no interoperable client-to-server file upload in the published spec.
Binary exists server-to-client (resources carry base64 `blob`), but the
direction needed here is the one that is not standardised —
[SEP-2356](https://github.com/modelcontextprotocol/modelcontextprotocol/pull/2356)
proposes file inputs as `data:` URIs and is still a proposal. A bespoke
`create_attachment(base64)` tool would depend on the client passing bytes it
has no obligation to pass.

The deeper objection is that it would not help. Cowork puts the vault connector
beside web search in one conversation, which is the `vault-mcp` exposure rather
than an escape from it — and Cowork's workspace runs on Anthropic's side, while
the NAS publishes no inbound ports on purpose.

---

## Passing a brief to the research agent

Added 2026-09-05. This **narrows** the absolute stated on 2026-08-31 under
[A third surface for research](#a-third-surface-for-research), which said the
vault reaching `vault-research` was the direction that must never exist. It now
exists, through one directory, and this entry is the argument for why that is
the right trade and what it costs.

### What forced it

The handoff worked in one direction and was silent about the other. Research
output crossed to the vault through `/scratch`; a research *brief* crossed by a
human reading it off one phone screen and typing it into another.

A real session made the cost concrete. The vault agent composed a 268-line brief
— 26 subjects, per-item source URLs, a manifest schema, an image size floor —
saved it into the vault so it could be copied from something better than
scrollback, and the operator pasted it across. Then the same session could not
tell whether that brief had already run, because nothing on either side recorded
that it had, and several turns went into establishing that the work was already
sitting in `/scratch` and had been for a day. The transcript ends with the user
asking, reasonably, *"so do I need to run the research markdown you gave me or
not?"*

So the friction was two things wearing one coat. Transcription, and the absence
of any shared state about a job. The second is the more expensive and the
cheaper to fix.

### Options assessed

**Copy-paste, kept as it was.** Free, and it is what the design intended: a
human deciding, per task, what crosses. Rejected because the deciding is not
what is expensive — the typing is, and a brief that costs a screenful of typing
gets shortened at the point of transfer, which is exactly where a research task
should not be lossy. It also leaves the state problem entirely unsolved.

**A `homelab brief <note> <topic>` command on the NAS.** Copies one named file
from the vault into the scratch volume. Attractive because the human still names
what crosses, and it is the same act `research.sh`'s header already sanctions,
scripted. Rejected on use: this system is driven from an iPhone, and a step that
requires an SSH session is a step that will not be taken. A dashboard button was
considered as the phone-reachable version and rejected in turn — `agent.go`
builds argv from constants plus a name matched against `nameRe`, and a
vault-relative path is free text, so it would have meant either a designated
folder the dashboard enumerates or the first passthrough field in that file.

**An `export_brief` tool on `vault-mcp`, served to `vault-claude`.** Rejected
outright. It puts "compose text" and "deliver it to the surface with egress"
into one tool call, with no human in the loop at all, and `vault-mcp` already
refuses the analogous pairing: `MCP_FETCH=1` and `IMPORT_DIR` together are a
startup error, because reaching the web and writing to the vault in one process
is the combination the whole design exists to prevent. A tool would also buy
nothing in containment — the container needs the write mount either way, and
`Write(path)` permission rules are accepted and never consulted, so nothing in
a settings file could have narrowed what the agent's own `Write` can reach.

**A jobs directory in the vault, mounted into `vault-research`.** The obvious
shape, and the one the request was phrased as. Rejected, and the reason is worth
stating because it is the near-miss: it puts a slice of the vault inside the
container that holds `WebSearch` and `WebFetch`. `research.sh` refuses to start
if `/vault` appears there, so shipping it would have meant writing the first
exception to that tripwire — and the tripwire's value is that it has none.
Worse, the jobs folder would be a vault path, so `move_file` (rooted at
`/vault`, and permitted to relocate attachments) could put arbitrary notes and
images into it. The channel would have been a file copy rather than a
composition.

### What was built

The inversion: the meeting point is on the **scratch** volume, which
`vault-research` already had in full, and the new access belongs to
`vault-claude` — the agent with no way out.

```
/scratch/jobs/
  2026-09-05-fly-images.md        the brief      -- written by vault-claude
  2026-09-05-fly-images.run.md    what happened  -- written by vault-research
```

`docker-compose.yml` mounts `${SCRATCH_HOST_PATH}/jobs` into `vault-claude`
read-write, nested inside the read-only `/scratch` it already had. Docker applies
mounts shortest destination first, so the narrow one lands on top and everything
else on the volume stays refused **by the kernel**. That is the same reasoning as
the working directory being the boundary rather than a deny list: there is no
rule here to mis-spell, and no rule to delete. It is also the only mechanism
available — a `Write(path)` deny would have been decoration, for the reason
[the snapshots deny](#the-snapshots-deny-that-was-not-one) established.

`vault-research`'s mount list did not change. Neither did `research.sh`'s
tripwire.

One file per writer, never shared. The brief is written once and not revised;
the run file is the other agent's, and carries `status`, `outputs`, and what is
missing. Nothing enforces the split — both agents can physically write both
files — but a brief that could be edited after the fact is a brief nobody can
audit, so it is stated in the contract as a rule.

**The contract lives at `<scratch>/JOBS.md`, not inside `jobs/`.** Both agents
read it; neither can write it. That placement is load-bearing rather than tidy:
`jobs/` is the one directory `vault-claude` can write to, so a protocol file
kept there would be standing instructions the vault agent could rewrite for the
agent holding the web tools. At the scratch root it is read-only to
`vault-claude` at the kernel and write-denied to `vault-research` by policy, and
reinstalled from the image on every start. `preflight.sh` fails if anything
named `CLAUDE.md`, `AGENTS.md` or `JOBS.md` appears under `jobs/`.

`scratch-sweep.sh` skips `jobs/`, which is a change to the one script here that
deletes anything and so is worth naming twice: the directory is the ledger, and
it is a mount point — sweeping it would break `vault-claude`'s next start rather
than merely lose a record.

### What this costs, stated plainly

**It is a path from the vault side to the web side, and there was none before.**
`vault-claude` reads clipped articles and forwarded mail. An injection in one of
them can tell it to write vault content into a job, and `vault-research` will
read that and can reach the network.
[The three-surface split is per session](ARCHITECTURE.md#the-three-surface-split-is-per-session-not-across-time)
now records this as the shorter of two such chains.

What bounds it, none of which is prevention:

- **Only composed text crosses.** `move_file` is rooted at `/vault` and refuses
  every spelling of a scratch path, `Write` emits text so it cannot author
  bytes, and `Bash` is denied. A note can be retyped by the model; it cannot be
  copied, and an attachment cannot cross at all.
- **The crossing is a file, and it stays.** A job that quotes half a note is
  visible in `jobs/` afterwards, next to the run that consumed it. The voice
  path's equivalent is a note whose provenance is a `git log`.
- **The receiving agent is told to refuse the tail of it.** `JOBS.md` instructs
  `vault-research` to refuse any job that asks it to submit, post, upload, email
  or encode a payload into a request, and to say in the run file that it did.
  That is an instruction to a model, so it is the weakest control here and is
  listed last on purpose.
- **`vault-claude` is told what a job is for.** Its policy comment and `JOBS.md`
  both say a job carries what the research needs and no more.

**Accepted, on the same basis as the surface itself.** This is containment and
audit rather than prevention, which is the call already made for `vault-mcp`'s
`MCP_EXCLUDE` and for the research surface's own residual risk. The alternative
on offer was not a safer handoff; it was the same handoff performed by a human
retyping the payload, which protects nothing that this does not — the human was
never reading 268 lines for exfiltration — while reliably making the brief worse.

### What it does not do

It does not let `vault-research` read the vault, and it does not let
`vault-claude` reach the network. It does not give either agent a way to start
work on the other's behalf: a job sits in `jobs/` until a human tells the
research session to look, which is one line on a phone instead of a brief.
Automating that last step — waking the research agent when a job appears — was
deliberately left out. It is the difference between a directory two agents share
and two agents that talk to each other unattended, and nothing about this
change requires it.

---

## Fetching attachments

Added 2026-08-31, with [the research surface](#a-third-surface-for-research)
that needed it. `fetch_attachment` is a second tool on `vault-mcp`'s stdio
surface, served **only** when `MCP_FETCH=1`, which only
`vault-research-mcp.json` sets.

### Why a tool rather than a permission

Covered above and worth repeating in the place someone will look: no combination
of permission rules puts an image on disk. WebFetch returns text by design,
`Write` cannot produce binary, `Write` emits text so it cannot author the bytes, `Bash` is denied. This
was a capability gap that looked like a policy problem, and enabling the web
tools would have paid the security cost without fixing it.

### Why it is safer than the tool it sits beside

**The bytes never reach a model.** It returns a filename, a size and a content
type. A page carrying an injection can only deliver it to something that reads
the page, and this reads it into a file. That is the quarantined-LLM idea from
[the design-patterns paper](https://arxiv.org/abs/2506.08837), obtained by
plumbing rather than by asking a filter to be right every time.

It is also why the research agent is told, in its `CLAUDE.md`, to describe an
image from its page and caption rather than from the file. It cannot do
otherwise, and saying so avoids it trying.

### What it refuses, and why each rule is in Go

Every check below is in `../vault-mcp/fetch.go` rather than in a settings file,
because a settings file cannot express any of them:

- **Plain HTTP.** An attachment fetched over a rewritable channel is a file of
  unknown provenance with a trustworthy name.
- **Private, loopback, link-local, multicast and CGNAT addresses**, checked at
  **connect time** rather than by parsing the hostname. The research container
  sits on the network with the QNAP admin interface, the other stacks and the
  dashboard's Docker socket proxy — and the NAS is on a tailnet, which is why
  100.64.0.0/10 is refused too. Hostname parsing would not be enough: DNS can
  answer `127.0.0.1`, and can answer differently on the second lookup than the
  first. Checking the peer closes both, on every redirect hop.
- **Bytes that disagree with the extension.** The server's `Content-Type` is a
  claim by the party being guarded against; the first 512 bytes are evidence.
- **SVG**, alone among the image types `move_file` will relocate. An SVG is a
  document that can carry script, and this is the one tool bringing files in
  from outside. The fetchable set is deliberately smaller than
  `attachmentExts`: that list says what may be moved *within* the vault, where
  the file is already yours.
- **Overwriting**, including onto a dangling symlink, re-checked immediately
  before the rename. Same rule as `move_file` and the same reason: overwrite is
  the delete path wearing a different name.
- **Every path `move_file` refuses.** It reuses `writablePath` rather than
  restating it, so no dotted folder, no `AGENTS.md`, no escape through a
  symlink, and no way for the two to drift.

`MCP_FETCH=1` on the HTTP surface is **fatal at startup**, not ignored. That
surface serves claude.ai, which has web access of its own; an operator setting
it there was trying to give an internet-facing endpoint an outbound fetch, and
the useful response is a refusal rather than a tool that silently does not
appear.

### What it does not defend against

A file you asked for, from a host you allowed, is downloaded. This is not a
malware scanner. Its promise is narrower: the download cannot reach inside the
network, cannot land outside the tree it was pointed at, cannot masquerade as a
type it is not, and cannot say anything to the model that fetched it.

**Every clause above is about `fetch_attachment` and about nothing else on that
surface.** `routableIP` and the connect-time dial guard exist because the
research container sits on the NAS's network and its tailnet — but that is a
property of the *container*, not of one function, and `WebSearch` and `WebFetch`
are allowed on it without a domain restriction. So the guarded path is the one
where the bytes are quarantined, and the unguarded path is the one where they
are not.

Whether `WebFetch` can actually reach a private address from here is **not
established**. Its documented behaviour is that it fetches a page, converts it
and runs a prompt against it with a small model; the documentation does not say
where the request originates, and there is no way to test it from this
repository. The private-address denial that *is* documented belongs to a
different tool. So this is recorded as an open question rather than either a
hole or a guarantee:

- If `WebFetch` runs on Anthropic's infrastructure, it cannot see this LAN and
  the question is moot.
- If it runs locally, the research agent can read internal endpoints and relay
  what it finds, and `routableIP` is guarding a door beside an open window.

Two things that would settle or close it, neither done here: try fetching a LAN
address from a live research session and see what comes back; or narrow the
surface with `WebFetch(domain:...)` rules, which is the control the entry above
rejected for *research* reasons and which would apply just as well for this one.

What is not acceptable is leaving `routableIP`'s rationale reading as a property
of the surface, which is what it did until this paragraph was written.

---

## Importing an attachment

Added 2026-08-31, after review of the branch that added
[the research surface](#a-third-surface-for-research) found that its headline
capability did not complete.

### The gap

`fetch_attachment` put a fly photograph on the NAS. Nothing could then put it in
the vault. Traced end to end:

| Route | Why not |
|---|---|
| `move_file` | rooted at `VAULT_DIR`; `resolveRef` returns `ErrOutside` for `/scratch/flies/a.jpg` and for `../scratch/flies/a.jpg` |
| `Write` | emits text, so it cannot author the bytes |
| `Bash` | denied on every surface here |
| the mount | `/scratch` is `:ro` on `vault-claude` |

Markdown crossed, because a note can be read and re-written. The one file type
the fetch tool exists to produce was the one type that could not cross, and
`scratch-sweep.sh` deleted it after a week. Three documents said otherwise.

That is worth recording as a design failure rather than a bug: each piece was
built correctly against its own rules, and the gap only existed *between* them.
The containment rules that make `move_file` safe are exactly what made the
handoff impossible.

### What was built

`import_attachment`, on `vault-claude`'s stdio surface, enabled by `IMPORT_DIR`
— which only `vault-claude-mcp.json` sets. It copies one attachment out of the
scratch volume and into the vault.

**This is the only place any surface creates a non-markdown file in the vault**,
and it revises, for the second time in one change, the rule that
[moving stays the only thing](#a-third-surface-for-research) done to one. Stated
plainly rather than buried: the rule is now *relocating and importing*, importing
only from a configured root outside the vault, and *reading* an attachment is
still refused everywhere.

What keeps it narrow:

- The source is a **separate `Vault`** rooted at `IMPORT_DIR`, so every
  containment and symlink check applies at that end too, in that root.
- Both ends go through `writablePath`: no dotted folder, no `AGENTS.md`, no
  `CLAUDE.md`, either direction.
- Attachments only. Markdown is refused, because the agent has `Read` and
  `Write` for notes and routing one through here would skip the `PostToolUse`
  stamp that gives an agent's writes their attribution.
- The extension cannot change, exactly as in `move_file`.
- It **copies**. The source mount is read-only and on another filesystem, so
  `rename(2)` would fail anyway — but the reason to want a copy is that
  `scratch-sweep.sh` stays the only thing that removes anything from scratch.
- Never overwrites, re-checked immediately before the rename, temp-then-rename
  so `ob sync` never sees a half-copied image.

**It adds no egress.** `vault-claude` still cannot reach the network. The bytes
are already on the NAS; this moves them between two mounts.

### The check that came from writing the test

A test asserting the two halves never share a surface **failed** when first
written: nothing in the code prevented it. Only the compose file and two
`.mcp.json` files kept `fetch_attachment` and `import_attachment` apart, which
is the kind of separation that survives until somebody consolidates a config.

`loadConfig` now refuses `MCP_FETCH=1` together with `IMPORT_DIR`. Together they
are "reach the open web, then write into the notes" in one session — the exact
combination the three-surface split exists to prevent — so it is a refusal to
start, not a warning.

### Rejected

**The human copies it over SMB.** Legitimate, zero code, and how it would have
worked by default. Rejected because the sweeper puts a deadline on remembering,
and nothing in the runbook said to.

**Mount `/scratch` read-write and let `move_file` address two roots.** Rejected
twice over: `rename(2)` fails across filesystems so it would become
copy-then-delete, which the shared contract in the root README forbids by name;
and a read-write mount ends "the sweeper is the only thing that deletes".

---

## Agent stamps in frontmatter

Added 2026-08-29. Notes written on an agent's behalf now carry
`agent-created`, `agent-modified` and `agent` in their YAML frontmatter, written
by `vault-mcp/stamp.go` and by `scripts/hook-stamp.sh` on a `PostToolUse` hook.

### Why, when the snapshot repo already records every agent write

Because the repo lives **outside** the vault — deliberately, and that is not
changing. Every consequence of that choice applies here: it is invisible from
Obsidian on the phone, which is where these notes are read, and a note that
leaves the vault through Sync arrives with none of its history. `git log
--author="Claude Code"` is still the better audit log and stays the thing to
reach for; it just cannot answer "did something write this?" while you are
standing in the note.

The two records answer different questions and are allowed to duplicate each
other. The repo says *what changed*. The stamp says *who last wrote this note*,
from inside the note.

### Why the names are not Claude-specific

The first draft used `claude-modified`/`claude-surface`. Rejected before it
shipped: Claude is not the only agent this vault will see, and the `fishing/`
stack is already a third writer. Baking one vendor into the key would mean a
second agent either inventing its own schema — leaving a vault where a query
answers for half the writes and silently misses the rest — or writing under a
name that misattributes its own work.

So the **key is generic and the identity is the value**: `claude-voice` for
`vault-mcp`, `claude-agent` for `vault-claude`. The registry of names is the
shared contract in the root [`README.md`](../README.md#shared-contract), which
is also where a future writer looks before inventing anything.

The cost is a collision risk `claude-*` would not have had: `agent` is a common
word, and a note that already uses it as a property loses that value on the
first agent write. Checked on 2026-08-29 — nothing in this vault used one.

### Line surgery, not a YAML library

Both implementations edit the three stamp lines and preserve every other byte,
rather than unmarshalling the block and re-emitting it.

Obsidian's own property editor round-trips this YAML. A marshal pass would
reorder keys, requote strings and drop comments in **every note an agent
touches** — a silent reformat of the corpus, arriving one note at a time and
indistinguishable from the agent having rewritten the note. Preserving bytes is
worth more here than schema correctness: this is a stamp, not a parser.

The trap it buys is that a leading horizontal rule is byte-identical to an
opening delimiter, so `---\nsome prose\n---` would take properties injected into
its text. Both implementations require the block to hold a **property**, which
is also the condition under which Obsidian parses it as properties. That errs
toward not recognising frontmatter, because the failure directions are not
symmetric: a redundant block above a rule leaves the note's content intact, and
properties inserted into prose do not.

Amended 2026-08-30. That test originally accepted a leading `#` line as
frontmatter too, for YAML comments — but a markdown ATX heading is the same
bytes, so a note opening with a rule above its title was read as frontmatter,
the stamp landed in its prose, and a rule further down the note became the
closing delimiter. Comments are now *skipped* on the way to a property rather
than standing in for one, which keeps the case they were exempted for (a block
opening with `# schema v2` above real properties) and drops the one they were
never meant to cover. A block of nothing but comments now reads as a rule, on
the same asymmetry: it has no properties to lose. An **empty** block still
counts, because `---\n---` is what Obsidian leaves behind when the last
property is deleted.

### The invariant: a stamp adds lines, it never edits the note

Added 2026-08-30, with the amendment above.

Everything else here decides *where* the three lines go. Both implementations
now also check *what changed* before writing, without reusing any of that
reasoning: every line the note arrived with must still be there, byte for byte
and in order, with the two lines the stamp rewrites in place — `agent` and
`agent-modified` — the only exceptions. `agent-created` is not exempt, because
it is never rewritten. When the check fails the note is written unstamped, and
the next agent write tries again.

This is belt-and-braces over reasoning that has now been wrong twice, and it
exists because of *how* the failure presents. A stamp landing in the body is
not a crash and not a diff anyone reads — it is one corrupted note in a vault
of hundreds, found weeks later or never, and by then the snapshot repo has
buried it under the writes that followed. A guard costs one unstamped note per
false alarm, which is the same price every other refusal in these two
implementations already pays.

It is deliberately not the same code as the placement logic. A check that
shared it would agree with it, including when it is wrong.

### Per-write, not batched at the end of a session

The rejected alternative was stamping in the existing `Stop` hook: walk
everything dirty in `vault.git` and stamp it before the commit. It reuses hook
plumbing that already exists and never touches a file mid-session.

It was rejected because at `Stop` there is no way to tell an agent's edit from a
Mac or phone edit that Sync landed while the session was running, and both are
dirty in the same working tree. It would stamp a note you wrote as agent work.
**A wrong attribution is worse than a missing one** — the whole point of the
stamp is that its presence means something.

The cost of the per-write choice is real: the hook changes the file after Claude
Code wrote it, so the agent's next `Edit` of that note can be refused as
modified-since-read. Mitigated by returning `additionalContext` telling it to
re-read. **If that proves noisy in practice, the escalation is the `Stop`
variant**, accepting the misattribution — or a `PreToolUse` hook recording
intent and `Stop` stamping only what it saw. Revisit this with evidence from a
live session, not in the abstract.

### What it deliberately does not do

`agent-created` is written once and never rewritten, so it means "an agent made
this note" rather than "an agent touched it recently" — which `agent-modified`
already says. And nothing clears any of the three when you edit the note by hand
afterwards. `agent-modified` therefore means *an agent last wrote this note*,
never *this content is the agent's*. Reading it as the latter is the failure
mode to guard against, because it is the reading that makes the stamp feel
useful.

---

## Giving the agent a move

Added 2026-08-30.

`vault-claude` could not move a note. Not "it was awkward" — there was no
expressible way to do it, and this was the single most common thing the agent
was asked to do and could not: triage that ends in *"file this under
Projects/"*. Claude Code has no move tool. `Bash` is denied, so `mv` is out. The
only remaining shape is `Read` the note, `Write` it at the new path, and leave
the original behind, because nothing in this vault may delete a file. The
failure was worse than a refusal: the agent would report a note filed and leave
two copies, which `ob sync` then propagated to every device.

### The obvious fix, and why not

**Allow `Bash(mv:*)` and nothing else.** Rejected, and it is worth being precise
about why, because "just allow one command" reads as proportionate.

Claude Code's own documentation is explicit that Bash command matching is not a
security boundary — argument parsing is best-effort against a shell whose
grammar allows substitution, chaining and expansion in places a prefix match
does not look. That is a fine caveat in a repo you wrote; it is the wrong
foundation here, where the agent reads clipped articles and forwarded mail and
[ARCHITECTURE.md#trust-boundary](ARCHITECTURE.md#trust-boundary) rests entirely
on `Bash` being *absent*, not narrowed. Worse, `deny` beats `allow` in Claude
Code, so allowing one `Bash` form means **removing the blanket `Bash` deny** and
replacing an allow-list posture with a blacklist. The blanket deny is one line
whose failure mode is a missing feature; the blacklist's failure mode is a
breach, discovered later.

**A small mover written in Node, shipped in the agent image.** Rejected. It
would be the *fourth* implementation of the vault's deny list, after
`vault-claude-settings.json`, `vault.go` and `hook-stamp.sh` — and the shared
contract in the root README already names drift between those as the failure
that turns a convenience feature into a persistent compromise. A mover is not
five lines either: it needs the traversal guards, the symlink check, the
`.md`-only rule, the concurrent-writer re-check and the atomic rename, all of
which exist and are tested in `vault.go`.

**Point the agent at the deployed `vault-mcp` over the compose network.**
Rejected. Different stack, and it would mean either authenticating the agent
against the OAuth flow or opening an unauthenticated listener on a network the
whole point of this design is not to have. It would also apply `MCP_EXCLUDE` to
the agent, hiding the Inbox from the surface whose job is triaging it.

### What was built

`vault-mcp` gained a `move_file` tool and a `-stdio` flag, and the agent image
builds that Go source into `/usr/local/bin/vault-mcp`. Claude Code runs it as a
local MCP server declared in `<vault>/.mcp.json`, which
`install-settings.sh` materialises from `vault-claude-mcp.json` alongside the
tool policy, on every agent start, for the same reasons as
[that decision](#shipping-the-tool-policy-with-the-image).

One binary, two surfaces. The deny list has exactly one enforcement in code, so
a rule added to it cannot be honoured for voice and missed for the agent. The
full comparison of what `-stdio` changes is in
[`../vault-mcp/README.md`](../vault-mcp/README.md#two-surfaces-one-binary); the
decisions inside it worth recording here:

- **One tool, not seven.** The agent already has `Read`, `Write`, `Edit`, `Glob`
  and `Grep`. Serving it `edit_note` too would be a second way to write a note —
  one that fires no `PostToolUse` hook, so those writes would land *unstamped*
  while looking to the model like any other edit. Adding a tool to that surface
  is a decision about the stamp contract, not a convenience, and
  `stdio_test.go` pins the list at exactly `move_file`.
- **The deny list applies to the source, not just the destination.** Denying
  only where a note lands would leave "move `CLAUDE.md` to `Archive/old`" as a
  way to revoke the vault's standing instructions without ever writing to the
  file.
- **`rename(2)`, not copy-then-delete.** A copy doubles the note on disk for as
  long as the delete takes to land, and `ob sync` is reading the directory
  continuously — it would propagate the duplicate and then the deletion, which
  is the shape of a sync conflict rather than a move.
- **`<vault>/.mcp.json` is write-denied to the agent**, at any depth. An entry
  there is an arbitrary command every future session executes: the `AGENTS.md`
  compromise with the shell the agent does not otherwise have.
- **Snapshots stay with the hooks.** The stdio server runs with
  `MCP_SNAPSHOT=0`; the session's `Stop` hook commits the move like every other
  agent write. The connector needs its own commits because it fires no hooks —
  the agent does not.

### Attachments, added the same day

The first cut moved notes only, because `vault-mcp` had been markdown-only since
it was written and the rule looked like part of the trust boundary. It is not:
it is a rule about what the server may **create**, inherited from a connector
whose whole job is writing notes. Moving an image trips none of the reasoning
behind it — a move creates nothing, it relocates bytes a human already put in
the vault — and half the filing a vault actually needs is attachments, which
Claude Code cannot author *at all* (Write emits text, so there is no way to
produce the bytes even when it can see the file).
Notes-only made the tool useless for the harder half of the job.

So `move_file` may move attachments, and it is the only operation here that may
touch a file which is not a note. Four rules keep the exception narrow, and each
one is a decision:

**An allow list, not "anything not denied".** `attachmentExts` is media, PDFs,
EPUB and `.canvas`. A `.sh`, a `.js`, a `.json` in a vault is not an attachment,
and relocating executable- or configuration-shaped files is capability with no
use case behind it. The rejected alternative — allow every extension, rely on
the path denies — would have been defensible (the agent has no shell to run
anything with) but it makes the tool's blast radius a function of what is *not*
on a list, which is the posture this repo rejects everywhere else.

**The extension may not change.** A move relocates, never converts. Beyond
stopping `scan.png` → `scan.md`, this is what prevents the note path and the
attachment path from being chosen independently at each end of a move — which is
exactly where a "stamped YAML into a PNG" bug would live.

**No stamp, and the gap is recorded rather than closed.** An attachment cannot
carry frontmatter, so an attachment move breaks the shared contract's promise
that every agent write is attributed in the file itself. The snapshot commit and
the audit line are what remain, and with `EXCLUDE_ATTACHMENTS=1` only the audit
line. A **sidecar `.md` per attachment** was considered and rejected: it doubles
the file count in the vault to describe files Obsidian already lists, and Sync
would carry the sidecar and the image as two unrelated objects that can arrive
apart — a stamp that says a file moved, next to a file that did not. The honest
version is a documented exception in the root README's shared contract.

**No link rewriting.** Obsidian updates `![[scan.png]]` across the vault when
you move an attachment in the app; this does not. With the default *shortest
path when possible* link format an embed still resolves after a move — it
matches on filename — so the exposure is renames, and vaults configured for
relative or absolute paths. Rewriting backlinks means editing notes the caller
never asked to change, which is a larger decision than this tool and belongs in
its own one.

### What this costs

The agent image now has a Go build stage and builds from the **repository root**
rather than `obsidian-vault/`, so a change under `vault-mcp/*.go` rebuilds both
images. That coupling is deliberate and is the point: the alternative was two
implementations that could disagree. It is also the thing most likely to
surprise someone later, which is why the Dockerfile, the workflow and
`.dockerignore` all say so at the top.

This does not widen [the trust boundary](ARCHITECTURE.md#trust-boundary). The
new capability is one tool that renames one markdown file, with no socket, no
network call and no credential mounted. Prompt injection's third leg — egress —
is exactly as absent as it was.

---

## Shipping the tool policy with the image

Added 2026-08-29, when adding the stamping hook made the cost obvious.

`vault-claude-settings.json` was hand-copied to `<vault>/.claude/settings.json`
as a setup step. The hook *scripts* it points at have always shipped in the
image, so a change touching both — which the stamping hook was — deployed as two
halves with a manual step between them. Forget the copy and the repo describes a
policy the running agent does not have, with nothing failing.

That is worse than an ordinary stale-config problem, because this file is where
`Bash`, `WebFetch` and `WebSearch` are denied. A deny rule added upstream did
nothing at all until somebody remembered a `cp`.

So the file now ships in the image beside those scripts, and
`scripts/install-settings.sh` writes it into the vault before the agent starts.
The repo is the source of truth; the copy in the vault is a materialisation of
it. `VAULT_SETTINGS_MANAGED=0` pins a hand-edited file instead, and then drift is
reported rather than corrected — including a security fix you will not receive.

**Two files since 2026-08-30**, installed by the same script under the same
flag: the tool policy, and `vault-claude-mcp.json` → `<vault>/.mcp.json`, which
registers the agent's move tool. One flag rather than two because they are one
setting split across two files — the policy's allow rule for
`mcp__vault-tools__move_file` and the server name in `.mcp.json` have to agree,
and pinning one while the other updates produces an agent told it may use a
server that does not exist. Their *failures* are not symmetric, though, and
`agent.sh` treats them differently: a missing policy is a refusal to start,
because that agent is unsafe; a missing `.mcp.json` is a loud warning, because
that agent is merely unable to file notes.

### Why a copy, and not a link or a mount

Both were considered and are worse here for the same underlying reason: the
vault is not just a directory the agent reads, it is the work tree `vault-sync`
commits and Obsidian browses.

**A symlink** would be committed as a symlink and restored as a dangling one.
Worse, it cannot work at all: the repo checkout is not mounted into these
containers, deliberately — an agent that can reach the checkout can edit the
policy that constrains it.

**A bind mount** of the repo file over `/vault/.claude/settings.json` reads well
in compose and splits the truth in two: the agent would obey the mounted file
while the snapshot repo, an SMB browse, and a restore all saw whatever was on
disk underneath. Mounting it into every service instead makes the commit agree
and leaves the host copy stale. Docker also creates missing mount points as
root, which on a vault owned by `1002:100` is the
[known trap](#traps-found-while-building) about ownership, arriving in the one
directory that holds the agent's permissions.

A copy keeps exactly one file on disk. Everything downstream —
`ARCHITECTURE.md#snapshots`' promise that the policy restores with the notes,
`preflight.sh`'s comparison against the checkout, the backup bundles — keeps
working unchanged.

### What this moved, not removed

`preflight.sh` used to **fail** when the file was missing, which was the
enforcement: run it before starting, as the README says, and you could not
start an unconstrained agent by accident. That check is now a warning, because
an absent file before the first `docker compose up -d` is expected.

So the enforcement had to move into the container, and `agent.sh` now **exits
rather than starting an agent with no tool policy**. That is a change of
behaviour worth stating plainly: it used to warn and start. Warning made sense
while a human was expected to copy the file by hand and might be mid-setup;
once installing it is automatic, reaching that line means the install genuinely
failed, and the response to a failed security control is not to proceed without
it. `restart: unless-stopped` turns it into a container that visibly
crash-loops instead of an agent quietly holding `Bash` and the web tools over a
corpus of clippings. `VAULT_SETTINGS_MANAGED=0` is unaffected: that path skips
*installing*, and the pinned file is present, so the check passes.

The enforcement is therefore stronger than what it replaced — it happens on
every start rather than on every remembered preflight — but it is worth being
explicit that a check was downgraded in one place before being restored in
another. What `preflight.sh` still uniquely answers is whether the copy on the
**host** matches this checkout, which the container cannot tell you: that copy
is the one committed, bundled and restored.

---

## The dashboard and the Docker socket

**2026-08-29. This reverses a position taken three times in this repo, and the
reversal is narrower than it looks.**

`vault-cron` refuses `/var/run/docker.sock`. So does
[`docker-compose.yml`](docker-compose.yml), so does
[`ARCHITECTURE.md`](ARCHITECTURE.md#services), and all three say the same thing:
anything holding that socket has effective root on the NAS, and a cron job that
runs `backup.sh` over its own volumes gains nothing from it. **That reasoning is
untouched and those containers still refuse it.** What changed is that a new
stack wants something none of them wanted.

### What changed the answer

The ask was a page that shows what is running, what is out of date, and lets you
update and restart it from a phone. The first two thirds need only reads. The
last third does not exist without something on this host being able to pull an
image and recreate a container, and on Container Station that means the daemon
socket. There is no version of "press a button and it deploys" that avoids it.

So the decision was not *"should a container hold the socket"* in the abstract.
It was: hold it in one small container with a closed verb list, or do not build
the feature.

### Options assessed

**A read-only socket proxy.** The usual answer, and correct for a dashboard that
only reads. It does not survive the requirement: deploying needs image pulls and
container creates, so a proxy permissive enough to allow it is a proxy that
allows the dangerous half. A proxy allowlist would also be expressed in Engine
API verbs and paths — strictly weaker than one expressed as `homelab deploy
vault-mcp`, and a second place to keep correct. Rejected.

**A privileged helper driven by a file or a queue** — the web container writes a
request somewhere, a socket-holding container picks it up. Same trust boundary
as an HTTP call between two containers on a private network, plus a spool
directory to reason about and no request/response. Rejected as more moving parts
for the same guarantee.

**Off-the-shelf (Homepage, What's Up Docker, Portainer).** Portainer is the
socket exposure with a much larger surface and its own auth to keep patched.
Homepage plus WUD is two config-heavy services that still want the socket, know
nothing about this repo's health signals — snapshot freshness, voice writes,
`.env` mode — and would sit alongside `bin/homelab` rather than driving it.
Rejected: more surface, less fit.

**Do not build it.** Genuinely on the table. What tipped it is that the socket
was going to be reachable from the LAN either way the moment a deploy button
existed, and the *shape* of that reachability was the only real variable.

### What was actually built

Two containers off one image. The half that renders HTML and is published on the
LAN holds no socket and no checkout.

> **Superseded 2026-09-01 on the publishing detail only.** That half is now
> published on **loopback**, reached through `tailscale serve`, and there is no
> LAN path to it at all. The split it describes is unchanged and is still the
> design. See
> [Updating services: identity at the door, digests in git](#updating-services-identity-at-the-door-digests-in-git).
 The half that holds the socket publishes no
port, mounts the checkout read-only, and accepts only a verb from a closed list
plus names validated against what is on disk and running. It shells out to
`bin/homelab`, so the dashboard is a front-end for the sanctioned entry point
rather than a second way to deploy — which also means `deploy.sh`'s delegation,
provenance verification and the re-pair warning keep working instead of being
reimplemented and drifting.

**This is defence in depth, not containment,** and it is worth being blunt about
that. A full compromise of the web container still reaches verbs that deploy
things. What the split buys is that the reachable surface is `deploy vault-mcp`
rather than any Docker API call: no `docker run -v /:/host`, and no `exec` into
`vault-claude` to walk off with the Claude OAuth token that
[the trust boundary](ARCHITECTURE.md#trust-boundary) exists to protect.

The full design is in [`../dashboard/README.md`](../dashboard/README.md#trust-boundary).

### The cost, stated plainly

The NAS now runs a container with effective root on the host, reachable
indirectly from anything on the LAN. Before this stack, the worst a LAN attacker
could do to these containers was nothing at all — no stack published a port.
That is a real reduction in the security of this host, accepted knowingly in
exchange for a feature, and it is the reason the mutating half needs a token,
the reason the checkout is mounted read-only, and the reason this section exists
rather than a line in a commit message.

> **Superseded 2026-09-01, and the cost is smaller than this paragraph states.**
> The reachable population is no longer "anything on the LAN" but "anything on
> the tailnet": the port is published on loopback behind `tailscale serve`. And
> there is no token — the mutating half is gated by the tailnet identity Serve
> attaches, checked against an allow list. The read-only checkout is unchanged
> and is still a reason. This paragraph is the repo's statement of the accepted
> cost of holding the socket, so it is corrected here rather than left to read as
> current. See
> [Updating services: identity at the door, digests in git](#updating-services-identity-at-the-door-digests-in-git).

### Publishing a port: the second reversal

The root README said no stack publishes ports and nothing needs configuration on
the UniFi gateway. The second half is still true — this port was on the LAN and
the tailnet (it is now loopback-only; see the note below), and the gateway
remains a bystander either way. The first half is now false,
and the README says so.

`8088` on the host, rather than a Tailscale sidecar like `vault-mcp`'s. The NAS
has been its own tailnet node since 2026-08-12, so a published port already
answers on `kyles-nas.tail3df177.ts.net` as well as on the LAN address. A second
node would have bought TLS and tailnet identity at the cost of another auth key,
another state directory that must not be lost, and another node to re-authenticate
— for a page that is deliberately not on the internet. The shared token is the
mitigation instead.

> **Superseded 2026-09-01.** There is no shared token, and this port is no longer
> on the LAN — it is published on loopback and reached through `tailscale serve`.
> The sidecar reasoning above is untouched and still correct about sidecars; what
> changed is that Serve on the host node buys the TLS and the identity at none of
> those three costs, which is an option this paragraph did not assess rather than
> one it got wrong. See
> [A correction to the record, not a reversal](#a-correction-to-the-record-not-a-reversal).

**Deliberately not a Funnel.** `vault-mcp` is on one because Claude's voice mode
calls connectors from Anthropic's cloud and there is no outbound-only path
available. Nothing about a status page forces that, and a page enumerating every
container and image version on this host is not the second thing to publish.

## Updating services: identity at the door, digests in git

**2026-09-01. Assessment and phasing. Phases 0 and 1 built; 2 and 3 decided but
not yet built, and this section says which is which.**

Two complaints, one plan. Neither is about the buttons themselves:

1. **Auth is a pasted token.** `DASH_TOKEN` is a shared secret that never
   expires, cannot be revoked per device, and travels over plain HTTP.
2. **Updating a service is still an SSH chore**, and one with a silent failure
   in it. The dashboard can `deploy` but not `update`, because the checkout is
   mounted read-only. So pressing deploy pulls a **new image against whatever
   compose file is already on disk**, and nothing on the page says the checkout
   is behind. That is not a missing feature; it is a wrong answer delivered
   confidently.

### Why there is a checkout at all

Asked directly while planning this, and worth writing down because the answer
constrains everything below: *why clone the repo onto the NAS instead of just
running the images?*

Because `docker compose` needs host-side files that no image can supply.
Compose running inside a container is a **client** of the host daemon: it
resolves relative bind mounts against the project directory as the client sees
them and hands the daemon those absolute host paths. `vault-mcp` mounts
`./funnel.json`; `obsidian-vault` mounts `vault-claude-settings.json`. Those
files, the compose files themselves, the gitignored `.env`s and `bin/homelab`
with its per-stack `scripts/*.sh` all have to exist as real paths on this host.
This is the same property `dashboard/.env.example` explains at length about
`REPO_HOST_PATH`, seen from the other side.

So the checkout stays. What that reframes is the security question. The risk was
never that the checkout is *writable* — it is that **the checkout decides what
runs**. The control that follows is not "never write it" but "only ever
fast-forward it to a signed commit on `origin/main`", which is a far narrower
thing than the read-only mount was protecting against in the abstract.

### Auth: options assessed

**Tailscale Serve identity on the host node. Chosen.** Bind the published port
to loopback, put `tailscale serve` in front. Serve terminates TLS with a real
Let's Encrypt certificate for `kyles-nas.tail3df177.ts.net` and injects
`Tailscale-User-Login` on tailnet requests; the dashboard checks that against an
allow list. No secret to type, no cookie to sign, no authorization server, and —
decisively — **no internet dependency to sign in**. That last point is not a
detail: this is the tool you open when something is broken, and an auth path that
needs a third party to be reachable fails exactly when it is needed.

**WorkOS AuthKit, as `vault-mcp` uses. Rejected for now, not forever.** It is
the consistent answer and it brings real MFA and revocation. But over plain HTTP
it is *worse* than the token — an authorization code and a session cookie in
cleartext on the LAN, and `auth.go` already cannot set `Secure` for that reason.
So OIDC needs TLS in front of it to be safe; and once TLS is provided by Serve,
Serve has already supplied the identity, leaving ~200 lines of security-critical
code doing what a header did. If access is ever needed for someone not on the
tailnet, the login allow-list check is the seam it slots into.

**Auth0. Rejected, and for a different reason than last time.**
[`vault-mcp/README.md`](../vault-mcp/README.md#why-not-an-authorization-server-of-our-own)
rejected Auth0 because it gates DCR controls behind Enterprise — an MCP-client
concern that does **not** apply to a browser app, so that objection is void
here. What stands is that the tenant for this house is WorkOS, and adding Auth0
would mean two identity providers for one person, plus the same internet
dependency as above.

**Local HTTPS with a private CA. Rejected.** A certificate for `kyles-nas` on
the LAN means either a CA installed on every device or public DNS with DNS-01
ACME. Serve gets a genuine public certificate for free, and the tailnet already
has HTTPS certificates enabled because the `vault-mcp` Funnel requires them. The
prerequisite is already paid for.

**Keep the token, add TLS only. Rejected** as the smallest change that leaves
the actual complaint unaddressed.

### A correction to the record, not a reversal

[Publishing a port: the second reversal](#publishing-a-port-the-second-reversal)
rejected tailnet identity on the grounds that a second node costs "another auth
key, another state directory that must not be lost, and another node to
re-authenticate". That assessment is about a **Tailscale sidecar**, like
`vault-mcp`'s, and it is still correct about sidecars.

`tailscale serve` on the host node costs none of those three. The host node has
existed since 2026-08-12, before that paragraph was written, so this is not the
earlier decision turning out to be wrong — it is an option that was not on the
list. Recorded here rather than by editing that section, because the sidecar
reasoning is still the right answer to the question it was asked.

### The header is only a credential if nothing else can set it

**This change inverts the security posture and that has to be said plainly.**
Today an attacker needs a 32-byte secret. After it, anyone who can reach the
listener directly can send `Tailscale-User-Login: kyle@example.com` and is root
on this NAS. The proxy is the whole control, so three things stop being
preferences and become assertions:

1. **The loopback bind.** `127.0.0.1:${DASH_PORT}:8080`, not `${DASH_PORT}:8080`.
2. **The agent asks the daemon whether that is actually true**, on every
   mutating and sensitive action, and refuses them all if any of this stack's
   host bindings is not loopback. A compose file is exactly the kind of thing
   this dashboard makes easy to change, so the premise is checked against
   reality rather than assumed from the file that states it. Getting the publish
   wrong therefore *breaks* the dashboard rather than opening it.
3. **Refusing any request carrying `Tailscale-Funnel-Request`.** Funnel traffic
   gets no identity headers *and* arrives by the same loopback path Serve uses,
   so nothing else would catch it. Nothing funnels this port today; the check is
   what keeps that true.

**The obvious version of (2) does not work, and that is worth recording so it is
not "simplified" back in.** The first design checked `r.RemoteAddr` in the web
role against a trusted CIDR. Inside a container behind Docker's port publishing
that address means nothing reliable: with the userland proxy every client
appears as the bridge gateway, and with iptables DNAT an external client's real
address is preserved. The CIDR you would have to trust to accommodate the
gateway is RFC1918 — which is also where the LAN is. Asking the daemon how the
port is bound is the version of the question that has an answer, and the process
that can ask it is the agent, not the web role.

Two ways of failing, deliberately not the same:

- **Positively published beyond loopback** — refuse, with no override. There is
  no setting for it, because the cheapest way to honour a rule is to make the
  wrong thing unexpressible.
- **Could not be determined** (daemon unreachable, own containers not found) —
  refuse, unless `DASH_ALLOW_UNVERIFIED_EXPOSURE` says otherwise out loud. That
  knob exists so "I cannot tell" is distinguishable from "you are exposed", and
  it deliberately cannot unlock the case above. Asserted in `agent_test.go`.

`dashboard/scripts/preflight.sh` checks the same things ahead of time, plus — run
over SSH — the one the agent cannot see: whether `tailscale serve` is running at
all. From the dashboard's own preflight button it runs inside a container with
neither the tailscale binary nor `getcfg`, so it skips exactly that part and says
so; the button is the weaker of the two runs.

A fourth follows from removing the cookie: `SameSite=Strict` no longer protects
anything, because the browser attaches proxy identity to cross-site requests as
readily as to same-site ones. **The `X-Homelab-Action` header is now the only
CSRF defence rather than the second of two**, which is a change in what that
check is load-bearing for, even though the code is unchanged.

Blank `DASH_ALLOWED_LOGINS` **refuses to start**, with
`DASH_ALLOW_ANY_TAILNET_USER=1` as the way to say "every account on this tailnet"
out loud. That is the `OAUTH_ALLOWED_SUBJECTS` / `OAUTH_ALLOW_ANY_SUBJECT` shape
from `vault-mcp`, for the reason recorded there: the empty value must never be
the permissive one.

### Removing DASH_TOKEN rather than keeping it for break-glass

Tempting to keep as a fallback, and refused. `vault-mcp` learned this the
expensive way: its one real hole was in the auth path nobody exercised, and every
unit test passed over it. A second scheme means keeping that class of code
correct forever with nobody testing it in anger.

The fallback is SSH, which
[`dashboard/README.md`](../dashboard/README.md#it-will-not-deploy-itself) already
names as the only way this stack gets deployed anyway. If `tailscaled` is down
the dashboard is unreachable, but so is tailnet SSH — and QNAP's `sshd` keeps
running for the LAN either way, which is
[why Tailscale SSH was rejected](#tailscale-ssh-considered-and-rejected). The
break-glass path is intact and it is the one that was always there.

Read-only mode survives as an explicit `DASH_READ_ONLY=1` rather than as "the
token is blank", which is the same argument in a third place: a mode worth
supporting is worth stating.

### Deploys pinned by a digest committed to git

**Decided, not yet built.** `.env` is gitignored, so the intended digest cannot
live there and today nothing records which digest a stack was on. That is why
there is no rollback.

A tracked `<stack>/image.lock`, written by CI after the attestation step, in
`KEY=value` form so `bin/homelab`'s existing `env_get` reads it with no new
parser. `homelab deploy` resolves `--to <digest>` → `image.lock` → `.env`'s
`IMAGE`, passing the result in the environment rather than rewriting `.env`.
Rollback becomes `homelab deploy <stack> --to <digest>` over digests read out of
`git log -- <stack>/image.lock`, and the update badge gains a third state:
*running != lock* is "deploy pending", *lock != origin* is "checkout behind".

Each workflow's path filter must exclude its own lock file or the commit
retriggers the build. Path filters handle that cleanly; a `[skip ci]` marker in
a commit message does not deserve to be load-bearing.

**This also closes the provenance asymmetry.** The dashboard skips
`gh attestation verify` because the image ships no `gh` — currently the one place
where pressing the button is weaker than typing the command. If `image.lock` is
written only by CI and the commit is signed, verifying that signature proves the
digest came from the workflow without `gh` in the image. It is a **different**
guarantee, trusting GitHub's signing rather than sigstore, and `gh attestation
verify` stays the stronger check on the SSH path. Do not let the docs blur those.

### Letting the dashboard update the checkout

**Decided, not yet built, and it reverses**
[`dashboard/README.md`](../dashboard/README.md#what-it-cannot-do): *"a web request
that can rewrite it is a web request that can change what every button does."*

That sentence stays true. What changes is the **capability**, not the truth of
the sentence. A third container off the same image, `-role=updater`, mounting the
checkout read-write and holding **no Docker socket**, with exactly two verbs:
`preview` (`git fetch` plus `HEAD..origin/main`) and `apply`
(`git merge --ff-only origin/main`). Enforced in Go rather than in git config:
the remote URL must match exactly, the branch must match, fast-forward only, a
dirty working tree refuses, and the target commit's signature is verified against
an allowed-signers file in the repo.

Two details decide whether it is safe rather than merely constrained:

- **The reviewed sha is echoed back.** `apply` carries the commit the human was
  shown and is refused if `origin/main` moved since. That is the TOCTOU guard,
  and it is what makes the press mean "I reviewed this" instead of "I pressed a
  button labelled update".
- **The agent owns the lock.** Route web -> agent -> updater so an update cannot
  race a deploy through the mutex `agent.go` already has.

The updater is the container that ingests repo content, so it is locked down
hardest: non-root, read-only root filesystem apart from the checkout, dropped
capabilities, egress to GitHub only, and `core.hooksPath=/dev/null` with
`GIT_CONFIG_NOSYSTEM=1` — `.gitattributes` filters and `diff.external` are
repo-controlled routes to execution.

**The cost, stated plainly.** A compromise of the web container can now advance
the checkout to whatever is already on `origin/main`. It cannot inject code; it
can only make this host take upstream sooner than a human would have. That is
genuinely narrower than arbitrary write — and provenance-attested images already
mean a GitHub compromise reaches this host, so the marginal exposure is smaller
than the sentence it reverses implies. Smaller is not zero, which is why the
signature check and the reviewed sha are both in the design rather than one of
them.

### Phasing, and why this order

0. **Visibility.** Show how far behind the checkout is. No new privilege, and it
   fixes the wrong-answer-delivered-confidently case on its own.
1. **Serve identity.** Must land before 3, because 3 hands the web surface more
   power and the door should be right first.
2. **Digest lock.** Independent of 1.
3. **Updater container.**


---

## The connectors nobody configured

**2026-09-05.** The research surface shipped, the stack came up, and the first
question asked of the new agent was what tools it had. It answered:
`vault-tools` with `fetch_attachment` and `move_file`, and then Gmail, Google
Drive, Notion, Deep WIKI and **kylejs Obsidian Vault**.

The last of those is this repo's own `vault-mcp` connector. The agent built to
have the open web and nothing else had the notes, over HTTPS, with write access.

### Why every existing control missed it

Claude Code signed into a claude.ai subscription fetches **that account's**
connectors at startup and serves them as MCP tools. Not the project's, not the
container's: the account's. And every control in this repo was aimed somewhere
else.

- **The mounts.** `vault-research`'s compose entry avoids the `*common` anchor
  specifically so the vault cannot arrive by inheritance, and `research.sh`
  refuses to start if `/vault`, `/snapshots` or `/backups` appears. A connector
  needs no mount. It is an outbound HTTPS call from a process that had one
  already.
- **The deny list.** `vault-claude` denies `Bash`, `WebFetch` and `WebSearch`,
  and that list is where the "no way out" guarantee lives. `deny` matches tool
  names this file knows about. Connectors arrive *after* `settings.json` is
  read, named by the account, and no `allow` or `deny` rule applies to them.
- **`preflight.sh`.** It checks anchoring, inert `Write(path)` rules and a
  misplaced `additionalDirectories`. It could not check a key nobody knew to
  set.

So the table in [ARCHITECTURE.md](ARCHITECTURE.md#three-surfaces-three-different-mitigations)
had two false cells. `vault-claude`'s egress was not **none**, because
`mcp__Gmail__send_message` sends mail, and that was true from the day the agent
first logged in. `vault-research`'s private data was not **none** either. One
surface with all three legs, and the other with a guarantee it had never held.

### What was rejected

**Revoking the connectors on the claude.ai account.** It fixes both agents and
nothing else, and it takes Gmail and Drive away from every ordinary Claude
session the account is used for. The problem is not that the connectors exist,
it is that these two containers inherit them.

**`deniedMcpServers` per connector.** Blocking `"claude.ai Gmail"` by name works
and is what you would reach for to keep one connector while dropping another.
Neither agent should have *any* of them, and a list by name is a list that is
missing whichever connector gets added next month.

**A permission `deny` on `mcp__Gmail__*`.** The pattern that looks right and is
the trap this repo has already fallen into twice. Rules are matched against
tools the session has; a wildcard covering a server that has not been named
denies nothing, silently, and reads like protection in the file. Same failure as
`Read(/snapshots/**)`.

### What was done

`disableClaudeAiConnectors: true` in both policy files, and
`ENABLE_CLAUDEAI_MCP_SERVERS=false` in `docker-compose.yml`.

The key is **top level**, beside `env`, not inside `permissions`. Inside
`permissions` it is an unrecognised key that is ignored without a word, which is
exactly what `additionalDirectories` did in this same file's first draft.
`preflight.sh` now fails distinctly for that case rather than reporting the key
missing, because the two have different fixes.

Both mechanisms, not one. The policy files are installed by
`install-settings.sh` at every container start and can be pinned off with
`VAULT_SETTINGS_MANAGED=0`; the environment variable holds when the file does
not. In compose it sits on the `*common` anchor rather than in `vault-claude`'s
own `environment:` block, because a service-level `environment:` **replaces**
the anchor's map rather than extending it, and would have taken `VAULT_DIR` with
it.

### The limit, stated because it is easy to miss

The setting covers connectors Claude Code **fetches for itself**, which is what
a terminal session in a container does. A cloud host or the desktop app can
deliver connectors in-process instead, and neither the key nor the variable
touches those ([docs](https://code.claude.com/docs/en/mcp)). `preflight.sh`
asserts the configuration, which is not the same as asserting the outcome. The
check that settles it is `/mcp` in a live session, with `vault-tools` as the
only server listed.

### The general lesson

This repo's whole design is "each surface is missing one leg", and it enforces
that with mounts, deny lists and a startup tripwire — three mechanisms that all
describe **this machine**. A connector is granted somewhere else entirely, to an
account, and arrives without touching any of them. Anything that reaches a
session through the login rather than through the filesystem is invisible to
every control here.

The find took one question to a live agent: *what tools do you have?* Nothing in
the repo answers it, because everything in the repo describes what it configured
rather than what arrived. Ask a new surface that question before trusting any
paragraph written about it.

---

## Open questions

- **Monitoring.** Still the largest remaining gap, but narrower since
  2026-08-29. HBS's "job fails" notification covers the Drive leg, and the
  [`dashboard`](../dashboard/) stack now makes a stopped `vault-sync`, a stale
  snapshot and an out-of-date image visible at a glance. That is *pull*, not
  alerting: it tells you when you look. Nothing yet pushes, so `vault-cron`
  stalling or the Claude login expiring still waits on someone opening the page
  — and that last one silently stops the agent, with its three-day warning
  appearing only inside a tmux nobody watches.
- **Remote Control pairing model.** How long a pairing stays valid, whether it is
  single-use, and what someone else obtaining the URL would get. Worth
  understanding before pairing over an untrusted network.
- **Remote Control is young.** Operational dependence on a new feature carries
  deprecation risk; the plugin and SSH routes stay documented for that reason.
- **Tailscale ACLs.** If unconfigured, every tailnet device reaches the NAS.
- **Anthropic's bridge sees the session** — inherent to Remote Control, and the
  same trust already extended by using Claude at all, but it means vault contents
  transit Anthropic's infrastructure.
- **Whether `MCP_EXCLUDE` stays aligned with reality.** It names folders. Rename
  one in Obsidian and the entry matches nothing; the server warns at startup, but
  only where someone reads the log, and the monitoring gap above is why that is
  thin. Paste a forwarded email into a project note and the exclusion never
  applied in the first place.
- **Four trust relationships.** Your notes exist on the NAS, Anthropic's
  infrastructure, Obsidian's sync servers, and Google Drive. Inherent to the goal,
  but it should be a decision rather than a side effect — particularly where
  notes concern other people who did not opt in.
- **Git history is permanent.** A password pasted into a note once appears in
  every subsequent bundle forever, even after the note is deleted. The only
  remedy is rewriting history and re-issuing every bundle.

---

## References

- Claude Code Remote Control — https://code.claude.com/docs/en/remote-control
- Claude Code hooks — https://code.claude.com/docs/en/hooks
- Claude Code settings — https://code.claude.com/docs/en/settings
- Authentication / `setup-token` limits — https://code.claude.com/docs/en/iam
- Obsidian CLI & Headless — https://obsidianmd-obsidian-help.mintlify.app/extending/obsidian-cli
- `obsidianmd/obsidian-headless` — https://github.com/obsidianmd/obsidian-headless
- supercronic — https://github.com/aptible/supercronic
- Independent build of this architecture — https://www.varokas.com/remote-control-obsidian-vault-claude-code/
- `Vault Companion for Claude` — https://community.obsidian.md/plugins/vault-companion-for-claude
- `.claude.json` concurrent-write corruption — [#28847](https://github.com/anthropics/claude-code/issues/28847), [#29217](https://github.com/anthropics/claude-code/issues/29217), [#29153](https://github.com/anthropics/claude-code/issues/29153)
- Self-hosted LiveSync — https://github.com/vrtmrz/obsidian-livesync
- `jimprosser/obsidian-web-mcp` — https://github.com/jimprosser/obsidian-web-mcp
- Vault conventions for Claude Code — https://github.com/AgriciDaniel/claude-obsidian
