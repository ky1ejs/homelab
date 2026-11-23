#!/usr/bin/env bash
#
# Exercises `homelab autorun` against a directory standing in for the QNAP
# flash config partition (HOMELAB_AUTORUN_DIR), since CI has no partition to
# mount. Covers the parts that can go wrong without QNAP hardware: keeping
# someone else's autorun.sh lines, idempotence, removal, and that the block
# itself, run under a POSIX sh, rewrites the profile.
#
# Run locally from the repo root:  .github/scripts/autorun-test.sh

set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

PART="${WORK}/config"
FAKE_HOME="${WORK}/home"
mkdir -p "${PART}" "${FAKE_HOME}"
touch "${PART}/uLinux.conf"

hl() { HOME="${FAKE_HOME}" SHELL=/bin/sh HOMELAB_AUTORUN_DIR="${PART}" "${REPO}/bin/homelab" "$@"; }

count_blocks() { grep -c '^# >>> homelab autorun' "${PART}/autorun.sh" || true; }

# 1. Someone else's autorun.sh, ending in exit: kept, and the block runs first.
printf '#!/bin/sh\necho theirs\nexit 0\n' > "${PART}/autorun.sh"
cp "${PART}/autorun.sh" "${WORK}/original"
hl autorun install 2>/dev/null
[ "$(count_blocks)" = 1 ] || fail "expected one block after install"
[ "$(head -n 1 "${PART}/autorun.sh")" = '#!/bin/sh' ] || fail "shebang is not first"
[ "$(sed -n 2p "${PART}/autorun.sh")" = "$(sed -n 1p <(hl autorun block))" ] || fail "block is not straight after the shebang"
grep -qx 'echo theirs' "${PART}/autorun.sh" || fail "lost the existing lines"
[ -x "${PART}/autorun.sh" ] || fail "autorun.sh is not executable"
grep -q 'homelab install: shell setup' "${FAKE_HOME}/.profile" || fail "install did not write the profile now"

# 2. A second install changes nothing.
cp "${PART}/autorun.sh" "${WORK}/after1"
hl autorun install 2>/dev/null
cmp -s "${PART}/autorun.sh" "${WORK}/after1" || fail "second install changed autorun.sh"

# 3. status reports the block for this checkout.
hl autorun status | grep -q "block     present, for ${REPO}" || fail "status did not report the block"

# 4. The block parses and runs under a POSIX sh, and restores a lost profile.
#    `sh -n` catches a quoting slip in the heredoc that bash -n of bin/homelab
#    cannot see.
sh -n "${PART}/autorun.sh" || fail "autorun.sh does not parse under sh"
rm -f "${FAKE_HOME}/.profile"
LOG="$(grep -o '>/tmp/homelab-autorun.log' "${PART}/autorun.sh" || true)"
[ -n "${LOG}" ] || fail "block does not log to /tmp/homelab-autorun.log"
# Run only the block, foreground, with the log redirected into the work dir.
sed -n '/^# >>> homelab autorun/,/^# <<< homelab autorun$/p' "${PART}/autorun.sh" \
    | sed -e "s|>/tmp/homelab-autorun.log 2>&1 &|>${WORK}/boot.log 2>\&1|" \
    > "${WORK}/block.sh"
sh "${WORK}/block.sh"
grep -q 'homelab install: shell setup' "${FAKE_HOME}/.profile" || { cat "${WORK}/boot.log"; fail "the block did not restore the profile"; }
grep -q 'homelab autorun: done' "${WORK}/boot.log" || fail "boot log has no done line"
if grep -q 'open a new SSH session' "${WORK}/boot.log"; then fail "boot log carries the interactive hints"; fi

# 5. remove gives back the original file byte for byte.
hl autorun remove 2>/dev/null
cmp -s "${PART}/autorun.sh" "${WORK}/original" || { diff "${WORK}/original" "${PART}/autorun.sh" || true; fail "remove did not restore the original"; }

# 6. With no autorun.sh, install creates one and remove deletes it again.
rm -f "${PART}/autorun.sh"
hl autorun install 2>/dev/null
[ "$(count_blocks)" = 1 ] || fail "expected one block in a new autorun.sh"
hl autorun remove 2>/dev/null
[ ! -e "${PART}/autorun.sh" ] || fail "remove left a file it created"

# 7. A linked autorun.sh (create-autorun's layout) is refused, untouched.
printf '#!/bin/sh\n' > "${WORK}/managed.sh"
ln -s "${WORK}/managed.sh" "${PART}/autorun.sh"
if hl autorun install 2>/dev/null; then fail "install wrote through a linked autorun.sh"; fi
[ "$(cat "${WORK}/managed.sh")" = '#!/bin/sh' ] || fail "linked target was modified"

echo "autorun: all checks passed"
