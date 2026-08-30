# dashboard

One page that says what is running on the NAS, whether any of it is out of
date, and lets you do something about it from a phone.

It is a front-end for [`bin/homelab`](../bin/homelab), not a second way to
deploy these stacks. Every button runs the same command you would type over
SSH, which is what keeps provenance verification, `obsidian-vault`'s
`deploy.sh` delegation and its re-pair warning working rather than quietly
reimplemented and drifting.

```
http://kyles-nas:8088/                              on the LAN
http://kyles-nas.tail3df177.ts.net:8088/            from anywhere on the tailnet
```

## What it shows

Per stack: the containers and their state, health and age; the image `.env`
names; whether `.env` is `0600`; and a badge saying whether the registry has a
newer image than the one actually running.

The badge compares **digests**, not tags. `:latest` is not a version, and the
whole question is that the tag has not moved while the thing behind it has. It
reads the digest off the running container rather than out of `.env`, so a
stack whose `.env` was edited but never deployed shows as out of date, which is
the drift worth seeing.

Four states, and `pinned` is not the same as up to date:

| Badge | Means |
|---|---|
| `up to date` | The running digest is the one the tag resolves to now. |
| `update available` | The registry has moved on. Press deploy. |
| `pinned` | The reference names a digest, so there is nothing to chase. The Tailscale sidecar is pinned this way on purpose; upstream may well be newer. |
| `unknown` | The registry could not be reached, or the image was never pulled from one. Hover for why. |

It also lists the containers this repo does **not** own — `home-assistant`,
`esphome`, `matter-server` — with no buttons. The root README is explicit that
`docker ps` is the ground truth for the host, so a page that showed only this
repo's stacks would be quietly wrong about what is running.

They do still get the version check, per container rather than per stack: they
have no `.env` naming an intended image, so the reference on the container
itself is the only question there is to ask. Nothing here can act on the
answer — Container Station owns their lifecycle — but *"Home Assistant is three
releases behind"* is worth knowing from the same page as everything else.

### Bringing one of them in properly

Nothing in this code would need to change. The agent asks `bin/homelab stacks`
what it can act on, so adopting Home Assistant is: add `home-assistant/` with a
`docker-compose.yml` and `.env`, add it to `STACKS` in `bin/homelab`, and it
gets the same buttons as everything else. HA Container has no Supervisor, so
updating it genuinely is pull-and-recreate — exactly the model here.

The risk is not in the dashboard, it is in the migration. Capture
`docker inspect` of the running container first, because three details decide
whether adoption is routine or destructive:

- **`/config` as a named volume rather than a host bind mount.** A new compose
  project creates a *different* named volume and HA comes up factory-fresh.
- **`network_mode: host`**, which HA needs for mDNS and SSDP discovery — and
  which makes this repo's "one published port" claim wrong again.
- **Device passthrough** for a Zigbee or Z-Wave stick (`/dev/ttyUSB*`,
  `/dev/ttyACM*`). Losing it looks like every device going unavailable at once.

Adopting also makes the root README's *"they share nothing with the stacks
above ... so the shared contract does not reach them"* into a question that
needs an answer rather than an observation.

## Who can press the buttons

Reading the page needs nothing. Running a command needs `DASH_TOKEN` if it
changes the host **or hands back content the page does not already show**.

| | Needs the token |
|---|---|
| The page itself: containers, state, health, update badges | no |
| `status`, `ps`, `env check`, `preflight` | no |
| `logs`, `connector url` | **yes** |
| `deploy`, `deploy (sync only)`, `restart` | **yes** |

