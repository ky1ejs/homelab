# `qnap/` - the SSH login banner and admin helpers

What you see when you `ssh admin@kyles-nas`: one line of host state, one line of
repo state, and a generated index of the helper commands, all of them already on
`PATH`.

```
kyles-nas  up 6 days, 3:12  1.4T free
repo main 6050794

helpers
  containers   what Container Station is running right now
  disks        free space on the data volume, and SMART health if smartctl is present
  helpers      reprint the login banner
  stacks       the compose stacks in this checkout and what each has running

/share/CE_CACHEDEV4_DATA/homelab/qnap/README.md  -  update with: homelab update
```

This is the read-only glance. [`bin/homelab`](../bin/homelab) is the real CLI
for the stacks, and `profile.sh` puts it on `PATH` too.

## How it works

**The reason any of this needs explaining is that QTS rebuilds its own root
filesystem on every boot.** `/etc`, `/root`, `/bin`, `/opt` and `/usr` come back
from firmware each time the NAS comes up, so the obvious approach - edit
`/etc/profile`, drop scripts in `/usr/local/bin` - produces a setup that works
until the next reboot and then silently vanishes. Every design choice below is
downstream of that one fact.

Three things persist:

| Location | What lives there |
|---|---|
| `/share/CE_CACHEDEV4_DATA/` | The data volume. The checkout, and everything in it. |
| `/share/homes/admin/` | The admin home directory, on that same volume. SSH keys. |
| The config partition | Partition 6 of the boot device. Reachable only by mounting it. Holds `autorun.sh`. |

So the chain is: **config partition -> `/etc/profile` -> the checkout.**

1. **`autorun.sh`** is copied onto the config partition by `bootstrap.sh`. QTS
   runs it as root at boot, if *Control Panel -> Hardware -> General -> "Run user
   defined processes during startup"* is enabled. Its only job is to append one
   line to `/etc/profile` pointing at the checkout.

   It appends unconditionally with no duplicate check, and that is correct rather
   than sloppy: `/etc/profile` is a fresh file from firmware every boot, so there
   is nothing to accumulate into. It is also deliberately tiny, because changing
   it means re-running `bootstrap.sh` to re-copy it, and that is the step that
   gets forgotten. Anything that might need to change lives in `profile.sh`,
   which is read live from the checkout.

2. **`profile.sh`** is sourced into every login shell. It exports `HOMELAB`, puts
   `qnap/bin` and `bin` on `PATH`, and prints the banner. Because it is *sourced*
   it contains no `set -e` and no `exit`: either would act on the user's own
   login shell, and the failure mode of getting that wrong is an account that
   cannot SSH in. Every step is written to be skippable, and re-sourcing it is a
   no-op.

   The banner is printed only for interactive shells (`case "$-" in *i*`).
   Without that test it would be prepended to the output of `ssh nas <cmd>` and
   injected into scp/rsync/sftp transfers, which breaks them outright.

3. **`motd.sh`** prints the banner. It runs on *every login*, so it never blocks
   and never fails: no network, no `git fetch`, no `docker` (a cold `docker ps`
   on this box can take seconds), no writes, and every optional command guarded.
   A banner that hangs turns `ssh nas` into a tool you stop reaching for.

4. **`qnap/bin/*`** are the helpers. Small, read-only, one job each.

### The helper index is generated

`motd.sh` builds the list by reading `qnap/bin/`. **Line 2 of every helper is its
one-line description**, and `motd.sh` extracts it with `sed -n '2s/^# *//p'`:

```sh
#!/bin/sh
# what Container Station is running right now
#
# Longer rationale goes here, from line 3 onward.
set -eu
```

Non-executable files are skipped, so a work-in-progress is not advertised.

A hand-maintained list would go stale silently, and the banner's whole value is
that it is trustworthy - a listed command that no longer exists is worse than no
list. For the same reason, **add helpers conservatively**: the banner presents
them as things that work.

### It reads git without git

