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
password is kept in `.env` at all ([setup](#setup)).

**No `env_file`.** The other stacks pass all of `.env` to every service. This one
has four services from three authors, so each is handed only the variables it
names: the SMTP password is not in the hub's environment and the agent key is
not in the reporter's. The hub does hold an SMTP password for alert emails, in
its database; [setup](#setup) gives it its own app password so the two can be revoked
separately.

## Setup

Four things by hand, then one command. The command is re-runnable: the first
install and a restore are the same thing.

**By hand, first:**

1. **Two Gmail app passwords**
   ([app passwords](https://myaccount.google.com/apppasswords)), both for the
   sending account. One is for the weekly report and goes in `.env`. The other
   is for the hub's alert emails; you type it when setup asks, and it is stored
   only in the hub's database. Two rather than one means either can be revoked
   without breaking the other.

2. **`.env`**, on the NAS, in the checkout:

   ```sh
   cd monitoring && cp .env.example .env && chmod 600 .env
   ```

   Set `REPORT_FROM`, `REPORT_TO`, `SMTP_USERNAME` and `SMTP_PASSWORD` (the
   first app password). Check `MON_SUBNET` does not collide with an existing
   Docker network (`docker network ls`, then `inspect`). Leave `MON_HUB_KEY`
   and `REPORT_BESZEL_*` blank: setup writes them.

3. **Pull and verify the images:**

   ```sh
   homelab deploy monitoring
   ```

   After this, all three of the hub, the agent and the reporter restart in a
   loop, and that is expected. Docker creates the data directories root-owned,
   so the hub cannot open its database; the agent has no key, and the reporter
   no account. Setup fixes all three, which is why it must run as `admin`: it
   `chown`s the directories. The hub is published on loopback only, so nothing
   on the tailnet can reach it yet.

**Then:**

4. **Run setup**, as `admin`:

   ```sh
   homelab setup monitoring
   ```

   First it refuses anything it can see is wrong, before changing anything:
   a `MON_APP_URL` without an explicit port, or on 443 (the dashboard's); that
   port already serving something else; a Funnel on the node; the reporter
   image not pulled yet. Then it asks for your Tailscale login (defaulting to
   `DASH_ALLOWED_LOGINS`), your Beszel password twice (on a new hub, choose one
   and keep it in 1Password), and the hub's SMTP app password. **Do not leave
   that last one blank on a first install or a restore**: a hub with no mail
   settings sends no alerts, and setup will warn you that mail is off. Then it
   does, looking before each write:

   | Step | Where it lives afterwards |
   |---|---|
   | Data directories under `MON_DATA_HOST_PATH`, owned by `APP_UID:APP_GID` | the NAS |
   | Your account, via the first-run page: admin and `/_/` superuser | the hub |
   | The `kyles-nas` system, on `/beszel_socket/beszel.sock` | the hub |
   | The hub's public key | `MON_HUB_KEY` in `.env` |
   | <a id="the-reporters-account"></a>The reporter's account: `reporter@homelab.invalid`, role `readonly`, a generated password you never see | the hub, and `REPORT_BESZEL_*` in `.env` |
   | The hub's SMTP settings, from `.env` plus the password you typed | the hub |
   | Alert emails go to `REPORT_TO` | the hub |
   | The alerts in [`alerts.json`](alerts.json), exactly | the hub |
   | `tailscale serve --https 8443`, **last**: only once the hub is claimed | the NAS's tailnet node |
   | `DASH_MONITORING_URL` in `dashboard/.env`, if blank | written, not applied |

   Then it brings the whole stack up and prints `homelab status monitoring`.
   Run it again whenever you like. On a configured hub, with the SMTP prompt
   left blank, it changes nothing and says so; typing an SMTP password
   re-applies it.

   **The order is the security.** Until the hub has an account, whoever opens
   its first-run page first becomes its admin. So the hub stays on loopback,
   reachable from nothing on the tailnet, until setup has claimed it over the
   compose network; Serve is the last step. A run interrupted at a prompt
   leaves an unclaimed hub that only the NAS itself can reach.

   Neither secret is written to `.env` or appears on a command line; the hub
   keeps its SMTP password in its own database. They reach the reporter's
   container on stdin, which is also why setup runs there: it talks to the hub
   over the compose network with your password, and never relies on the
   trusted identity header. Setup runs only from an image `homelab deploy`
   already pulled and verified, since that image is handed your superuser
   password.

   `fs-probe/` must stay empty and must be on the data volume. It is what the
   "disk data" graph measures. If it is on the same filesystem as Docker's own
   data root, the agent drops it as a duplicate of the root disk. On this NAS,
   Container Station is on `CE_CACHEDEV2` and the data is on `CE_CACHEDEV4`, so
   it is not.

5. **Check the report**, then send one:

   ```sh
   docker compose -p monitoring run --rm --no-deps weekly-report -dry-run
   docker compose -p monitoring run --rm --no-deps weekly-report -once
   ```

   On a fresh install the dry run says there are no two-hour samples yet. That
   is correct for the first two hours.

6. **Show the link on the dashboard:** `homelab restart dashboard`. Setup
   writes the setting but does not restart another stack, because the root
   README's shared contract forbids that as a side effect.

**By hand, last:**

7. **The dead-man's switches** (optional, recommended). Create two checks at
   [healthchecks.io](https://healthchecks.io) (free tier):

   | Check | Period | Grace | `.env` |
   |---|---|---|---|
   | Hub heartbeat | 5 min | 10 min | `MON_HEARTBEAT_URL` |
   | Weekly report | 7 days | 1 day | `REPORT_PING_URL` |

   Then run `homelab restart monitoring`. The first check emails you when the
   NAS goes quiet. The second emails you on Tuesday if Monday's report did not
   go out. These stay manual because scripting them would mean keeping a
   Healthchecks.io API key here.

8. **QTS's hardware alerts.** *Notification Center > System Notification
   Rules*: email (or the Qmanager push app) for *Storage & Snapshots* (SMART,
   volume status, capacity) and *System* (temperature, fan), at Warning and
   Error.

### The alerts

[`alerts.json`](alerts.json) is the list, and setup makes the hub match it
exactly: an alert tuned in the UI and not in the file is removed on the next
run. Change a threshold with a commit, then re-run setup once the new image
is deployed.

The thresholds are the report's ACT lines, so an alert means "act" and the
weekly email covers the slower WATCH drift. `alerts_test.go` fails if the two
disagree, and if a name is not one Beszel knows; the hub accepts any name and
an unknown one would never fire. *Container Health* is the one alert with no
report line. It catches the `healthcheck:` blocks in the other stacks
(`vault-mcp`, the dashboard) failing, which the dashboard otherwise shows only
when you open it.

The dashboard's link to this page goes one way. Beszel is an upstream image
with no setting for custom links in 0.21.0, and patching it is not worth a
fork. The weekly email links here; bookmark the dashboard.

## Operating it

```sh
homelab status monitoring             # containers, the Serve proxy, blank keys
homelab setup monitoring              # re-apply the repo's config; also the restore
homelab logs monitoring weekly-report # when the Monday email did not arrive
homelab deploy monitoring             # pulls all four images; verifies provenance of this repo's one
```

**Upgrading Beszel is a compose-file change**, because its images are pinned
by tag and digest. Move the hub and the agent together: they speak a protocol
to each other, and upstream versions them as a pair. Read the
[changelog](https://github.com/henrygd/beszel/releases) first, for three
reasons:

- the stats JSON keys `beszel.go` decodes are Beszel's internal format, so a
  rename shows up as a report full of zeros rather than as an error (0.21.1
  already deprecates the GB memory fields in favour of byte arrays);
- the socket proxy's allowlist is exactly the calls the agent makes at the
  pinned version, so a new call shows up as a missing graph, with the refusal
  in `homelab logs monitoring docker-proxy`;
- `setup.go` uses Beszel's first-run, alert and settings endpoints, and
  `alerts_test.go` holds its list of alert names.

The e2e workflow runs setup and the trust-boundary checks against whatever the
compose file pins, so a PR that bumps the digests tests the upgrade before it
merges. `bash monitoring/scripts/e2e.sh` runs the same thing on any Docker host.

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
- **Keep its history.** The hub's database under `MON_DATA_HOST_PATH/hub`
  holds 30 days of samples, and it is not in the off-site backup. Everything
  else in it is reproducible: lose the directory and `homelab setup
  monitoring` restores the account, system, reporter, mail settings and
  alerts, and gives the agent the new hub's key. It needs two things from
  you: your Beszel password, and the hub's SMTP app password, since that one
  lived only in the lost database (revoke the old one and make a new one if
  you no longer have it). CI runs this restore on every change: it empties
  the hub directory, reruns setup, and checks that the key is replaced, the
  agent reconnects and the reporter's new password works.
- **Catch up on a missed Monday.** If the reporter is down at the scheduled
  minute (a reboot, a redeploy at 08:00), that week is skipped, not sent late.
  Nothing records the last run, so catching up on start could double-send
  after any restart. `REPORT_PING_URL` is what turns a skipped week into an
  alert.
- **Show host network traffic.** The agent is not in the host's network namespace,
  by design ([why](#trust-boundary)). Per-container traffic still shows.