That line is not "does it change anything", and it moved during review. It was
once argued that every read could stay open because anyone on the LAN could run
`docker ps` on the NAS anyway. That does not hold twice over: running `docker
ps` needs an SSH account and access to the root-owned socket, which is a far
smaller population than "can reach port 8088"; and `logs obsidian-vault
vault-claude` is nothing like `docker ps` — it returns the output of the
always-on Claude agent, which is vault content, and sits squarely inside the
trust boundary
[`ARCHITECTURE.md`](../obsidian-vault/ARCHITECTURE.md#trust-boundary) exists to
protect.

What stays open tells a caller no more than the page they could already load.

Sign in once and a signed, expiring cookie carries it for `DASH_SESSION_TTL`.
The cookie holds a signature rather than the token, is `HttpOnly` and
`SameSite=Strict`, and every mutating request must also carry an
`X-Homelab-Action` header — a header a cross-site form cannot set at all, so
there is no simple-request path to a deploy. Rotating `DASH_TOKEN` invalidates
every outstanding cookie, because the token is the signing key.

**Leaving `DASH_TOKEN` blank is a supported mode.** The dashboard runs
read-only, shows everything, and refuses each mutating action with an
explanation rather than a 403 that looks like a bug.

The cookie is not `Secure`, because this is served over plain HTTP. That is
acceptable *here* and would not be for `vault-mcp`: the traffic never leaves
the house or the tailnet. See the trust boundary below.

## Trust boundary

**Two containers off one image, and the split is the whole security design.**

| | `homelab-dash` | `homelab-dashd` |
|---|---|---|
| Renders HTML | yes | no |
| Reachable from the LAN | yes, published port | no, compose network only |
| Holds `/var/run/docker.sock` | **no** | yes |
| Holds the checkout | **no** | yes, read-only |
| Runs as | `1002:100` | `0:0` |

The process that parses untrusted HTTP and renders HTML holds no privilege.
The process that holds the socket serves no HTML and accepts exactly one shape
of request: a verb from a closed list, plus a stack name and optionally a
service name, both checked against what is actually on disk and running before
they go anywhere near `exec`.

**The list of stacks comes from `bin/homelab stacks`, not from the filesystem.**
That is deliberate and was originally wrong: the agent used to scan the checkout
for `docker-compose.yml` while the CLI gates every command on its own `STACKS`
list. A directory in one and not the other meant a card with a full row of
buttons that every command then rejected with `unknown stack`. One question, one
answer, and adding a stack stays a single edit.

There is no passthrough, no "extra args" field and no shell anywhere in the
path. The agent builds every `argv` itself out of constants, so a service name
of `; rm -rf /` is a name that fails validation rather than a command. That is
asserted in `agent_test.go` against path traversal, leading dashes, shell
metacharacters, newlines and NUL bytes, and those tests run inside the
Dockerfile as well as in CI — an image cannot reach the NAS having skipped them.

This is defence in depth, not a claim of containment. A full compromise of the
web container still reaches the agent's verbs, and those verbs deploy things.
What it buys is that the reachable surface is *"deploy vault-mcp"* rather than
*"any Docker API call"* — no `docker run -v /:/host`, no exec into `vault-claude`
to read its Claude credentials, no new container at all.

### Why the socket, after refusing it three times

`obsidian-vault`'s `vault-cron`, its compose file and `ARCHITECTURE.md` all
refuse `/var/run/docker.sock` on the grounds that it is effective root on the
NAS for no benefit. **Those refusals still stand**, and are unchanged: a cron
job that runs `backup.sh` over its own volumes gains nothing from the socket.

What changed is the goal. "Press a button and it deploys" cannot be built
without something on this host holding the socket, and the honest options were
to hold it in one small, closed-verb container or to not build the feature. The
assessment is recorded in
[`../obsidian-vault/DECISIONS.md`](../obsidian-vault/DECISIONS.md#the-dashboard-and-the-docker-socket).

### Why not a socket proxy

A read-only socket proxy (`tecnativa/docker-socket-proxy` and friends) is the
usual answer, and it is a good one for a dashboard that only *reads*. It does
not survive contact with this stack's actual requirement: deploying needs image
pulls and container creates, so any proxy configured to allow it is a proxy
that allows the dangerous half. A proxy filtering by HTTP verb and path would
be a second allowlist, in a config file, expressed in terms of Engine API
endpoints — strictly weaker than one expressed in terms of `homelab deploy
vault-mcp`, and one more thing to keep correct.

## What it cannot do

Deliberate gaps, each with a reason:

- **Edit `.env`.** The checkout is mounted read-only, so `homelab env set` is
  not reachable from the browser. Those files hold `MCP_TOKEN` and
  `TS_AUTHKEY`; giving a web-facing service write access to them to save an SSH
  session is a bad trade. `env check` is available and shows which keys are
  blank, never their values.
- **`git pull` the checkout** (`homelab update`). Same read-only mount. The
  checkout is what the deploy buttons execute; a web request that can rewrite
  it is a web request that can change what every button does.
- **Deploy or restart itself.** See below.
- **Verify build provenance.** `homelab deploy` verifies attestations when the
  `gh` CLI is present, and this image does not ship it — so a deploy from the
  dashboard logs `WARNING: skipping provenance verification` where the same
  command from a NAS session with `gh` installed would check. This is the one
  place where pressing the button is weaker than typing the command, and it is
  why the output pane shows you the whole log rather than a green tick.
- **Reach the internet on your behalf**, beyond `HEAD` requests to a registry
  to resolve a tag to a digest.

### It will not deploy itself

`deploy` and `restart` are refused for the `dashboard` stack. The command would
recreate the container running it, so it is killed partway through and can
report nothing — did it pull? did it recreate the agent but not the web half?
Rather than special-case a self-restart that cannot report its own outcome, the
verb is simply unavailable, in the same spirit as `bin/homelab` having no
`--all`: the cheapest way to honour a rule is to make the wrong thing
unexpressible.

Deploy the dashboard over SSH, like everything else in this repo was deployed
before it existed:

```sh
homelab deploy dashboard
```

Reading its own status, logs and `env check` is fine and available.

## Setup

```sh
cd /share/CE_CACHEDEV4_DATA/homelab/dashboard
cp .env.example .env && chmod 600 .env
```

Fill in three things:

```sh
REPO_HOST_PATH=/share/CE_CACHEDEV4_DATA/homelab   # this checkout's real path
DASH_AGENT_TOKEN=$(openssl rand -hex 32)          # between the containers
DASH_TOKEN=$(openssl rand -hex 32)                # what you type in
```

`REPO_HOST_PATH` must be the checkout's **real host path**, and the compose
file mounts it at that identical path inside the agent rather than at `/repo`.
This is not cosmetic. `docker compose` running inside a container is a *client*
of the host daemon: it resolves a stack's relative bind mounts against the
project directory as the client sees it, then hands the daemon that absolute
path. `vault-mcp`'s compose file mounts `./funnel.json`, so with the checkout at
`/repo` the daemon would be asked for `/repo/vault-mcp/funnel.json`, which does
not exist on the host, and the Funnel would come up with an empty serve config.
Same path on both sides, and every relative mount in every stack keeps meaning
what it says.

Then:

```sh
homelab deploy dashboard
homelab status dashboard
```

`homelab status dashboard` reports the URL, whether the buttons are armed, and
whether `REPO_HOST_PATH` matches the checkout you are standing in. It never
prints either token.

## Reaching it over the tailnet

There is no Tailscale sidecar here, unlike `vault-mcp`. There does not need to
be: the NAS has been its own tailnet node since 2026-08-12, so a port published
on the host answers on its tailnet address as well as its LAN one. One
published port covers both, with no second node, no auth key and no state
directory to lose.

The cost is that this is plain HTTP with no tailnet identity attached, which is
why `DASH_TOKEN` exists. It is deliberately **not** on a Funnel — `vault-mcp` is
the one stack in this repo that belongs on the internet, and a page listing
every container and version on the host is not the second.

## Operating it

Everything the page does is also a command:

| Button | Runs |
|---|---|
| deploy | `homelab deploy <stack>` |
| deploy (sync only) | `homelab deploy obsidian-vault --sync-only` |
| restart | `homelab restart <stack>` |
| status / ps / logs | `homelab status\|ps\|logs <stack>` |
| logs / restart, on a container row | `homelab logs\|restart <stack> <service>` |
| env check | `homelab env check <stack>` |
| preflight | `homelab preflight <stack>` (only where the stack ships one) |
| connector url | `homelab url` |

The stack list itself comes from `homelab stacks`, which exists for this and
prints one name per line with no colour or header.

Output appears verbatim under the card, as text — never as markup. Double-click
it to dismiss, which also resumes the idle refresh. The page refreshes itself
every `DASH_REFRESH` while idle and never while a command's output is on
screen, so it cannot eat a deploy log you are reading.

Mutating actions are serialised host-wide: a second one gets `busy: deploy
vault-mcp is still running` rather than interleaving. Reads are not blocked, so
you can watch logs during a deploy.

**Per-container buttons are the finer-grained path.** Each running container
gets its own `logs` and `restart`, so recreating `vault-claude` does not touch
`vault-sync` — which is the whole reason `obsidian-vault` splits them into
separate services. The stack-wide `restart` recreates everything.

**`deploy (sync only)` exists for a reason.** `obsidian-vault`'s `vault-claude`
holds a live tmux session your phone may be paired to, and the repo's shared
contract warns that a deploy can interrupt an agent run in progress. Sync-only
updates `vault-sync` and leaves that session alone.

## Failure modes

| Symptom | Cause |
|---|---|
| "Cannot reach the agent" | `homelab-dashd` is down. It is the half with the socket; the web half is up or you would see nothing. |
| "The agent cannot reach the Docker daemon" | The socket mount is missing, or QTS moved it. Stacks still list from the checkout; nothing reflects reality. |
| Every badge says `unknown` | No outbound HTTPS from the NAS, or GHCR is down. Failures are cached for `DASH_REGISTRY_TTL` so the page stays fast. |
| "Cannot read the stack list" | `bin/homelab stacks` could not run — usually a broken `REPO_HOST_PATH` mount. Every button would have failed anyway, so the page says so instead of rendering as a host with no stacks. |
| A stack shows no containers | It has never been deployed. Do the first deploy over SSH. |
| A container row has no buttons | Per-service `logs` and `restart` are offered per running container. A container with no compose service label, or one that has never been created, has nothing to target; use the stack-wide buttons. |
| `too many commands running at once` | The read limiter (`DASH_MAX_READS`, default 4) is saturated. Reads are deliberately concurrent, but not unbounded: each one forks `bash` and `docker compose` inside the socket-holding container. |
| Deploy log says `skipping provenance verification` | Expected. See "What it cannot do". |

## Why Go, and why stdlib only

`bin/homelab` is bash because it is glue that runs `docker` and checks exit
codes — the case
[`DECISIONS.md`](../obsidian-vault/DECISIONS.md#runtime-node-not-bun-or-go)
already argued. This is not that: it parses untrusted HTTP input and renders
HTML, which is where a compiler and a type system earn their keep. It follows
`vault-mcp`'s conventions — `-version` and `-healthcheck` flags, tests as the
security assertion, no `curl` in the runtime image.

There are **no third-party modules**, and CI fails the build if `go.mod` grows a
`require` block. The process holding the Docker socket should be auditable by
reading it, and the Docker client here is three read-only endpoints rather than
most of Docker's internals vendored in.
