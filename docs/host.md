# The host

How to reach the QNAP, and the traps that cost time the first time round.
Written for whoever is SSH'd in at 1am, including future you.

Nothing in this repo enforces anything on this page. Where a line says *verify
rather than trust*, take it literally.

| | |
|---|---|
| Hardware | QNAP, Intel Celeron J4125, 4 cores, x86-64, **no AVX2** |
| Memory | 16 GB, ~11.7 GB free |
| OS | QTS 5.2.10 (build 20260722) |
| Runtime | Container Station |
| Access | Tailscale — the NAS is its own tailnet node (`kyles-nas`), not behind a subnet router |

## Reaching the host

**This row has been wrong in both directions in a single day.** It described a
tailnet node that had never been installed; that was corrected on 2026-08-12 to
"LAN SSH, not on the tailnet"; the node was then actually installed a few hours
later, making the correction stale in turn. It is now true as written — checked
against the host, not inferred from intent. Check before correcting it again.

Host Tailscale is the **App Center** package (Communications category), installed
2026-08-12, separate from the `vault-mcp` and `gmail-mcp` funnel sidecars — three nodes,
three state directories, no interaction. SSH in over the tailnet name rather than the LAN
address, so the same config works from anywhere:

```sh
ssh admin@kyles-nas.tail3df177.ts.net
```

**Tailscale SSH is off** (`RunSSH: false`) and staying off — access is by SSH key
through the 1Password agent. That is a decision, not a gap; the reasoning is in
[`DECISIONS.md`](../obsidian-vault/DECISIONS.md#tailscale-ssh-considered-and-rejected),
and the short version is that QNAP's `sshd` has to keep running for the LAN
fallback anyway, so enabling it would add a second SSH server rather than replace
one.

The node is **untagged** and staying that way, so ACLs cannot target it by tag —
do not write anything that assumes ACL-governed access to it. Tagging would have
cost a re-authentication of the one node that cannot be re-authenticated
remotely, to buy things this tailnet does not use;
[`DECISIONS.md`](../obsidian-vault/DECISIONS.md#tagging-rejected-and-the-expiry-hazard-it-was-hiding)
has the assessment.

**Key expiry is disabled on `kyles-nas`, and must stay that way** — confirmed
2026-08-12. Untagged nodes expire by default and this one was 179 days out. On
expiry the node leaves the tailnet, recoverable only by running `tailscale up` on
the box, which needs the access that just expired: a scheduled lockout, not an
inconvenience. Nothing in this repo enforces it, so verify rather than trust:

```sh
tailscale status --json | \
  python3 -c "import json,sys; d=json.load(sys.stdin); \
  print({v['HostName']: v.get('KeyExpiry','never') for v in d['Peer'].values()})"
```

**Do not install Tailscale from the App Center.** The listing is a curated shelf
that QNAP has to republish for each release, and it has stalled: it shipped
**1.40.0** — from 2023 — long after 1.102.2 was stable. This is not a stale cache
on your NAS; everyone installing from App Center gets 1.40.0
([tailscale-qpkg#130](https://github.com/tailscale/tailscale-qpkg/issues/130)).
`tailscale update` is no help either; it reports *"not supported on this
platform"* on QNAP.

The host was upgraded to **1.102.2** on 2026-08-12 by downloading the QPKG from
[pkgs.tailscale.com](https://pkgs.tailscale.com/stable/) and using *App Center →
Install Manually*. That registers properly — `qpkg.conf` shows the real version
and build — and **preserves node state**: same tailnet IP, no re-auth, prefs
intact. So the same route works for the next upgrade.

**This is now a recurring manual chore.** Nothing will notify you. The marker is
`Build` in `qpkg.conf` (currently `20260804`); when `pkgs.tailscale.com` is newer
than that, it is time for another pass.

**Find QPKG paths per package, never by guessing the volume.** The two packages
that matter here sit on *different* volumes, and neither is the volume the repo
is checked out on (`/share/CE_CACHEDEV4_DATA`), so any hardcoded path is wrong
somewhere:

```sh
getcfg Tailscale         Install_Path -f /etc/config/qpkg.conf  # /share/CACHEDEV1_DATA/.qpkg/Tailscale
getcfg container-station Install_Path -f /etc/config/qpkg.conf  # /share/CE_CACHEDEV2_DATA/.qpkg/container-station
```

Tailscale's own docs suggest `getcfg SHARE_DEF defVolMP -f /etc/config/def_share.info`,
but that returns the *default* volume — correct for Tailscale here and wrong for
Container Station. Ask `qpkg.conf` about the specific package instead. This is
the same trap [`DECISIONS.md`](../obsidian-vault/DECISIONS.md#traps-found-while-building)
records about volume names not being predictable.

**`docker` is not on the PATH of a non-interactive SSH session** — `ssh nas
docker ps` fails with `command not found` even though an interactive login works.
Anything scripted has to put it there first:

```sh
export PATH=$PATH:$(getcfg container-station Install_Path -f /etc/config/qpkg.conf)/bin
```

**Nothing needs configuration on the UniFi gateway**, and that has survived every
stack added since it was written. The connector stacks' Tailscale sidecars dial
*out* and reach the server over a compose network rather than a host port;
`dashboard` and `monitoring` publish ports, but on loopback only, reached through
`tailscale serve`; `monitoring`'s agent has no network at all and is read over
a unix socket. The gateway stays a bystander either way.

Two things about that have changed, in opposite directions:

- Two services are **reachable from the internet**: `vault-mcp` and
  `gmail-mcp`, each over its own Funnel whose TLS terminates on the NAS rather
  than at a third party. See
  [`vault-mcp/README.md`](../vault-mcp/README.md#trust-boundary) and
  [`gmail-mcp/README.md`](../gmail-mcp/README.md#trust-boundary).
- **Two stacks now publish a host port**, which was previously true of none.
  Both bind loopback and both authenticate by the identity header Serve adds.
  `monitoring` binds `127.0.0.1:${MON_PORT:-8091}`, served at
  `https://kyles-nas.tail3df177.ts.net:8443/`; the same reasoning applies, to a
  page that can read graphs rather than press deploy. See
  [`monitoring/README.md`](../monitoring/README.md#trust-boundary).
  `dashboard` binds `127.0.0.1:${DASH_PORT:-8088}`, and `tailscale serve` on the
  NAS's own tailnet node puts it at `https://kyles-nas.tail3df177.ts.net/` with a
  real certificate. **No LAN path**, and that is a security control rather than
  an omission: the dashboard authenticates by the tailnet identity header Serve
  adds, so a listener the LAN could reach would be a deploy button the LAN could
  press. It is deliberately not on a Funnel. That reversal, and the Docker socket
  that comes with it, are assessed in
  [`DECISIONS.md`](../obsidian-vault/DECISIONS.md#the-dashboard-and-the-docker-socket);
  the move from a pasted token to tailnet identity is in
  [`DECISIONS.md`](../obsidian-vault/DECISIONS.md#updating-services-identity-at-the-door-digests-in-git).
