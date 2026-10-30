#!/usr/bin/env bash
#
# preflight.sh - assert the NAS is ready for gmail-mcp.
#
#   ./scripts/preflight.sh          # check only, change nothing
#   ./scripts/preflight.sh --fix    # also create, chown and chmod what is wrong
#
# Or from anywhere: `homelab preflight gmail-mcp [--fix]`. `homelab deploy
# gmail-mcp` runs the check first and stops if it fails.
#
# Run it on the NAS, as root, from gmail-mcp/. Run it before the first
# `-login`: the container runs as APP_UID:APP_GID, and a data directory that
# does not exist or belongs to root fails -login only at the very end, when it
# tries to save the token ("open /data/.google-token-....tmp: permission
# denied"), after Google has already issued the grant.
#
# Same shape as obsidian-vault/scripts/preflight.sh, and like it, it reads
# .env rather than hardcoding paths, so it and docker-compose.yml cannot
# disagree about where the data lives.
#
# Exit codes: 0 all good, 1 problems remain, 2 could not run the checks.

set -euo pipefail

ENV_FILE="${ENV_FILE:-./.env}"
FIX=0
[ "${1:-}" = "--fix" ] && FIX=1 || true   # `|| true` is load-bearing under set -e

fail=0
warn=0

ok()    { printf '  \033[32mok\033[0m    %s\n' "$*"; }
bad()   { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; fail=$((fail + 1)); }
note()  { printf '  \033[33mwarn\033[0m  %s\n' "$*"; warn=$((warn + 1)); }
did()   { printf '  \033[36mfixed\033[0m %s\n' "$*"; }
head_() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# One key out of .env, parsed rather than sourced, as obsidian-vault's does.
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

# The same defaults docker-compose.yml uses.
APP_UID="$(env_get APP_UID)"; APP_UID="${APP_UID:-1002}"
APP_GID="$(env_get APP_GID)"; APP_GID="${APP_GID:-100}"
DATA="$(env_get DATA_HOST_PATH)"
TS_STATE="$(env_get TS_STATE_HOST_PATH)"

if [ -z "${DATA}" ] || [ -z "${TS_STATE}" ]; then
    bad "DATA_HOST_PATH and TS_STATE_HOST_PATH must both be set in .env"
    printf '\nCannot check directories without them.\n'
    exit 2
fi

# ---------------------------------------------------------------------------

head_ "Directories (owner ${APP_UID}:${APP_GID})"

check_dir() {
    local path="$1" want_mode="$2" label="$3" mode uid gid

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

    if [ -n "${want_mode}" ]; then
        mode="$(stat -c '%a' "${path}")"
        if [ "${mode}" = "${want_mode}" ]; then
            ok "${label}: mode 0${want_mode}"
        elif [ "${FIX}" = "1" ]; then
            chmod "${want_mode}" "${path}"
            did "${label}: 0${mode} -> 0${want_mode}"
        else
            bad "${label}: mode 0${mode}, want 0${want_mode}  (--fix)"
        fi
    fi
}

# 0700 on data: it holds the sealed refresh token. The key is in .env, so the
# directory alone opens nothing, but nobody else on the NAS needs to list it.
check_dir "${DATA}"     "700" "data"
check_dir "${TS_STATE}" ""    "funnel state"

# The token is written by -login, so on a fresh NAS its absence is expected,
# not a failure. It is still the first thing to rule out when the server
# restarts in a loop, because the server refuses to start without it.
if [ -f "${DATA}/google-token.json" ]; then
    ok "sealed Google token present"
else
    note "no Google token yet - run -login next (README.md#signing-in)"
fi

# ---------------------------------------------------------------------------

printf '\n'
if [ "${fail}" -gt 0 ]; then
    printf '\033[31m%d problem(s)\033[0m, %d warning(s).\n' "${fail}" "${warn}"
    [ "${FIX}" = "1" ] || printf 'All of these are repairable: re-run with --fix\n'
    exit 1
fi
printf '\033[32mAll checks passed\033[0m (%d warning(s)).\n' "${warn}"
exit 0
