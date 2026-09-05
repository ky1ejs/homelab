#!/bin/sh
# motd.sh - the SSH login banner: host state, repo state, and the helper index.
#
# THIS RUNS ON EVERY LOGIN, so the hard rule is that it never blocks and never
# fails. No network calls (no `git fetch`, no `docker` - a cold `docker ps` on
# this box can take seconds), no writes, and every command that could be absent
# is guarded. A banner that hangs turns `ssh nas` into a tool you stop reaching
# for, which is worse than having no banner.
#
# It also runs without host git. Container Station gives QTS docker but no git,
# and README.md#getting-the-repo-onto-the-nas-without-host-git records why
# installing one is a bad trade - so branch, SHA and "differs from origin" are
# read straight out of .git/ as plain files. Host git, when it happens to be
# there, is used for exactly one extra fact: whether the tree is dirty.
#
# Output is ASCII-only and under ~15 lines, matching bin/homelab's contract: a
# QTS session may have no UTF-8 locale, and mojibake in a banner reads as a
# broken system.
set -eu

# Resolve the checkout from this script's own location rather than a constant.
# There is one hardcoded path in this directory (profile.sh) and this is not it.
HOMELAB="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${HOMELAB}/qnap/bin"

# Colour only for a real terminal, honouring TERM=dumb and NO_COLOR, the same
# three conditions bin/homelab uses. Piping the banner anywhere gets plain text.
if [ -t 1 ] && [ "${TERM:-dumb}" != dumb ] && [ -z "${NO_COLOR:-}" ]; then
    B="$(printf '\033[1m')"; D="$(printf '\033[2m')"; R="$(printf '\033[0m')"
else
    B=""; D=""; R=""
fi

# Read one ref without git. Loose refs win over packed-refs, which is the same
# precedence git itself applies.
read_ref() {
    _r="$1"
    if [ -f "${HOMELAB}/.git/${_r}" ]; then
        head -n 1 "${HOMELAB}/.git/${_r}"
        return 0
    fi
    if [ -f "${HOMELAB}/.git/packed-refs" ]; then
        awk -v ref="${_r}" '$2 == ref { print $1; found = 1; exit } END { exit !found }' \
            "${HOMELAB}/.git/packed-refs"
        return $?
    fi
    return 1
}

# ---- line 1: the host -------------------------------------------------------
# df against the checkout, not against a named volume: QNAP volume names are not
# predictable (obsidian-vault/DECISIONS.md), so asking about a path we know
# exists is the only form that cannot go stale.
free="$(df -h "${HOMELAB}" 2>/dev/null | awk 'NR==2 { print $4 }')"
up="$(uptime 2>/dev/null | sed -n 's/.*up *\([^,]*\(, *[0-9]* min\)\?\).*/\1/p')"
printf '%s%s%s  up %s  %s free\n' \
    "${B}" "$(hostname 2>/dev/null || echo nas)" "${R}" "${up:-?}" "${free:-?}"

# ---- line 2: the repo -------------------------------------------------------
repo="no checkout at ${HOMELAB}"
if [ -d "${HOMELAB}/.git" ]; then
    head_line="$(head -n 1 "${HOMELAB}/.git/HEAD" 2>/dev/null || echo '')"
    case "${head_line}" in
        "ref: "*)
            ref="${head_line#ref: }"
            branch="${ref#refs/heads/}"
            sha="$(read_ref "${ref}" 2>/dev/null || echo '')"
            ;;
        *)
            branch="detached"
            sha="${head_line}"
            ;;
    esac
    # cut, not printf '%.7s': busybox printf's precision handling for strings
    # is not something to rely on across QTS firmware revisions.
    repo="${branch} $(printf '%s' "${sha:-???????}" | cut -c1-7)"

    # Behind/ahead without fetching: compare the local branch tip against the
    # last-known origin tip. This reports a DIVERGENCE, not a direction - telling
    # the two apart needs history walking, and being precise is not worth a
    # slower login. `homelab update` resolves it either way.
    if [ "${branch}" != detached ]; then
        origin="$(read_ref "refs/remotes/origin/${branch}" 2>/dev/null || echo '')"
        if [ -n "${origin}" ] && [ "${origin}" != "${sha}" ]; then
            repo="${repo}, differs from origin"
        fi
    fi

    # The one fact that needs real git. -uno skips untracked files: they are not
    # a reason to warn, and scanning for them is the slow half of git status.
    if command -v git >/dev/null 2>&1; then
        if [ -n "$(git -C "${HOMELAB}" status --porcelain -uno 2>/dev/null)" ]; then
            repo="${repo}, dirty"
        fi
    fi
fi
printf '%srepo%s %s\n' "${D}" "${R}" "${repo}"

# ---- the helper index -------------------------------------------------------
# Generated, never hand-maintained: a hand-written list goes stale silently, and
# a banner that advertises a helper which no longer exists is worse than one that
# lists nothing. The convention is line 2 of each file is its description.
# Non-executable files are skipped so a work-in-progress is not advertised.
printf '\n%shelpers%s\n' "${B}" "${R}"
for f in "${BIN}"/*; do
    [ -x "${f}" ] || continue
    desc="$(sed -n '2s/^# *//p' "${f}" 2>/dev/null || echo '')"
    printf '  %-12s %s\n' "$(basename "${f}")" "${desc}"
done

printf '\n%s%s/qnap/README.md  -  update with: homelab update%s\n' \
    "${D}" "${HOMELAB}" "${R}"
