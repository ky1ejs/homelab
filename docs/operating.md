# Operating it

[`bin/homelab`](../bin/homelab) is the entry point when you are SSH'd into the NAS.
It wraps the per-stack scripts rather than replacing them — `deploy` delegates to
a stack's own `scripts/deploy.sh` where one exists, so `--sync-only` and the
re-pair warning keep working.

The mac studio has its own `homelab` (`hl`), with `status` and `logs` too:
[`mac-studio.md`](mac-studio.md#the-homelab-command).

```sh
bin/homelab stacks                 # the stacks this CLI can act on
bin/homelab status                 # every stack: containers, images, snapshot and voice freshness
bin/homelab deploy vault-mcp       # pull, verify provenance, recreate
bin/homelab env check vault-mcp    # .env mode and any key left blank
bin/homelab token rotate           # new MCP_TOKEN, recreate, print the header to paste into Claude
bin/homelab setup monitoring       # configure the monitoring hub from the repo; re-runnable with no password, and the restore
bin/homelab url                    # the connector URL the Funnel is actually serving
bin/homelab url gmail-mcp          # the same, for the Gmail connector
bin/homelab attach                 # the vault-claude tmux session
bin/homelab attach research        # the vault-research tmux session
bin/homelab install                # set up the login shell: `homelab` or `hl` from anywhere, `homelab cd`
bin/homelab autorun install        # redo `install` at every boot, from QTS's autorun.sh

bin/homelab update                 # git pull this checkout
bin/homelab git status             # git against this checkout
bin/homelab snapshots log --author='Claude Voice'   # the vault's own history
```

**Every command takes an explicit stack, and there is no `--all`** — the [shared
contract](shared-contract.md) says never restart another stack's containers, and the cheapest
way to honour that is to make it unexpressible.

## Running it from any directory

Run this once, from the checkout:

```sh
bin/homelab install
```

It adds three lines to your login profile and tells you which file it wrote:
a marker, `HOMELAB` set to this checkout, and a line that sources
[`dotfiles/profile.sh`](../dotfiles/profile.sh). Open a new SSH session (or `.`
the file it names) and you get:

- `homelab status` and every other command from any directory, because `bin/`
  is on the **end** of `PATH`
- `hl` as a short name for `homelab`
- `homelab cd` to jump into the checkout, and `homelab cd vault-mcp` into a
  stack's directory
- `$HOMELAB`, the checkout's path, for anything else

A second run changes nothing. The setup itself lives in `dotfiles/profile.sh`,
not in your profile, so it is versioned here and `homelab update` changes it
without running `install` again. `homelab cd` is a shell function in that file
because a command cannot change the directory of the shell that ran it; before
`install`, the script tells you the `cd` to type instead.

- **Which profile.** `~/.profile`, which is what the QTS admin shell (busybox
  `ash`) reads. If your shell is bash and `~/.bash_profile` or `~/.bash_login`
  exists, it writes that instead, because bash then never reads `~/.profile`.
  zsh and fish are refused with the path to add yourself.
- **Why `PATH` and not a symlink in `/usr/local/bin`.** The QTS root filesystem
  is a RAM disk rebuilt at boot, so a link there is gone after a restart. The
  script also resolves its own symlinks now, so a link from somewhere that *does*
  persist works too.
- **It does not survive a restart on this host**, so run `homelab autorun
  install` once as well. See [Surviving a restart](#surviving-a-restart).
- **Login shells only.** `ssh nas homelab status` still fails, for the same
  reason `ssh nas docker ps` does: a non-interactive session reads no profile.
  A nested `bash` started from your login has `PATH` but not `hl` or
  `homelab cd`, because shells do not pass functions to their children.
  See [`host.md`](host.md).

## Surviving a restart

The admin's login profile is on the QTS RAM disk, so a restart takes `homelab`,
`hl` and `homelab cd` with it. That was checked on 2026-11-23, after a restart
left `homelab` missing. [`host.md`](host.md) records it next to the other
`PATH` trap. `homelab autorun` puts the setup back at every boot, through
the one hook QNAP supports: `autorun.sh` on the flash config partition.

It takes two steps, once. First, in the QTS web UI, turn on **Control Panel >
Hardware > General > Run user defined processes during startup**. QTS ignores
`autorun.sh` while that is off, and the command reads the setting but leaves it
to you, because the setting decides whether QTS runs root code at boot. Then,
from an SSH session as admin:

```sh
homelab autorun install
```

It runs `homelab install` for this session, then mounts the config partition,
adds a marked block to `autorun.sh` and unmounts it again. At boot the block
waits up to a day for the checkout's volume to mount, then runs
`homelab install` with the `HOME` and `SHELL` you ran the command with. A
second run changes nothing, and lines already in `autorun.sh` stay where they
are. The block goes straight after the shebang, so an `exit` further down
cannot skip it. If the checkout ever moves, run it again from the new one.

```sh
homelab autorun status
```

`status` shows the setting, whether the block is there and for which checkout,
and the output from this boot in `/tmp/homelab-autorun.log`. `remove` takes the
block out, and deletes `autorun.sh` if the block was all it held. `block`
prints the lines without writing them, for an `autorun.sh` you manage by hand.

- **Finding the partition.** Newer models name it in `/etc/platform.conf`.
  Older x86 models like this one use partition 6 of the boot device that
  `/sbin/hal_app` reports. The rules follow OneCD's
  [create-autorun](https://github.com/OneCDOnly/create-autorun), which tracks
  QNAP's models. One check is stricter than there: a partition with neither
  `uLinux.conf` nor `.sys_update_time` on it is refused, and nothing is written.
- **Not handled:** ubifs models, QuTS hero, and an `autorun.sh` that is a link,
  as create-autorun makes it. Each is refused with the reason, and
  [QNAP's article](https://www.qnap.com/en-us/how-to/faq/article/running-your-own-application-at-startup)
  covers the edit by hand.
- **Untested until the next restart.** CI checks the edit to `autorun.sh` and
  runs the block under a POSIX `sh` against a stand-in directory. Two things
  need the NAS itself: the partition rules on this model, and whether QTS
  lets a background process from `autorun.sh` outlive the script. After the
  next restart, check that `hl` works in a new SSH session, then read
  `homelab autorun status`.

## Getting the repo onto the NAS without host git

Container Station gives QTS a `docker` binary but no `git`, and **installing one
via Entware is a bad trade**: Entware is a well-documented way to break the SSH
`PATH`, and the `PATH` is how you reach `docker` in the first place.

The CLI handles this itself — `homelab git`, `homelab update` and
`homelab snapshots` use host git when it exists and a pinned `alpine/git`
container when it does not. **So the NAS never needs git installed.**

The one thing the CLI cannot do is the *initial* clone, because it lives inside
the repo it would be cloning. That one stays a one-liner (the repo is public, so
no credential is involved):

```sh
cd /share/CE_CACHEDEV4_DATA
docker run --rm -v "$PWD:/git" -w /git -u 1002:100 -e HOME=/tmp \
    alpine/git clone https://github.com/ky1ejs/homelab.git
```

`-u 1002:100` matters, and it is a number rather than `$(id -u)` on purpose: the
QTS SSH account is `admin`, which is **uid 0**, so `$(id -u)` clones as root.
That is what this line used to say, and it is how the first clone ended up
root-owned. The checkout belongs to the account the stacks run as (see
[identity 1002](../obsidian-vault/DECISIONS.md#identity-1002-not-1000)), and the
dashboard's updater refuses to write a root-owned one.

An existing root-owned clone is fixed in place, with nothing re-cloned:

```sh
chown -R 1002:100 /share/CE_CACHEDEV4_DATA/homelab
```

After that, `bin/homelab update` does the pulling, and it runs git **as the
checkout's owner** whoever you are logged in as, so a root SSH session does not
hand ownership back to root one pull at a time.

**Nothing else needs host git either.** Every snapshot commit happens inside a
container whose image already has it, and `homelab status` and
`homelab snapshots` borrow git from any running container that mounts
`/snapshots` when the host has none.

Output is ASCII-only and colour is dropped when stdout is not a terminal, so it
stays readable on a QTS session with no UTF-8 locale and pastes cleanly into a
chat window. Both properties are asserted in CI.
