#!/usr/bin/env bash
#
# preflight.sh - assert the NAS is ready for daily-brief.
#
#   ./scripts/preflight.sh          # check only, change nothing
#   ./scripts/preflight.sh --fix    # also create, chown and chmod what is wrong
#
# Or from anywhere: `homelab preflight daily-brief [--fix]`. `homelab deploy
# daily-brief` runs the check first and stops if it fails.
#
# Run it on the NAS, as root, from daily-brief/, before the first sign-in:
# the containers run as APP_UID:APP_GID, and a state directory that belongs to
# root fails gmail-login only at the very end, after Google issued the grant.
#
# Same shape as gmail-mcp/scripts/preflight.sh, and like it, it reads .env
# rather than hardcoding paths, so it and docker-compose.yml cannot disagree.
#
# Exit codes: 0 all good, 1 problems remain, 2 could not run the checks.

set -euo pipefail

ENV_FILE="${ENV_FILE:-./.env}"
INTERESTS_FILE="${INTERESTS_FILE:-./interests.md}"
FIX=0
[ "${1:-}" = "--fix" ] && FIX=1 || true   # `|| true` is load-bearing under set -e

fail=0
warn=0

ok()    { printf '  \033[32mok\033[0m    %s\n' "$*"; }
bad()   { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; fail=$((fail + 1)); }
note()  { printf '  \033[33mwarn\033[0m  %s\n' "$*"; warn=$((warn + 1)); }
did()   { printf '  \033[36mfixed\033[0m %s\n' "$*"; }
head_() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# One key out of .env, parsed rather than sourced, as gmail-mcp's does.
env_get() {
    local key="$1" line value
    line="$(grep -E "^[[:space:]]*${key}=" "${ENV_FILE}" 2>/dev/null | tail -n1)" || true
    if [ -z "${line}" ]; then
        printf ''
        return 0
    fi
    value="${line#*=}"
    value="${value%%[[:space:]]#*}"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    printf '%s' "${value}"
}

# ---------------------------------------------------------------------------

head_ "Config"

if [ ! -f "${ENV_FILE}" ]; then
    bad "${ENV_FILE} not found - copy .env.example to .env first"
    printf '\nCannot continue without .env.\n'
    exit 2
fi
ok ".env present"

env_mode="$(stat -c '%a' "${ENV_FILE}" 2>/dev/null || printf '???')"
if [ "${env_mode}" = "600" ]; then
    ok ".env is 0600"
elif [ "${FIX}" = "1" ]; then
    chmod 600 "${ENV_FILE}"
    did ".env 0${env_mode} -> 0600"
else
    bad ".env is 0${env_mode}, want 0600  (--fix)"
fi

# Each blank one stops a container or the first send. Named, not counted, so
# the line says what to fill in.
for key in IMAGE CLAUDE_CODE_OAUTH_TOKEN GMAIL_ACCOUNT BRIEF_TO GMAIL_CLIENT_ID GMAIL_CLIENT_SECRET BRIEF_TOKEN_KEY; do
    if [ -n "$(env_get "${key}")" ]; then
        ok "${key} set"
    else
        bad "${key} is blank in .env  (README.md#setting-up)"
    fi
done

# The same test parseKey makes, so a key the publisher will refuse is caught
# here rather than at 05:30.
key="$(env_get BRIEF_TOKEN_KEY)"
if [ -n "${key}" ]; then
    if [[ "${key}" =~ ^[0-9a-fA-F]{64}$ ]] \
        || [ "$(printf '%s' "${key}" | base64 -d 2>/dev/null | wc -c)" = "32" ]; then
        ok "BRIEF_TOKEN_KEY is 32 bytes"
    else
        bad "BRIEF_TOKEN_KEY is not 32 bytes as hex or base64  (openssl rand -base64 32)"
    fi
fi

# The same test parseShowID makes.
show="$(env_get SPOTIFY_SHOW_ID)"
if [ -z "${show}" ]; then
    note "SPOTIFY_SHOW_ID is blank - the brief goes out by email only, with no podcast"
elif [[ "${show}" =~ ^(spotify:show:)?[A-Za-z0-9]{10,64}$ ]]; then
    ok "SPOTIFY_SHOW_ID looks like a show id"
else
    bad "SPOTIFY_SHOW_ID is not a show id or spotify:show: URI  (save-to-spotify --json shows)"
fi

# ---------------------------------------------------------------------------

head_ "Interests"

# Gitignored, so `homelab update` never brings it. docker-compose.yml mounts
# it with create_host_path off, so a missing one stops `up` rather than
# Docker making a directory here as root.
if [ -L "${INTERESTS_FILE}" ] || [ ! -f "${INTERESTS_FILE}" ]; then
    bad "${INTERESTS_FILE} is missing or not a plain file - cp interests.example.md interests.md, then edit it"
