# monitoring

How the NAS is doing, a page to look at it whenever you like, alerts when it is
not doing well, and an email every Monday either way.

```
https://kyles-nas.tail3df177.ts.net:8443/           from anywhere on the tailnet
```

The question this stack exists to answer is the one the homelab keeps raising
as it takes on more services: **is this box still big enough, and when should
the approach change?** The dashboard answers it when you look. The alerts answer
it when something crosses a line. The weekly email answers it when you did not
think to look, which is most weeks.

| Layer | What | Fires when |
|---|---|---|
| Dashboard | [Beszel](https://beszel.dev)'s hub: CPU, memory, swap, load, disk, I/O, temperature, and per-container CPU, memory and network traffic, with history | You open it |
| Alerts | Beszel's threshold alerts, emailed | A metric stays over a threshold for a set time, or the agent stops reporting |
| Weekly report | `homelab-report`, this repo's own code | Monday 08:00, with a verdict: all clear, or what to watch and what to act on |
| Dead-man's switch | Healthchecks.io, pinged by the hub and by the report | The pings **stop**: the NAS is off, offline, or the hub is dead |
| Hardware | QTS Notification Center | SMART warnings, a degraded volume, fan failure, overheating |

The last two matter because the first three run on the box they are watching. A
NAS that has lost power sends no alerts; Healthchecks.io notices the silence.
SMART is QTS's job because QTS already does it, and the agent here runs without
the root access it would need ([why](#trust-boundary)).

## What the weekly report says

Subject first, so the verdict is readable from the notification:

```
[homelab] weekly: 1 to act on, 2 to watch
```

Then each finding with what it means for this box, then the numbers: CPU,
iowait, 15-minute load against the thread count, memory and swap, each disk with
its 30-day growth rate and a days-until-full projection, the hottest sensor, and
the top five containers by memory. Once there is any history from before the
week, the CPU and memory averages are compared against it, and the email says
how many weeks that comparison covers.

The thresholds are constants in [`report.go`](report.go), not `.env` tunables. They
are the judgement the report exists to apply, and a threshold quietly raised in
an untracked file is a report that stops saying the thing it was built to say.

| Area | WATCH | ACT | What it usually means here |
|---|---|---|---|
| Memory | average 75%, or peak 90% | average 85%, or peak 95% | Lower `AGENT_MEM_LIMIT`/`RESEARCH_MEM_LIMIT`, move a service off the box, or add RAM |
| Swap | peak 25% in use | peak 50% | Same as memory. A little swap is normal; this much is not |
| CPU | average 50% | average 70% | Find the top consumer; move it, or accept slower agents |
| Load (15 min) | 0.7 per thread | 1.0 per thread | Work is queueing. With high iowait, it is the disks |
| iowait | average 10% | average 20% | Disks are the bottleneck. SSD helps more than a faster CPU |
| Disk | 80% full, or full within 180 days | 90% full, or within 60 days | Find what grew before buying disks |
| Temperature | 70 C | 80 C | Fans, dust, placement |
| Trend | memory up 10 points, or CPU up 15, on the prior weeks | | Climbing. Fine if a new service explains it |
| Coverage | under 90% of the week's samples present | | The agent or hub was down; the numbers cover less than a week |

**Why the memory lines are low.** This host already overcommits: the stacks'
`mem_limit` values add up to more than the RAM that is free
([`README.md`](../README.md#the-host)). Average 85% leaves no room for both agents
to be busy at once, and when that happens the kernel's OOM killer picks the
victim, not you.

**What none of these numbers can show.** Some images will not run on this CPU at
all: the J4125 has no AVX2, and a service built for it exits with `illegal
instruction` before it uses any memory. That is a "change approach" signal no
metric produces. The fix is different hardware or a hosted service, not tuning.

### Limits of the numbers

- **They are two-hour averages.** Beszel keeps a per-window peak for host CPU
  and memory, and the report uses it. It keeps none for load, swap,
  temperature or container memory, so for those the report shows the busiest
  two hours, labelled as such. A ten-minute spike to a container's
  `mem_limit` reads at about a twelfth of its height.
- **The first weeks are thin.** Two-hour samples start two hours after the
  agent connects. The comparison against prior weeks and the disk growth rate
  both come from the 30-day series, so they firm up as history accumulates; an
  early trend finding can be against a fraction of a week, and says so.
- **A filesystem that stops reporting is an ACT finding**, not a missing line.
  The agent drops a filesystem it cannot read, so absence is the signal.
- **Growth ignores samples taken at a different size.** The hub averages a
  filesystem over every sample in a rollup window, including ones it was absent
  from, so a partly covered window reports a smaller disk with less on it.
  Those samples are skipped rather than read as growth.
- **Disk growth needs three days of history** before the report projects
  anything.
- **No alert history.** Beszel stores fired alerts per user, and the reporter's
  readonly account cannot read yours. The report computes its own findings
  from the stats instead, which is also why its thresholds and your alert
  thresholds can differ.

## Trust boundary

**The Docker socket is held by a proxy with no network and a four-route
allowlist.** The agent wants the socket for per-container CPU and memory. It
does not get it. `docker-proxy` mounts it and hands the agent a filtered socket
that answers exactly these, GET only:

```
/version   /info   /containers/json   /containers/<12-64 hex>/stats
```

Everything else is refused at the proxy, not merely unused by the agent:
`inspect` (which returns every container's environment, including this repo's
tokens), logs, exec, image routes, and every write. One visible consequence:
Beszel's *Container Health* alert normally pastes a log excerpt into the email.
Here the log fetch is refused, the hub logs a warning, and the email arrives
without it. That is the intended result, because `vault-mcp`'s logs name notes.
Checked against a running stack, not inferred from the config:

| Request | Result |
|---|---|
| `GET /containers/json` | 200 |
| `GET /containers/<id>/stats` | 200 |
| `GET /containers/<id>/json` (inspect) | 403 |
| `GET /containers/<id>/logs` | 403 |
| `GET /v1.47/containers/<id>/json` | 403 |
| `GET /containers/../containers/<id>/json` | 403 |
| `GET /images/json` | 403 |
| `POST /containers/<id>/restart` | 405 |

This is the socket proxy that
[`DECISIONS.md`](../obsidian-vault/DECISIONS.md#the-dashboard-and-the-docker-socket)
rejected for the dashboard, and the reason it fits here is the reason it did not fit there: it
is *"correct for a dashboard that only reads"*. Deploying needed the dangerous
half of the API; reading container stats does not.

**The agent has no network.** `network_mode: none`: it listens on a unix socket
in a directory only the hub and the agent mount, and reads Docker through
another. Only a hub holding the private key that matches `MON_HUB_KEY` can
read from it.

This costs the host network graphs, and that is the trade. Beszel's own
example runs the agent on the host network to see the real NICs, but the host
network includes the host's loopback, and on loopback both this hub and the
dashboard trust an identity header. A host-networked agent could have used
the dashboard's `logs` and `deploy` verbs as you. CPU, memory, load, disk and
temperature are host-wide from inside a container, and per-container network
traffic still comes from the Docker API.

**The agent runs unprivileged.** Everything it collects is world-readable under
`/proc` and `/sys`. The cost is SMART data, which needs root and raw device
access. QTS already monitors SMART, so the root access buys nothing here.

**The agent cannot read the vault.** Disk usage for the data volume comes from
`statfs` on an empty directory that lives on it (`fs-probe/`), mounted
read-only. The agent is third-party code; mounting the share itself to measure
its free space would give it every note.

**Identity at the door, the dashboard's way.** `tailscale serve` adds
`Tailscale-User-Login`, and the hub signs in the Beszel user with that email.
A tailnet user with no matching Beszel user gets the ordinary login page.

Two things make the header a credential rather than a suggestion. The
listener is published on loopback only, as with
[`dashboard/`](../dashboard/README.md#who-can-press-the-buttons). And the hub
honours the header **only from the compose network's gateway**
(`TRUSTED_PROXY_IPS`), which is where connections through that loopback
publish arrive from. Without the second, the hub believed the header from
every container on its network. Checked against a running stack:

| Request with `Tailscale-User-Login: <you>` | Result |
|---|---|
| Through the loopback publish, `127.0.0.1:8091` | 200, signed in as you |
| From a container on `monitoring_default`, to `beszel:8090` | 401 |
| Through the loopback publish with an unknown email | 401 |

What remains is what the dashboard accepts too: a process on the NAS itself
that connects to `127.0.0.1:8091` can claim to be you. Your account there is an
admin, which can manage systems and alert targets, mint agent tokens, and read
graphs. It cannot reach the vault or act on any container.

If Docker on the NAS ever stops sourcing published-port connections from the
gateway, the header is ignored and you get the password login. It fails
closed. Check after a Container Station upgrade by loading the page.

**A separate origin from the dashboard.** Same hostname, port 8443 rather than
a path under the dashboard's 443. The port makes it a different origin, so the
browser stops Beszel's JavaScript from scripting the dashboard's pages. On a
shared origin it could have sent the dashboard's required `X-Homelab-Action`
header itself, which is the dashboard's only CSRF defence. Cookies are *not*
separated by port, so this protects nothing cookie-based; the dashboard sets
none.
[`DECISIONS.md`](../obsidian-vault/DECISIONS.md#identity-and-the-trust-it-extends)
has the rejected alternative.

**The reporter's credential is worth little.** It logs in as a Beszel user
with the `readonly` role, and refuses to run as any other role. The costly
secret in `.env` is the SMTP app password, which can read the mailbox it
belongs to; use a dedicated sending account if that matters. No Beszel admin
password is kept in `.env` at all ([step 3](#setup)).

**No `env_file`.** The other stacks pass all of `.env` to every service. This one
has four services from three authors, so each is handed only the variables it
names: the SMTP password is not in the hub's environment and the agent key is
not in the reporter's. The hub does hold an SMTP password for alert emails, in
its database; step 7 gives it its own app password so the two can be revoked
separately.

## Setup

On the NAS, in the checkout.

1. **Create the data directories**, on the encrypted volume, owned by the
   account everything runs as:

   ```sh
   mkdir -p /share/CE_CACHEDEV4_DATA/monitoring/{hub,agent,socket,fs-probe}
   chown -R 1002:100 /share/CE_CACHEDEV4_DATA/monitoring
   ```

   `fs-probe/` must stay empty and must be on the data volume. It is what the
   "disk data" graph measures. If it is on the same filesystem as Docker's own
   data root, the agent drops it as a duplicate of the root disk. On this NAS,
   Container Station is on `CE_CACHEDEV2` and this is on `CE_CACHEDEV4`, so it
   is not.

2. **Fill in `.env`**:

   ```sh
   cd monitoring && cp .env.example .env && chmod 600 .env
   ```

   Set the `REPORT_FROM`, `REPORT_TO` and `SMTP_*` values. For Gmail, the
   password is an [app password](https://myaccount.google.com/apppasswords),
   not your real one. Check `MON_SUBNET` does not collide with an existing
   Docker network (`docker network ls`, then `inspect`). Leave `MON_HUB_KEY` and
   `REPORT_BESZEL_*` blank for now.

3. **Deploy, put Serve in front, and claim the hub straight away:**

   ```sh
   homelab deploy monitoring
   tailscale serve --bg --https 8443 http://127.0.0.1:8091
   ```

   Open `https://kyles-nas.tail3df177.ts.net:8443/` and create the account the
   first-run page asks for, **with your Tailscale login as the email** (the
   same value as `DASH_ALLOWED_LOGINS`) and a generated password kept in
   1Password. That makes you an admin and the `/_/` superuser, and from then on
   Serve signs you in without the password. Do it now: until an account exists,
   anyone on the tailnet who opens the page can claim it.

   Until steps 4 and 5 are done, `beszel-agent` restarts in a loop (no key) and
   `weekly-report` restarts in a loop (no account). Both are expected.

4. **Add the system and give the agent the hub's key.** In the hub, choose
   *Add System*. Name it **`kyles-nas`** (the reporter looks it up by that
   name) and set the host to **`/beszel_socket/beszel.sock`**. Copy the public
   key the dialog shows into `MON_HUB_KEY`, save the system, and run
   `homelab restart monitoring`. Within a minute the system shows as up.

   This is a click rather than a file in git on purpose: Beszel can sync
   systems from a `config.yml`, but with no users yet that sync fails and the
   hub restarts in a loop, so the first-run page in step 3 never appears.

5. <a id="the-reporters-account"></a>**Create the reporter's account.** Open
   `https://kyles-nas.tail3df177.ts.net:8443/_/` and sign in as the superuser
   (your step 3 email and password). Under *Collections > users*, add a
   record: any email (`reporter@homelab.invalid` is fine, since nothing is ever
   sent to it), a generated password, **role `readonly`**, verified. Put both
   in `REPORT_BESZEL_EMAIL` and `REPORT_BESZEL_PASSWORD`.

   ```sh
   homelab restart monitoring
   ```

6. **Check the report before trusting it**, then send one:

   ```sh
   docker compose -p monitoring run --rm --no-deps weekly-report -dry-run
   docker compose -p monitoring run --rm --no-deps weekly-report -once
   ```

   On a fresh install the dry run says there are no two-hour samples yet. That
   is correct for the first two hours. What it proves is that the login, the
   readonly check and the queries all work.

7. **Turn on alert emails.** Create a **second** app password for the same
   account, named for the hub. In `/_/`, under *Settings > Mail settings*,
   enable SMTP with the same host, port and username as `.env` and that second
   password, and set the sender address. Two passwords rather than one shared
   means either can be revoked without breaking the other, and the hub's copy
   lives in its database rather than in `.env`. Then in the Beszel UI, under *Settings > Notifications*,
   add your email address. Use *Test notification* there.

8. **Set the alerts.** Open the system, select the bell, and set these. They
   are deliberately the report's ACT lines, so an alert means "act" and the
   weekly email covers the slower WATCH drift:

   | Alert | Threshold | For |
   |---|---|---|
   | Status | down | 10 min |
   | Memory Usage | 85% | 30 min |
   | CPU Usage | 70% | 60 min |
   | CPU I/O Wait | 20% | 60 min |
   | Load Average 15m | 4 | 60 min |
   | Disk Usage | 90% | 10 min |
   | Temperature | 80 C | 15 min |
   | Container Health | unhealthy | 10 min |

   *Container Health* is the one alert with no line in the report. It catches
   the `healthcheck:` blocks in the other stacks failing (`vault-mcp`, the
   dashboard, and both agents — `vault-claude` and `vault-research` turn
   unhealthy when `claude remote-control` keeps failing, such as after the
   login expires), which the dashboard otherwise shows only when you open it.

9. **Add the dead-man's switches** (optional, recommended). Create two checks at
   [healthchecks.io](https://healthchecks.io) (free tier):

   | Check | Period | Grace | `.env` |
   |---|---|---|---|
   | Hub heartbeat | 5 min | 10 min | `MON_HEARTBEAT_URL` |
   | Weekly report | 7 days | 1 day | `REPORT_PING_URL` |

   Then run `homelab restart monitoring`. The first check emails you when the NAS
   goes quiet. The second emails you on Tuesday if Monday's report did not go
   out.

10. **Set QTS's hardware alerts**, if you have not already. *Notification
    Center > System Notification Rules*: email (or the Qmanager push app) for
    *Storage & Snapshots* (SMART, volume status, capacity) and *System*
    (temperature, fan), at Warning and Error.

11. **Link the two pages.** In `dashboard/.env`, set
    `DASH_MONITORING_URL=https://kyles-nas.tail3df177.ts.net:8443/` (the
    `.env.example` default), then `homelab restart dashboard`. The dashboard's
    header gains a *monitoring* link. The link goes one way only: Beszel is an
    upstream image, and 0.21.0 has no setting for custom links, and patching it is not
    worth a fork. The weekly email links to Beszel; bookmark the dashboard.

## Operating it

```sh
homelab status monitoring             # containers, the Serve proxy, blank keys
homelab logs monitoring weekly-report # when the Monday email did not arrive
homelab deploy monitoring             # pulls all four images; verifies provenance of this repo's one
```

**Upgrading Beszel is a compose-file change**, because its images are pinned
by tag and digest. Move the hub and the agent together: they speak a protocol
to each other, and upstream versions them as a pair. Read the
[changelog](https://github.com/henrygd/beszel/releases) first, for two reasons:

- the stats JSON keys `beszel.go` decodes are Beszel's internal format, so a
  rename shows up as a report full of zeros rather than as an error (0.21.1
  already deprecates the GB memory fields in favour of byte arrays);
- the socket proxy's allowlist is exactly the calls the agent makes at the
  pinned version, so a new call shows up as a missing graph, with the refusal
  in `homelab logs monitoring docker-proxy`.

```sh
docker buildx imagetools inspect henrygd/beszel:<version>
docker buildx imagetools inspect henrygd/beszel-agent:<version>
```

## What it cannot do

- **Alert on itself being off.** That is what the Healthchecks.io heartbeat is
  for, and why it is not on the NAS.
- **SMART.** The agent runs unprivileged. QTS does this.
- **Show memory against each container's limit.** The proxy refuses `inspect`,
  which is where limits live. The container table shows usage only, so compare
  it against the `mem_limit` lines in each stack's compose file.
- **Keep its state in git.** Users, the system entry, alert thresholds, SMTP
  settings and the hub's SSH keypair all live under `MON_DATA_HOST_PATH/hub`,
  and none of it is in the off-site backup. Losing that directory costs steps
  3, 4, 5, 7 and 8 above, and the history: the new hub generates a new key,
  which the agent rejects until `MON_HUB_KEY` is updated.
- **Catch up on a missed Monday.** If the reporter is down at the scheduled
  minute (a reboot, a redeploy at 08:00), that week is skipped, not sent late.
  Nothing records the last run, so catching up on start could double-send
  after any restart. `REPORT_PING_URL` is what turns a skipped week into an
  alert.
- **Show host network traffic.** The agent is not in the host's network namespace,
  by design ([why](#trust-boundary)). Per-container traffic still shows.
