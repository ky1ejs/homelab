# `qnap/` verification checklist

Run this on the NAS after any change in `qnap/`, and in full after a restore.

These scripts run at **login**, which makes their failure modes unusually
expensive: a broken `profile.sh` can lock you out of SSH, and a broken `motd.sh`
can corrupt `scp`/`sftp` transfers that only look like they succeeded. Checks 1
and 4 are the two that catch those, and neither takes a minute.

**Keep a second SSH session open** while testing anything that touches
`profile.sh`. If a change breaks login, that already-authenticated session is how
you undo it.

## 1. Lint and syntax (on the Mac, before pushing)

Not covered by CI - `shellcheck-bin.yml` is path-filtered to `bin/**`, which does
not match `qnap/bin/**`. Same pinned image and flags the workflow uses, with
`--shell=sh` because these are POSIX, not bash:

```sh
cd <repo>
for f in qnap/*.sh qnap/bin/*; do sh -n "$f" || echo "SYNTAX FAIL $f"; done

docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:v0.11.0 \
    --severity=style --shell=sh \
    qnap/autorun.sh qnap/bootstrap.sh qnap/motd.sh qnap/profile.sh qnap/bin/*
```

- [ ] `sh -n` clean on every script
- [ ] `shellcheck` exits 0 (two `disable=` directives are deliberate and
      documented in place; do not add more without a reason in a comment)
- [ ] ASCII only:
      `LC_ALL=C grep -rn '[^ -~\t]' qnap/` prints nothing
- [ ] Every file in `qnap/bin/` is executable and has a `# description` on line 2:
      `for f in qnap/bin/*; do test -x "$f" || echo "not executable: $f"; \`
      `sed -n '2p' "$f" | grep -q '^# .' || echo "no line-2 description: $f"; done`

## 2. Install

```sh
sh /share/CE_CACHEDEV4_DATA/homelab/qnap/bootstrap.sh
```

- [ ] Reports the checkout path and installs `/tmp/config/autorun.sh`
- [ ] Unmounts on the way out: `mount | grep /tmp/config` prints nothing
- [ ] **Re-run it.** Second run is clean, says "already hooked", and
      `grep -c 'qnap/profile.sh' /etc/profile` still prints `1`
- [ ] Run it from another directory (`cd / && sh /share/.../qnap/bootstrap.sh`) -
      it resolves the checkout from its own path, so this behaves identically
- [ ] As a non-root user it refuses with a usable message

## 3. The banner

```sh
ssh admin@kyles-nas
```

- [ ] Banner appears on login
- [ ] Line 1 shows a real hostname, a plausible uptime, and free space that
      matches `df -h /share/CE_CACHEDEV4_DATA`
- [ ] Line 2 shows the branch and short SHA that `homelab git log -1` agrees with
- [ ] Total output is under ~15 lines
- [ ] No mojibake, no box-drawing characters, no colour codes visible as literal
      escapes
- [ ] It returns immediately. If login ever feels slower after a change here,
      that is a regression, not a mood: something in `motd.sh` started touching
      the network or the docker daemon

## 4. It stays out of the way when it should

This is the check that protects file transfers, and the one worth being fussy
about.

- [ ] `ssh admin@kyles-nas uptime` prints **only** uptime output - no banner
- [ ] `ssh admin@kyles-nas 'echo hi'` prints exactly `hi`
- [ ] `scp somefile admin@kyles-nas:/tmp/` succeeds
- [ ] `sftp admin@kyles-nas` connects and reaches a prompt

## 5. The helpers

From an arbitrary working directory, because each resolves the checkout from its
own path rather than from `$PWD`:

```sh
cd / && helpers && containers && disks && stacks
```

- [ ] `helpers` reprints the banner identically
- [ ] `containers` lists running containers; matches `docker ps`
- [ ] `stacks` lists `dashboard`, `obsidian-vault` and `vault-mcp`, each with its
      compose state - and agrees with `homelab status`
- [ ] `disks` shows the data volume, and either SMART lines or the
      "smartctl not found" note
- [ ] Each also works non-interactively: `ssh admin@kyles-nas stacks`
- [ ] `command -v helpers` resolves inside `qnap/bin`, not somewhere unexpected

## 6. The index is really generated

```sh
printf '#!/bin/sh\n# a dummy helper\nexit 0\n' > /share/CE_CACHEDEV4_DATA/homelab/qnap/bin/zzdummy
chmod +x /share/CE_CACHEDEV4_DATA/homelab/qnap/bin/zzdummy
helpers          # zzdummy appears, with its description, no edit to motd.sh
chmod -x /share/CE_CACHEDEV4_DATA/homelab/qnap/bin/zzdummy
helpers          # zzdummy is gone
rm /share/CE_CACHEDEV4_DATA/homelab/qnap/bin/zzdummy
```

- [ ] Appears when executable, with its line-2 text
- [ ] Disappears when not executable
- [ ] Removed afterwards (`git status` in the checkout is clean)

## 7. PATH hygiene

- [ ] `echo $PATH` has `qnap/bin` **once**, not twice
- [ ] `. /share/CE_CACHEDEV4_DATA/homelab/qnap/profile.sh` by hand does not
      duplicate it either
- [ ] `docker ps` still works from the interactive session - if Entware is ever
      installed, this is the check that catches `/opt/bin` shadowing something,
      and the fix is that `profile.sh` appends `/opt/bin` rather than prepending

## 8. It survives a reboot

The one check that cannot be faked, and the one the whole design exists for.

- [ ] *Control Panel -> Hardware -> General -> "Run user defined processes during
      startup"* is enabled **before** rebooting
- [ ] Reboot the NAS
- [ ] Log back in: the banner is there
- [ ] `grep -c 'qnap/profile.sh' /etc/profile` prints `1` - if it prints more,
      something other than `autorun.sh` is also appending
- [ ] The data volume is encrypted and unlocks during boot, so if the banner is
      missing immediately after a reboot, log out and back in once before
      treating it as a failure

## 9. After a firmware update

- [ ] Re-check the startup-processes toggle - it has been known to reset
- [ ] Log in and confirm the banner
- [ ] If the toggle was off, re-enabling it is enough; `bootstrap.sh` does not
      need re-running unless `autorun.sh` itself changed