elif [ ! -s "${INTERESTS_FILE}" ]; then
    bad "${INTERESTS_FILE} is empty"
elif [ "$(stat -c %s "${INTERESTS_FILE}")" -gt 16384 ]; then
    bad "${INTERESTS_FILE} is over 16 KiB; write.sh refuses it"
else
    ok "${INTERESTS_FILE} present"
fi

# ---------------------------------------------------------------------------

# The same defaults docker-compose.yml uses.
APP_UID="$(env_get APP_UID)"; APP_UID="${APP_UID:-1002}"
APP_GID="$(env_get APP_GID)"; APP_GID="${APP_GID:-100}"
ISSUES="$(env_get ISSUES_HOST_PATH)"
AUDIO="$(env_get AUDIO_HOST_PATH)"
PODCAST="$(env_get PODCAST_HOST_PATH)"
STATE="$(env_get STATE_HOST_PATH)"

if [ -z "${ISSUES}" ] || [ -z "${AUDIO}" ] || [ -z "${PODCAST}" ] || [ -z "${STATE}" ]; then
    bad "ISSUES_HOST_PATH, AUDIO_HOST_PATH, PODCAST_HOST_PATH and STATE_HOST_PATH must all be set in .env"
    printf '\nCannot check directories without them.\n'
    exit 2
fi

head_ "Directories (owner ${APP_UID}:${APP_GID})"

check_dir() {
    local path="$1" want_mode="$2" label="$3" mode uid gid

    # Docker follows a link in a bind mount's source, and podcast/days is
    # mounted into the publisher from inside a directory brief-podcast writes.
    if [ -L "${path}" ]; then
        bad "${label}: ${path} is a symlink; replace it with a directory"
        return 0
    fi
    if [ ! -d "${path}" ]; then
        if [ "${FIX}" = "1" ]; then
            mkdir -p "${path}"
            did "created ${path}"
        else
            bad "${label}: ${path} does not exist  (--fix creates it)"
            return 0
        fi
    fi

    uid="$(stat -c '%u' "${path}")"
    gid="$(stat -c '%g' "${path}")"
    if [ "${uid}:${gid}" = "${APP_UID}:${APP_GID}" ]; then
        ok "${label}: owned ${APP_UID}:${APP_GID}"
    elif [ "${FIX}" = "1" ]; then
        chown -R "${APP_UID}:${APP_GID}" "${path}"
        did "${label}: ${uid}:${gid} -> ${APP_UID}:${APP_GID}"
    else
        bad "${label}: owned ${uid}:${gid}, want ${APP_UID}:${APP_GID}  (--fix)"
    fi

    mode="$(stat -c '%a' "${path}")"
    if [ "${mode}" = "${want_mode}" ]; then
        ok "${label}: mode 0${want_mode}"
    elif [ "${FIX}" = "1" ]; then
        chmod "${want_mode}" "${path}"
        did "${label}: 0${mode} -> 0${want_mode}"
    else
        bad "${label}: mode 0${mode}, want 0${want_mode}  (--fix)"
    fi
}

# Four directories, not one, because each container may see only its own:
# the agent reads the web, so it must never see /state, where the Gmail grant
# lives, or /podcast, where the Spotify login does. 0700 on all of them: one
# account reads them all.
check_dir "${ISSUES}"       "700" "issues"
check_dir "${AUDIO}"        "700" "audio"
check_dir "${PODCAST}"      "700" "podcast"
check_dir "${PODCAST}/days" "700" "podcast/days"
check_dir "${PODCAST}/home" "700" "podcast/home"
check_dir "${STATE}"        "700" "state"

# Written by the sign-ins, so on a fresh NAS their absence is expected.
if [ -f "${STATE}/google-token.json" ]; then
    ok "sealed Google token present"
else
    note "no Google token yet - run gmail-login next (README.md#setting-up)"
fi
if [ -n "$(env_get SPOTIFY_SHOW_ID)" ]; then
    if [ -n "$(ls -A "${PODCAST}/home/.config/save-to-spotify" 2>/dev/null)" ]; then
        ok "Spotify sign-in present"
    else
        note "no Spotify sign-in yet - run save-to-spotify auth login (README.md#setting-up)"
    fi
fi

# ---------------------------------------------------------------------------

printf '\n'
if [ "${fail}" -gt 0 ]; then
    printf '\033[31m%d problem(s)\033[0m, %d warning(s).\n' "${fail}" "${warn}"
    [ "${FIX}" = "1" ] || printf 'Directory problems are repairable: re-run with --fix\n'
    exit 1
fi
printf '\033[32mAll checks passed\033[0m (%d warning(s)).\n' "${warn}"
exit 0
