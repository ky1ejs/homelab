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
hairpin NAT, dynamic DNS — is machinery for accepting inbound connections, and
this system never accepts one. Tailscale is only for *your own* admin access.

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
the agent follows, and `Write(./.claude/**)` is denied precisely so an injected
note cannot rewrite the agent's own instructions. An ordinary vault folder
reopens that: a hostile clipping could have the agent author a skill, inherited
by every later session. Mitigation is to deny the agent writes there:

```json
"Write(./Extras/Claude Skills/**)",
"Edit(./Extras/Claude Skills/**)"
```

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
its text. Both implementations require the first non-empty line of the block to
be a key or a comment — the same condition under which Obsidian parses it as
properties. That errs toward not recognising frontmatter, because the failure
directions are not symmetric: a redundant block above a rule leaves the note's
content intact, and properties inserted into prose do not.

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

`vault-mcp` gained a `move_note` tool and a `-stdio` flag, and the agent image
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
  `stdio_test.go` pins the list at exactly `move_note`.
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
`mcp__vault-tools__move_note` and the server name in `.mcp.json` have to agree,
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

## Open questions

- **Monitoring.** The largest remaining gap. HBS's "job fails" notification
  covers the Drive leg. Nothing covers `vault-sync` dying, `vault-cron` stalling,
  or the Claude login expiring — and that last one silently stops the agent, with
  its three-day warning appearing only inside a tmux nobody watches.
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
