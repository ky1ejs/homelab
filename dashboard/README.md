# dashboard

One page that says what is running on the NAS, whether any of it is out of
date, and lets you do something about it from a phone.

It is a front-end for [`bin/homelab`](../bin/homelab), not a second way to
deploy these stacks. Every button runs the same command you would type over
SSH, which is what keeps provenance verification, `obsidian-vault`'s
`deploy.sh` delegation and its re-pair warning working rather than quietly
reimplemented and drifting.

```
https://kyles-nas.tail3df177.ts.net/                from anywhere on the tailnet
```

**One URL, and no LAN one.** The container publishes on `127.0.0.1` only;
`tailscale serve` on the NAS's own tailnet node is what makes it reachable, with
a real Let's Encrypt certificate. That is not a convenience — it is what makes
the authentication work at all. See [Who can press the buttons](#who-can-press-the-buttons).

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

### Whether the checkout is stale

**Deploying pulls a new image and runs it against the compose file already on
disk.** The checkout is mounted read-only, so `homelab update` is not reachable
from the browser — which means a stack whose compose file changed upstream would
deploy the new image into the old configuration, and until now nothing said so.
It reported success.

The page now says how far behind the checkout is, which commits are missing, and
which stacks they touch:

> **The checkout is 3 commits behind.** Deploy pulls a new image but runs it
> against the compose file already on disk, so update first: `ssh
> admin@kyles-nas.tail3df177.ts.net` then `homelab update`. Then deploy:
> `vault-mcp`

It cannot fix it — that is still an SSH trip, and
[deliberately so](#what-it-cannot-do). Making it visible is the part that was
missing.

Answered by asking GitHub to compare the checkout's `HEAD` against the branch it
tracks — read from `branch.<name>.merge` in `.git/config`, which is the ref `git
pull` itself would use, not an assumption that a branch of the same name exists
on `origin`. A branch tracking nothing is said to be tracking nothing rather than
compared against a guess. Not by running git, though: Answering it locally needs `git fetch`, a fetch
*writes*, and the read-only mount is what keeps the web-facing half of this stack
from changing what the deploy buttons execute. The commit and remote are read
straight out of `.git` by the agent — no git binary, no `exec`, no write — and
the comparison is an outbound HTTPS call made from the web role, for the same
reason the registry lookups are.

That call is **unauthenticated**, so it is rate limited to 60 an hour. It is
cached for `DASH_GITHUB_TTL` (15m); at the default that is four calls an hour,
and a page left open on a phone cannot exhaust the budget.

GitHub caps its own answer at 250 commits and 300 files while the counts stay
exact, so a very stale checkout can get a short commit list and — the one that
matters — a short "then deploy" list. The page says so when it happens rather
than presenting a partial answer as complete.

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

**Nothing is typed.** `tailscale serve` terminates TLS in front of this stack
and adds a `Tailscale-User-Login` header naming the tailnet user behind the
request; the dashboard checks it against `DASH_ALLOWED_LOGINS`. There is no
password, no token, no cookie and no session to expire — which matters more than
it sounds for the one tool you open *because* something is broken. It also means
no third party has to be reachable for you to sign in, which is why this is not
WorkOS AuthKit like [`vault-mcp`](../vault-mcp/README.md#how-this-authenticates).

Reading the page needs nothing. Running a command needs an allowed identity if
it changes the host **or hands back content the page does not already show**.

| | Needs an identity |
|---|---|
| The page itself: containers, state, health, update badges | no |
| `status`, `ps`, `env check`, `preflight` | no |
| `logs`, `connector url` | **yes** |
| `deploy`, `deploy (sync only)`, `restart` | **yes** |

That line is not "does it change anything", and it moved during review. It was
once argued that every read could stay open because anyone on the LAN could run
`docker ps` on the NAS anyway. That does not hold twice over: running `docker
ps` needs an SSH account and access to the root-owned socket; and `logs
obsidian-vault vault-claude` is nothing like `docker ps` — it returns the output
of the always-on Claude agent, which is vault content, and sits squarely inside
the trust boundary
[`ARCHITECTURE.md`](../obsidian-vault/ARCHITECTURE.md#trust-boundary) exists to
protect.

What stays open tells a caller no more than the page they could already load.

### The header is only a credential because nothing else can reach the listener

**This inverts what used to protect this stack, and that is the thing to
understand before changing anything here.** Under the old `DASH_TOKEN` an
attacker needed a 32-byte secret. Under a proxy header they need only to reach
the listener directly and send

```
Tailscale-User-Login: you@example.com
```

which is one `curl` away. Three things make that impossible rather than
unlikely, and they live in three different places on purpose:

1. **`docker-compose.yml` binds `127.0.0.1:${DASH_PORT}:8080`.** Not `0.0.0.0`.
   Publishing this port to the LAN does not merely restore LAN access — it hands
   the deploy buttons to anything on the LAN.
2. **The agent asks the daemon whether that is actually true**, on every mutating
   and sensitive action, and refuses them all if any of this stack's host
   bindings is not loopback — or if it cannot find the container that serves the
   page (`DASH_SELF_SERVICE`) to inspect in the first place. Checking only "does
   anything in this project publish badly" was not enough: `homelab-dashd`
   publishes nothing, so a listing containing only the agent passed cleanly
   having examined no web listener at all. The compose file is exactly the kind
   of thing this dashboard makes it easy to change, so the premise is checked
   against reality rather than read off the file that asserts it. Get the publish
   wrong and the dashboard *breaks* — the page renders, says so in red, and every
   button is refused.
3. **Any request carrying `Tailscale-Funnel-Request` is refused outright.**
   Funnel traffic gets no identity headers and arrives by the same loopback path
   Serve does, so nothing else would catch it.

The obvious version of (2) — checking `RemoteAddr` against a trusted CIDR in the
web role — does not work, and is documented in `agent.go` so it is not
reintroduced: inside a container behind Docker's port publishing that address is
either the bridge gateway or a preserved external address depending on how the
daemon forwards, and the CIDR wide enough to cover the gateway is also the LAN.

`scripts/preflight.sh` checks all of this ahead of time, plus the one thing the
agent cannot see: whether `tailscale serve` is running at all.

### The one CSRF defence

Every request to `/action` must carry an `X-Homelab-Action` header — **every**
one, not only the mutating ones. There is no cookie any more, so
`SameSite=Strict` protects nothing: the proxy attaches identity to a cross-origin
request from anywhere as readily as to one from this page. A plain form post
cannot set a custom header, and a `fetch` that does triggers a CORS preflight
this server never answers.

It used to be required only of the gated verbs, which left `status`, `ps`, `env
check` and `preflight` reachable as CORS *simple requests*: the body is never
inspected for `Content-Type`, so a cross-origin form post of
`{"action":"preflight","stack":"dashboard"}` needed no preflight and the browser
sent it. Nothing leaked — the response is unreadable to the sender — but those
verbs are not inert. Each forks `bash` and `docker compose` inside the container
holding the daemon socket, and `DASH_MAX_READS` is 4, so any page a tailnet user
visited could hold the read pool shut and make every button return 429.
Requiring it uniformly costs nothing, because the page already sends it on every
request.

Do not relax it, and do not add CORS headers to this binary.

### Modes that have to be stated out loud

A blank `DASH_ALLOWED_LOGINS` **refuses to start**. It is not "allow everybody",
and it must never quietly become that — an empty value silently meaning "allow"
is [how `vault-mcp` was once wide open on a public
hostname](../vault-mcp/README.md#pinning-the-subject). The permissive answers are
both explicit:

| Variable | Means |
|---|---|
| `DASH_ALLOWED_LOGINS=you@example.com` | these logins may act |
| `DASH_ALLOW_ANY_TAILNET_USER=1` | every account on this tailnet may act |
| `DASH_READ_ONLY=1` | nobody may act; show everything, refuse with an explanation |

**Read-only is still a supported mode**, it is just no longer spelled "leave the
token blank".

## Trust boundary

**Two containers off one image, and the split is the whole security design.**

| | `homelab-dash` | `homelab-dashd` |
|---|---|---|
| Renders HTML | yes | no |
| Reachable from the LAN | **no**, loopback publish only | no, compose network only |
| Reachable from the tailnet | yes, through `tailscale serve` | no |
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
- **`git pull` the checkout** (`homelab update`). Same read-only mount, and the
  page now *tells you* when that matters rather than leaving it silent — see
  [Whether the checkout is stale](#whether-the-checkout-is-stale). The
  checkout is what the deploy buttons execute; a web request that can rewrite
  it is a web request that can change what every button does.
- **Deploy or restart itself.** See below.
- **Verify build provenance.** `homelab deploy` verifies attestations when the
  `gh` CLI is present, and this image does not ship it — so a deploy from the
  dashboard logs `WARNING: gh CLI not found - skipping provenance verification`
  where the same command from a NAS session with `gh` installed would check.
  This is the one place where pressing the button is weaker than typing the
  command, and it is why the output pane shows you the whole log rather than a
  green tick. It is also the *only* remaining reason a deploy skips the check: a
  blank `GITHUB_OWNER` used to produce the same line and now fails the deploy
  outright wherever `gh` exists.
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

## Upgrading an existing install

**Read this before `homelab deploy dashboard` if this stack already runs here.**
The section after it describes a fresh setup, and `.env` is gitignored — so
pulling this change does not touch the `.env` already on the NAS, and that file
is missing the one key the web container now refuses to start without.

Deploying first and reading later gets you a **restart loop**: `newAuthenticator`
returns an error on a blank `DASH_ALLOWED_LOGINS`, `main.go` treats that as
fatal, and compose's `restart: unless-stopped` keeps trying. Nothing is damaged
and SSH is unaffected, but the page is down until the `.env` is fixed.

The CLI will tell you, if you ask it before deploying rather than after:

```sh
homelab update
homelab env check dashboard
```

```
dashboard .env
  [ok  ] mode                   0600
  [FAIL] DASH_ALLOWED_LOGINS    empty
  [FAIL] DASH_GITHUB_TTL        empty
```

Then edit `dashboard/.env`:

| | |
|---|---|
| **add** `DASH_ALLOWED_LOGINS=` | your tailnet login — see below if you do not know it |
| **add** `DASH_GITHUB_TTL=15m` | how long the "checkout is behind" answer is cached |
| **delete** `DASH_TOKEN=` | nothing reads it; `preflight` warns while it is still there |
| **delete** `DASH_SESSION_TTL=` | there are no sessions to expire |

**Finding your tailnet login is a chicken-and-egg**, because the page that would
show it to you will not start without it. The way through is one throwaway
deploy:

```sh
# temporarily, in .env
DASH_ALLOW_ANY_TAILNET_USER=1
```

Deploy, load the page, read the login off the badge in the header, then put it
in `DASH_ALLOWED_LOGINS` and unset the override. Acceptable for a few minutes on
a tailnet with one human on it; not a resting state.

**Order matters, and only one ordering works:**

1. `homelab update` — pull this change into the checkout
2. fix `.env` as above
3. `tailscale serve --bg --https 443 http://127.0.0.1:8088` — without it there is
   no route at all, because step 4 moves the listener to loopback
4. `homelab deploy dashboard` — over SSH; it still will not deploy itself
5. `homelab preflight dashboard`

Steps 2 and 3 have to precede 4. Doing 4 first costs you the dashboard until you
go back and do them; it cannot lock you out of the NAS.

**Then check the premise the whole design rests on**, from any tailnet device:

```sh
curl -s -H 'Tailscale-User-Login: nobody@example.com' \
  https://kyles-nas.tail3df177.ts.net/ | grep -c 'nobody@example.com'
```

It must print `0`. Anything else means `tailscale serve` is forwarding a
client-supplied identity header rather than replacing it, and the allow list is
bypassable by any tailnet device impersonating an allowed one. A stranger still
could not reach it — the listener is loopback-only — but that is precisely the
control the allow list exists to provide.

## Setup

**First install only.** If this stack already runs on the NAS, its `.env`
survives every deploy and needs editing rather than replacing — see
[Upgrading an existing install](#upgrading-an-existing-install) above.

```sh
cd /share/CE_CACHEDEV4_DATA/homelab/dashboard
cp .env.example .env && chmod 600 .env
```

Fill in three things:

```sh
REPO_HOST_PATH=/share/CE_CACHEDEV4_DATA/homelab   # this checkout's real path
DASH_AGENT_TOKEN=$(openssl rand -hex 32)          # between the containers
DASH_ALLOWED_LOGINS=you@example.com               # who may press the buttons
```

`DASH_ALLOWED_LOGINS` is your **tailnet login** — the value `tailscale serve`
puts in `Tailscale-User-Login`, which on a personal tailnet is the email address
you signed up with. A blank value refuses to start; see [Modes that have to be
stated out loud](#modes-that-have-to-be-stated-out-loud).

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

Then put `tailscale serve` in front of it. This is not optional: the container
publishes on loopback, so without it there is no route to the dashboard at all.

```sh
tailscale serve --bg --https 443 http://127.0.0.1:8088
```

`--bg` persists it across reboots. The certificate comes from Let's Encrypt via
the tailnet, which already has HTTPS certificates enabled because `vault-mcp`'s
Funnel requires them — so there is nothing to turn on.

Then:

```sh
homelab deploy dashboard
homelab preflight dashboard
homelab status dashboard
```

`homelab preflight dashboard` asserts every clause of the authentication
premise: the loopback bind in the compose file, the live binding on the running
containers, the allow list, and that `serve` is actually proxying. `homelab
status dashboard` reports the URL, who the buttons are armed for, and whether
`REPO_HOST_PATH` matches the checkout you are standing in. Neither prints
`DASH_AGENT_TOKEN`.

## Reaching it over the tailnet

There is no Tailscale sidecar here, unlike `vault-mcp`, and there does not need
to be: the NAS has been its own tailnet node since 2026-08-12, so `tailscale
serve` on that node covers this with no second node, no auth key and no state
directory to lose. That is the difference from the sidecar assessed and rejected
in
[`DECISIONS.md`](../obsidian-vault/DECISIONS.md#publishing-a-port-the-second-reversal) —
Serve on an existing node costs none of the three things a sidecar costs.

It buys real HTTPS and a tailnet identity on every request, which is what
replaced the shared token. It is deliberately **not** on a Funnel — `vault-mcp`
is the one stack in this repo that belongs on the internet, and a page listing
every container and version on the host is not the second. The binary refuses
funnelled requests, and `preflight.sh` fails if one is configured.

**The cost is that there is no longer a LAN path.** Anything that presses these
buttons has to be on the tailnet. That is accepted deliberately: identity is a
header the proxy adds, so a listener the LAN could reach would be a deploy button
the LAN could press.

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

**`deploy (sync only)` exists for a reason.** `obsidian-vault` runs two
always-on agents — `vault-claude` and, since the research surface landed,
`vault-research` — each holding a live tmux session your phone may be paired to,
and the repo's shared contract warns that a deploy can interrupt an agent run in
progress. Sync-only updates `vault-sync` and leaves both sessions alone.

## Failure modes

| Symptom | Cause |
|---|---|
| The page will not load at all | `tailscale serve` is not running, or you are not on the tailnet. The container publishes on loopback; Serve is the only route. `homelab status dashboard` says which. |
| "not identified", and every button refused | The request reached the listener without going through `tailscale serve`, or came from a tagged node, which has no user. |
| "is not in DASH_ALLOWED_LOGINS" | The proxy identified you and the allow list does not have you. Add the login and recreate. |
| Red banner: "this stack is not published the way its authentication assumes" | Something published the web container beyond loopback. Every mutating and sensitive action is refused until it is fixed — that is the check working, not a bug. `homelab preflight dashboard`. |
| The web container restart-loops after a deploy | `DASH_ALLOWED_LOGINS` is blank and neither override is set. It refuses to start rather than run permissive. |
| "Cannot tell whether the checkout is up to date" | GitHub could not be reached, or the unauthenticated rate limit is spent. It resolves itself within the hour. |
| "Cannot reach the agent" | `homelab-dashd` is down. It is the half with the socket; the web half is up or you would see nothing. |
| "The agent cannot reach the Docker daemon" | The socket mount is missing, or QTS moved it. Stacks still list from the checkout; nothing reflects reality. |
| Every badge says `unknown` | No outbound HTTPS from the NAS, or GHCR is down. Failures are cached for `DASH_REGISTRY_TTL` so the page stays fast. |
| "Cannot read the stack list" | `bin/homelab stacks` could not run — usually a broken `REPO_HOST_PATH` mount. Every button would have failed anyway, so the page says so instead of rendering as a host with no stacks. |
| A stack shows no containers | It has never been deployed. Do the first deploy over SSH. |
| A container row has no buttons | Per-service `logs` and `restart` are offered per running container. A container with no compose service label, or one that has never been created, has nothing to target; use the stack-wide buttons. |
| `too many commands running at once` | The read limiter (`DASH_MAX_READS`, default 4) is saturated. Reads are deliberately concurrent, but not unbounded: each one forks `bash` and `docker compose` inside the socket-holding container. |
| Deploy log says `gh CLI not found - skipping provenance verification` | Expected from the dashboard; this image ships no `gh`. See "What it cannot do". Over SSH on a NAS that has `gh`, it means `gh` is not on that session's `PATH`. |
| Deploy over SSH fails with `GITHUB_OWNER not set` | The stack's `.env` is missing the key. Fill it in from `.env.example`; `homelab env check <stack>` lists every key left blank. |

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
