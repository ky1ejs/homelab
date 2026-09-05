#!/bin/sh
# profile.sh - sourced by /etc/profile on every login. Sets PATH and prints the banner.
#
# SOURCED, NOT RUN. There is no `set -e` and no `exit` anywhere below: both would
# act on the user's own login shell, and the failure mode of getting that wrong
# is an account that cannot log in over SSH. Every step here is written to be
# skippable. For the same reason the file is idempotent: /etc/profile is rebuilt
# each boot so it should only be sourced once, but a manual `. profile.sh` while
# debugging must not double the PATH.
#
# Deliberately does NOT `set -u`-style guard with parameter defaults everywhere:
# busybox sh is the login shell here, and the subset used below (case, command -v,
# ${VAR:-}) is what it actually supports.

# Overridable so a checkout somewhere else still works; the default is this NAS's
# real volume. QNAP volume names are not predictable and CACHEDEV1_DATA is NOT
# correct here - see obsidian-vault/DECISIONS.md#traps-found-while-building.
HOMELAB="${HOMELAB:-/share/CE_CACHEDEV4_DATA/homelab}"
export HOMELAB

# Prepend qnap/bin, but only once. The `case` test brackets PATH with colons so
# a substring match cannot fire on a path that merely ends with the same name.
case ":${PATH}:" in
    *":${HOMELAB}/qnap/bin:"*) ;;
    *) PATH="${HOMELAB}/qnap/bin:${PATH}" ;;
esac

# bin/homelab is the real CLI for the stacks; the helpers here are the thin
# read-only view. Put it on PATH too rather than making people type the path.
case ":${PATH}:" in
    *":${HOMELAB}/bin:"*) ;;
    *) PATH="${PATH}:${HOMELAB}/bin" ;;
esac

# Entware, if someone has installed it, goes on the END of PATH.
#
# This is the one place this directory disagrees with a reflex. README.md records
# that installing Entware is a bad trade because it is a documented way to break
# the SSH PATH that docker is reached through - and it breaks it by shadowing the
# busybox and QTS binaries the rest of QTS expects. Appending keeps that from
# happening while still making an already-installed opkg tool reachable. Nothing
# in qnap/ requires Entware; if it is absent this is a no-op.
if [ -d /opt/bin ]; then
    case ":${PATH}:" in
        *":/opt/bin:"*) ;;
        *) PATH="${PATH}:/opt/bin" ;;
    esac
fi

export PATH

# A backstop for the case where the PATH edit above did not take (a checkout at
# an unexpected path, a shell that reset PATH after this ran). The alias resolves
# the script by absolute path, so it works when `helpers` on PATH would not.
# SC2139 (expands when defined, not when used) is deliberate: baking the path in
# now is what makes this a backstop for a broken PATH. A lazily-expanded alias
# would depend on HOMELAB still being set in whatever shell finally runs it.
# shellcheck disable=SC2139
alias helpers="${HOMELAB}/qnap/motd.sh"

# Banner for interactive logins ONLY. Without this test the banner is prepended
# to the output of `ssh nas <cmd>` and to scp/rsync/sftp transfers, which breaks
# them - an sftp session in particular dies on any unexpected bytes.
case "$-" in
    *i*)
        [ -x "${HOMELAB}/qnap/motd.sh" ] && "${HOMELAB}/qnap/motd.sh"
        ;;
esac