Container Station gives QTS a `docker` binary but no `git`, and
[`README.md`](../README.md#getting-the-repo-onto-the-nas-without-host-git)
records why installing one via Entware is a bad trade - Entware is a documented
way to break the SSH `PATH`, which is how you reach `docker` in the first place.

So `motd.sh` reads branch, short SHA and "differs from origin" straight out of
`.git/` as plain files (loose refs, falling back to `packed-refs`), and nothing
in `qnap/` requires host git. If host git *does* happen to be installed, it is
used for exactly one extra fact: whether the tree is dirty.

"differs from origin" is a divergence, not a direction. Telling *behind* from
*ahead* means walking history, and being precise about it is not worth a slower
login; `homelab update` resolves it either way. Nothing here fetches - the
comparison is against the last-known `origin` tip on disk.

`profile.sh` does put `/opt/bin` on `PATH` if Entware is already installed, but
at the **end**, never the front. Entware breaks things by shadowing the busybox
and QTS binaries the rest of QTS expects; appending makes an already-installed
`opkg` tool reachable without that. This directory neither needs nor installs
Entware.

### Paths are not guessed

**QNAP volume names are not predictable.** `CACHEDEV1_DATA` is the common default
and is *not* correct on this NAS - the checkout is on `CE_CACHEDEV4_DATA` (the
`CE_` prefix means encrypted, auto-unlocking at boot), Tailscale is on
`CACHEDEV1_DATA`, and Container Station is on `CE_CACHEDEV2_DATA`. Find yours
with `ls -d /share/*/`.

The checkout path is a literal in exactly two files, `autorun.sh` and
`profile.sh`, because neither can discover it: `autorun.sh` runs from the config
partition, and `profile.sh` is sourced, so `$0` is the login shell rather than
the file. **`bootstrap.sh` compares the two and refuses to run if they disagree**,
and refuses again if the checkout is not where they say. Everything else -
`motd.sh` and every helper - derives the path from its own location.

The encrypted volume also means `autorun.sh` may run *before* the volume is
unlocked. That is harmless: the line it appends is guarded with `[ -f ... ]`
evaluated at **login** time, not at boot, so a boot that races the unlock still
produces a working `/etc/profile`.

`containers` and `stacks` find `docker` the same way - by asking `qpkg.conf`
about the Container Station package rather than assuming a volume.

### No secrets here

Nothing in `qnap/` reads a credential, and nothing should be added that keeps one
in the repo. If a helper ever needs a token, it reads it from a file under
`/share/homes/admin/` (mode `600`, owned by admin) and the expected path is
documented here at that point. The data volume persists across firmware updates,
which is exactly why it is the right place for a secret and the repo is not.

## Reinstating from scratch

Assume total loss: the NAS OS is gone and you are looking at a factory-fresh
QTS. This is the whole procedure.

1. **Enable SSH.** *Control Panel -> Network & File Services -> Telnet / SSH ->
   Allow SSH connection*. Then `ssh admin@<nas>`.

2. **Get the checkout back onto the data volume.** Find the volume first, because
   the name will not necessarily be the one below:

   ```sh
   ls -d /share/*/
   ```

   The repo is public, so there is no credential in this step and **no git is
   needed on the NAS** - the clone runs in a container:

   ```sh
   cd /share/CE_CACHEDEV4_DATA
   docker run --rm -v "$PWD:/git" -w /git -u "$(id -u):$(id -g)" -e HOME=/tmp \
       alpine/git clone https://github.com/ky1ejs/homelab.git
   ```

   `-u` matters: without it the clone lands owned by root and the stacks can no
   longer read their own checkout. After this, `homelab update` does the pulling.

   *If the volume name differs from `CE_CACHEDEV4_DATA`*, update the `HOMELAB`
   line in **both** `qnap/autorun.sh` and `qnap/profile.sh` and commit it;
   `bootstrap.sh` will stop you if you miss one.

   *If the repo has been made private since this was written*, it needs a
   **read-only deploy key** - not a personal key, and never pushed from the NAS.
   Put the private key at `/share/homes/admin/.ssh/id_ed25519_homelab` (`chmod
   600`), add a matching `Host github.com` / `IdentityFile` entry to
   `/share/homes/admin/.ssh/config`, and clone over SSH instead. **The private
   key is not in this repo.** It is kept in:

   > _`<FILL IN: 1Password vault / item name>`_

3. **Install the login hook.**

   ```sh
   sh /share/CE_CACHEDEV4_DATA/homelab/qnap/bootstrap.sh
   ```

   Safe to re-run. It verifies the paths agree, mounts the config partition,
   copies `autorun.sh` onto it, unmounts, and adds the same hook to the *running*
   `/etc/profile` so you do not have to reboot to see the result.

4. **Enable startup processes.** *Control Panel -> Hardware -> General -> "Run
   user defined processes during startup"*. **Without this the banner works now
   and disappears at the next reboot**, which is the failure mode most likely to
   go unnoticed. There is no supported CLI equivalent, which is why
   `bootstrap.sh` prints a reminder rather than setting it.

   **Re-check this after every firmware update** - it has been known to reset.

5. **Log out and back in.** The banner should appear. If it does not, run
   `sh /share/CE_CACHEDEV4_DATA/homelab/qnap/motd.sh` directly: it will say what
   is wrong far more usefully than a silent login does.

6. **Verify.** Work through [`TESTING.md`](TESTING.md).

The stacks themselves are a separate restore - see the root
[`README.md`](../README.md) and each stack's own README.

### The partial case: factory reset, disks intact

A factory reset wipes the config partition but **not** the data volume, so the
checkout is normally still there and steps 1-2 are already done. Confirm it:

```sh
ls /share/CE_CACHEDEV4_DATA/homelab/qnap/bootstrap.sh
```

If that exists, go straight to **step 3**. This is also the case after a
firmware update that resets the startup-processes toggle, where only **step 4**
is actually missing.

## Files

| File | |
|---|---|
| `autorun.sh` | Copied to the config partition. Runs at boot as root; hooks `/etc/profile`. |
| `profile.sh` | Sourced by `/etc/profile` at every login. `PATH`, `HOMELAB`, banner. |
| `motd.sh` | Prints the banner. Also `helpers`. |
| `bootstrap.sh` | One-shot installer. Idempotent. |
| `bin/` | The helpers. One per file, executable, line 2 is the description. |
| `TESTING.md` | The checklist to run on the NAS after any change here. |

Everything is `#!/bin/sh` POSIX, because the login shell on this box is busybox
`sh`, not bash. Nothing here depends on Entware. Output is ASCII-only, matching
`bin/homelab`'s contract: a QTS session may have no UTF-8 locale, and mojibake in
a banner reads as a broken system. Colour appears only when stdout is a terminal,
`TERM` is not `dumb`, and `NO_COLOR` is unset.

**These scripts are not covered by CI.** `shellcheck-bin.yml` is path-filtered to
`bin/**`, which does not match `qnap/bin/**`. Until that changes, `TESTING.md`'s
first check is the lint gate, and it is run by hand.
