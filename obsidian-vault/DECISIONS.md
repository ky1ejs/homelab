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
  `supercronic -test`. A failing `RUN` is much cheaper to debug than a container
  that starts and silently does nothing.

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
node, but behind a subnet router the QNAP is just an IP in a CIDR. Tailscale SSH
also only works to a real node, and that is the deploy path.

**Done 2026-08-12, with two parts still open.** This section had described the
above as settled for months while Tailscale was never installed on the host at
all — the only node in the system was the `vault-mcp` Funnel sidecar, which is
container-scoped and gives the host nothing. That gap was caught earlier the
same day and recorded here as *"never actually done"*; the QNAP was then
genuinely added to the tailnet a few hours later, which is what this replaces.
The lesson outlives both notes: **this file describes intent, so verify against
the host before trusting a networking claim in it.**

The QNAP is now a real tailnet node (`kyles-nas`), installed from the App
Center. But it is **untagged**, so ACLs cannot target it by tag, and **`RunSSH`
is `false`** — access is by SSH key through the 1Password agent, not tailnet
identity. So "Tailscale SSH is the deploy path" above is *still* aspirational.
The two are one decision: tagging transfers node ownership from the user to the
tag, so the ACL rule has to exist before `--ssh` is worth enabling. Until then,
do not write anything that assumes ACL-governed access to this node.

Three things that could bite:

1. **Threat Management in Detect-and-Block** occasionally interferes with
   sustained UDP flows. If `tailscale status` shows `relay` rather than `direct`,
   disable it as a test — **but rule out the address family first.** A direct
   connection needs both ends reachable over the *same* protocol. Observed
   2026-08-12: from an IPv6-only carrier network (Three 5G, NAT64) every path to
   `kyles-nas` was `relay "lhr"` at ~123 ms, and no UniFi setting could have
   changed that, because the client had no IPv4 at all. Relay is the *correct*
   outcome there. Only suspect Threat Management when you see relay from a
   dual-stack network.
2. **Direct vs relayed** — Tailscale wants UDP 41641 outbound; relay over 443
   works but adds latency.
3. **VLAN semantics get bypassed** — if the QNAP sits on an isolated VLAN,
   Tailscale routes around that entirely.

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
