#!/bin/sh
# bootstrap.sh - install the qnap/ login hook on a fresh or factory-reset NAS.
#
# Run once, as admin, from the checkout. Safe to re-run: every step is a check
# before an action, so a second run is a no-op that re-verifies. That matters
# because the situation this exists for -- a NAS that has just lost its OS -- is
# one where you will run things twice while working out what already happened.
#
# What it actually does is small: put autorun.sh on the config partition (the
# only writable place QTS reads at boot) and add the same hook to the running
# /etc/profile so you do not have to reboot to see the result. Everything else
# is verification and instructions.
#
# It does NOT need git. Container Station gives QTS a docker binary but no git,
# and README.md#getting-the-repo-onto-the-nas-without-host-git records why
# installing one is a bad trade; the clone that had to happen before this script
# could run is a documented docker one-liner, and `bin/homelab update` does the
# pulling afterwards.
#
# It does NOT touch QTS settings. "Run user defined processes during startup" is
# a Control Panel toggle with no supported CLI equivalent, so it is printed as a
# reminder rather than poked at. Firmware updates have been known to reset it.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "${HERE}/.." && pwd)"
MNT=/tmp/config

say()  { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# ---- 1. privileges ----------------------------------------------------------
# Mounting the config partition and writing /etc/profile both need root. On QTS
# the admin account is uid 0, so this is one check, not two.
if [ "$(id -u)" -ne 0 ]; then
    die "must run as root (the QTS admin account). Try: sudo sh $0"
fi

# ---- 2. the checkout is where the boot hook will look for it ----------------
# autorun.sh and profile.sh each carry the path as a literal because neither can
# discover it: autorun.sh runs from the config partition, and profile.sh is
# sourced, so $0 is the login shell rather than the file. Two literals is two
# chances to disagree, so they are compared here rather than trusted.
# SC2016 is the point: the single quotes keep ${HOMELAB:-...} as literal text
# for sed to match against, rather than expanding it here.
# shellcheck disable=SC2016
expected="$(sed -n 's/^HOMELAB="\${HOMELAB:-\(.*\)}"$/\1/p' "${HERE}/profile.sh" | head -n 1)"
autorun_path="$(sed -n 's/^HOMELAB=\(.*\)$/\1/p' "${HERE}/autorun.sh" | head -n 1)"

[ -n "${expected}" ] || die "could not read the expected path out of ${HERE}/profile.sh"

if [ "${expected}" != "${autorun_path}" ]; then
    die "qnap/profile.sh says ${expected} but qnap/autorun.sh says ${autorun_path} - fix both"
fi

if [ "${REPO}" != "${expected}" ]; then
    say "This checkout is at:  ${REPO}"
    say "The hook expects:     ${expected}"
    say ""
    say "Either move the checkout, or change the HOMELAB line in BOTH"
    say "  qnap/profile.sh  and  qnap/autorun.sh  and commit the change."
    say "QNAP volume names are not predictable, so this is a real possibility"
    say "on a rebuilt NAS - find yours with: ls -d /share/*/"
    exit 1
fi

say "checkout: ${REPO}"

# ---- 3. the config partition ------------------------------------------------
# The partition holding autorun.sh is not the data volume and is not mounted by
# default. On x86 QNAP models it is partition 6 of the boot device, and the
# device name is asked of hal_app rather than assumed.
mounted=0
if mount | grep -q " ${MNT} "; then
    say "config partition already mounted at ${MNT}"
    mounted=1
else
    [ -x /sbin/hal_app ] || die "/sbin/hal_app not found - is this an x86 QNAP running QTS?"
    boot_pd="$(/sbin/hal_app --get_boot_pd port_id=0)" \
        || die "hal_app could not identify the boot device"
    [ -n "${boot_pd}" ] || die "hal_app returned an empty boot device"

    mkdir -p "${MNT}"
    mount "${boot_pd}6" "${MNT}" || die "could not mount ${boot_pd}6 at ${MNT}"
    say "mounted ${boot_pd}6 at ${MNT}"
fi

# ---- 4. install autorun.sh --------------------------------------------------
# Unmount on the way out however this ends. Leaving the config partition mounted
# is not catastrophic but it is untidy state on a box that is otherwise stateless
# up here, and it hides the fact that the copy is a deliberate, rare act.
cleanup() {
    if [ "${mounted}" -eq 0 ]; then
        umount "${MNT}" 2>/dev/null || warn "could not unmount ${MNT} - do it by hand"
    fi
}
trap cleanup EXIT INT TERM

cp "${HERE}/autorun.sh" "${MNT}/autorun.sh" || die "could not write ${MNT}/autorun.sh"
chmod +x "${MNT}/autorun.sh"
say "installed ${MNT}/autorun.sh"

# ---- 5. hook the RUNNING /etc/profile ---------------------------------------
# Same line autorun.sh appends, so a login now behaves exactly like a login after
# the next reboot. Unlike at boot, /etc/profile here may already contain the line
# (this script re-run, or autorun.sh having already fired), and within one boot
# it does accumulate - so this one is guarded and autorun.sh is not.
hook="[ -f ${expected}/qnap/profile.sh ] && . ${expected}/qnap/profile.sh"
if grep -Fqx "${hook}" /etc/profile 2>/dev/null; then
    say "/etc/profile already hooked"
else
    printf '%s\n' "${hook}" >> /etc/profile
    say "hooked /etc/profile for this boot"
fi

# ---- 6. what the script cannot do -------------------------------------------
say ""
say "Done. Two things left, both by hand:"
say ""
say "  1. Control Panel -> Hardware -> General:"
say "     enable \"Run user defined processes during startup\"."
say "     Without it autorun.sh never runs and the banner dies at the next"
say "     reboot. Firmware updates have been known to turn it back off, so"
say "     re-check it after one."
say ""
say "  2. Log out and back in. You should see the banner."
say "     If not: sh ${expected}/qnap/motd.sh"
say ""

if command -v docker >/dev/null 2>&1; then
    say "docker: found"
else
    say "docker: NOT on this PATH. Container Station may be stopped; the"
    say "        containers/stacks helpers look up its real path themselves."
fi
