#!/usr/bin/env bash
#
# preflight.sh — assert the NAS is in the state the stack expects.
#
#   ./preflight.sh          # check only, change nothing
#   ./preflight.sh --fix    # additionally create/chown/chmod what is wrong
#
# Run it on the NAS, as root, from the directory holding docker-compose.yml and
# .env. Safe to run repeatedly — that is the point. Run it before the first
# `up -d`, and again after any QNAP change (new account, new share, volume
# migration), because most of what it checks is drift you cannot see by looking.
#
# It reads .env rather than hardcoding paths or ids, so this script and the
# compose file cannot disagree about what the setup is.
#
# Exit codes: 0 all good, 1 problems remain, 2 could not run the checks.
#
# QTS ships bash, but if yours does not, run it out of the image instead:
#   docker run --rm -u 0 -v /share/<vol>:/share/<vol> -v "$PWD:/cfg" -w /cfg \
#     --entrypoint /usr/local/lib/vault/preflight.sh \
#     ghcr.io/ky1ejs/homelab/obsidian-vault:latest

set -euo pipefail

ENV_FILE="${ENV_FILE:-./.env}"
FIX=0
[ "${1:-}" = "--fix" ] && FIX=1 || true   # NOTE: `|| true` is load-bearing under set -e

fail=0
warn=0

ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; fail=$((fail + 1)); }
note() { printf '  \033[33mwarn\033[0m  %s\n' "$*"; warn=$((warn + 1)); }
did()  { printf '  \033[36mfixed\033[0m %s\n' "$*"; }
head_() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# Parse one key out of .env. Deliberately NOT `. .env` — that file contains
# values with spaces (AGENT_GIT_NAME=Claude Code) which sourcing would execute
# as a command. docker compose parses env_file itself and does not use a shell;
# so do we.
env_get() {
    local key="$1" line value
    line="$(grep -E "^[[:space:]]*${key}=" "${ENV_FILE}" 2>/dev/null | tail -n1)" || true
    if [ -z "${line}" ]; then
        printf ''
        return 0
    fi
    value="${line#*=}"
    value="${value%%[[:space:]]#*}"          # strip ` # inline comment`
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    printf '%s' "${value}"
}

# ---------------------------------------------------------------------------

head_ "Config"

if [ ! -f "${ENV_FILE}" ]; then
    bad "${ENV_FILE} not found — copy .env.example to .env first"
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
    bad ".env is 0${env_mode}, want 0600  (--fix, or: chmod 600 ${ENV_FILE})"
fi

APP_UID="$(env_get APP_UID)"
APP_GID="$(env_get APP_GID)"
IMAGE="$(env_get IMAGE)"

if [ -z "${APP_UID}" ] || [ -z "${APP_GID}" ]; then
    bad "APP_UID / APP_GID not set in ${ENV_FILE}"
    exit 2
fi
ok "identity ${APP_UID}:${APP_GID}"

VAULT="$(env_get VAULT_HOST_PATH)"
SNAPSHOTS="$(env_get SNAPSHOT_HOST_PATH)"
BACKUPS="$(env_get BACKUP_HOST_PATH)"
SYNC_HOME="$(env_get SYNC_HOME_HOST_PATH)"
AGENT_HOME="$(env_get AGENT_HOME_HOST_PATH)"

for v in VAULT SNAPSHOTS BACKUPS SYNC_HOME AGENT_HOME; do
    if [ -z "${!v}" ]; then
        bad "${v}_HOST_PATH is empty in ${ENV_FILE}"
        exit 2
    fi
done

# ---------------------------------------------------------------------------

head_ "Identity"

# The uid must belong to a real account. QNAP allocates sequentially from 1000,
# so an *unallocated* uid is simply the next account someone creates — which
# would then own the 0700 credential directories. See DECISIONS.md#identity-1002-not-1000.
if owner_name="$(getent passwd "${APP_UID}" 2>/dev/null | cut -d: -f1)" && [ -n "${owner_name}" ]; then
    ok "uid ${APP_UID} belongs to a real account (${owner_name})"
else
    bad "uid ${APP_UID} is not allocated to any account — the next QNAP account created will inherit ownership of the credential directories"
fi

# The image bakes APP_UID as a build arg. Overriding `user:` in compose to a uid
# the image does not know makes Docker resolve $HOME to / instead of /home/app,
# and BOTH interactive logins then appear to succeed and fail to persist. This
# is the single most confusing failure in the whole setup, so check it directly.
if [ -n "${IMAGE}" ] && command -v docker >/dev/null 2>&1; then
    if baked="$(docker run --rm --entrypoint /usr/bin/id "${IMAGE}" -u app 2>/dev/null)"; then
        if [ "${baked}" = "${APP_UID}" ]; then
            ok "image's baked uid matches APP_UID (${baked})"
        else
            bad "image bakes uid ${baked} but .env says ${APP_UID} — \$HOME will resolve to / and logins will not persist. Rebuild the image, or run: docker compose pull"
        fi
    else
        note "could not inspect ${IMAGE} (not pulled yet?) — re-run after 'docker compose pull'"
    fi
else
    note "docker not available or IMAGE unset; skipped the baked-uid check"
fi

# ---------------------------------------------------------------------------

head_ "Directories"

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

check_dir "${VAULT}"      ""    "vault"
check_dir "${SNAPSHOTS}"  ""    "snapshots"
check_dir "${BACKUPS}"    ""    "backups"
check_dir "${SYNC_HOME}"  "700" "home-sync"
check_dir "${AGENT_HOME}" "700" "home-agent"

# ---------------------------------------------------------------------------

head_ "Volume"

# QNAP marks encrypted volumes with a CE_ prefix on the mount point. Volume
# encryption defends drives that leave the building; it does nothing against a
# running NAS. See ARCHITECTURE.md#trust-boundary.
vol="$(printf '%s' "${VAULT}" | cut -d/ -f1-3)"
case "${vol}" in
    /share/CE_*) ok "on an encrypted volume (${vol})" ;;
    *)           note "${vol} is not a CE_ (encrypted) volume — ARCHITECTURE.md#trust-boundary assumes it is" ;;
esac

# All five must share the volume; one on a plain volume silently drops out of
# the encrypted set, and nothing about the running system would look different.
for p in "${SNAPSHOTS}" "${BACKUPS}" "${SYNC_HOME}" "${AGENT_HOME}"; do
    case "${p}" in
        "${vol}"/*) : ;;
        *) bad "${p} is not on ${vol} — it is outside the encrypted volume" ;;
    esac
done

# ---------------------------------------------------------------------------

head_ "Exposure"

# The credential directories hold a live Anthropic OAuth token and the Obsidian
# Sync login. 0700 protects them from other accounts on the box; an SMB share
# pointing at one hands them out over the network regardless. They should be
# plain directories inside a volume, never QNAP shares.
smb_conf=/etc/config/smb.conf
if [ -r "${smb_conf}" ]; then
    exposed=0
    for p in "${SYNC_HOME}" "${AGENT_HOME}" "${SNAPSHOTS}"; do
        if grep -qF "path = ${p}" "${smb_conf}" 2>/dev/null; then
            bad "${p} is exported as an SMB share — remove it in the QNAP UI"
            exposed=1
        fi
    done
    [ "${exposed}" = "0" ] && ok "no SMB share points at credentials or snapshots" || true
else
    note "cannot read ${smb_conf}; verify by hand that no share points at home-sync, home-agent or snapshots"
fi

# ---------------------------------------------------------------------------

head_ "Agent policy"

# More than snapshot hooks: this file is also where Bash, WebFetch and WebSearch
# are denied. agent.sh only WARNS when it is missing and starts anyway, so a
# first session without it gets an agent holding exactly the tools that turn an
# injected note into an exfiltration. See ARCHITECTURE.md#trust-boundary.
settings="${VAULT}/.claude/settings.json"

# --fix can install it, but only from a copy it can actually see: beside this
# script (a repo clone) or in the working directory. The image does not carry
# vault-claude-settings.json, so a curl-only NAS setup must copy it by hand.
if [ ! -f "${settings}" ] && [ "${FIX}" = "1" ]; then
    src=""
    for cand in "./vault-claude-settings.json" \
                "$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)/vault-claude-settings.json"; do
        if [ -f "${cand}" ]; then
            src="${cand}"
            break
        fi
    done
    if [ -n "${src}" ]; then
        mkdir -p "${VAULT}/.claude"
        cp "${src}" "${settings}"
        chown -R "${APP_UID}:${APP_GID}" "${VAULT}/.claude"
        did "installed settings.json from ${src}"
    fi
fi

if [ -f "${settings}" ]; then
    ok "vault .claude/settings.json present"

    # Installed-but-stale is invisible otherwise, and this file is a security
    # policy: a deny rule added upstream does nothing until it is copied over.
    # --fix does not overwrite silently — you may have edited it deliberately.
    repo_copy=""
    for cand in "./vault-claude-settings.json" \
                "$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)/vault-claude-settings.json"; do
        if [ -f "${cand}" ]; then
            repo_copy="${cand}"
            break
        fi
    done
    if [ -n "${repo_copy}" ]; then
        if cmp -s "${repo_copy}" "${settings}"; then
            ok "settings.json matches ${repo_copy}"
        else
            note "settings.json differs from ${repo_copy} — intentional, or a stale copy? diff them, then: cp ${repo_copy} ${settings}"
        fi
    fi
    if command -v jq >/dev/null 2>&1; then
        if jq -e '.permissions.deny | index("Bash")' "${settings}" >/dev/null 2>&1; then
            ok "Bash is denied"
        else
            bad "${settings} does not deny Bash — see vault-claude-settings.json"
        fi
    fi
else
    bad "${settings} missing — copy vault-claude-settings.json there BEFORE 'docker compose up -d'"
fi

# ---------------------------------------------------------------------------

head_ "Backups"

# §11.6: a schedule that stops firing is invisible until you need a restore.
# This is the cheapest useful coverage — assert the newest bundle is recent.
# The live bundle is a single fixed name, replaced in place — its mtime is the
# freshness signal. The stamped copies under daily/ and monthly/ are corruption
# insurance, not the current backup, so they are deliberately not consulted
# here: an old daily/ copy must never make a stalled schedule look healthy.
#
# Pick by MTIME, not by preferring the encrypted name. If encryption regresses,
# backup.sh writes vault-latest.bundle and leaves the old vault-latest.bundle.age
# untouched beside it — so a name-ordered search reports the stale encrypted file
# and prints "bundles are encrypted" over a plaintext current backup. Found
# 2026-08-11 while recovering a lost .env; every check passed at the time.
enc="${BACKUPS}/vault-latest.bundle.age"
plain="${BACKUPS}/vault-latest.bundle"
newest=""
if [ -f "${enc}" ] && [ -f "${plain}" ]; then
    # Both present is itself wrong: one bundle, replaced in place, is the design.
    note "BOTH ${plain##*/} and ${enc##*/} exist — one is stale. Delete whichever is older once you know which is current."
    if [ "$(stat -c '%Y' "${plain}")" -gt "$(stat -c '%Y' "${enc}")" ]; then
        newest="${plain}"
    else
        newest="${enc}"
    fi
elif [ -f "${enc}" ]; then
    newest="${enc}"
elif [ -f "${plain}" ]; then
    newest="${plain}"
fi
if [ -z "${newest}" ]; then
    note "no bundle yet at ${BACKUPS}/vault-latest.bundle[.age] — run: docker compose --profile manual run --rm backup"
else
    # Two DIFFERENT questions, and conflating them produces false alarms.
    #
    # "Did the job run?" -> the state file, touched on every successful run
    #   including no-op ones. This is what catches a stalled schedule.
    # "When did the vault last change?" -> the bundle's mtime. An old bundle on
    #   an idle vault is correct, not a fault.
    state="${SNAPSHOTS}/.last-bundled-sha"
    if [ -f "${state}" ]; then
        ran_h=$(( ( $(date +%s) - $(stat -c '%Y' "${state}") ) / 3600 ))
        if [ "${ran_h}" -le 48 ]; then
            ok "backup job last ran ${ran_h}h ago"
        else
            bad "backup job has not run for ${ran_h}h — the schedule has stopped firing. Check: docker compose logs vault-cron"
        fi
    else
        note "no ${state} yet — cannot tell when the backup job last ran"
    fi

    age_h=$(( ( $(date +%s) - $(stat -c '%Y' "${newest}") ) / 3600 ))
    ok "bundle is ${age_h}h old ($(basename "${newest}")) — reflects the last vault change, not the last run"

    # An unencrypted bundle is plaintext personal notes on Google Drive. If
    # AGE_RECIPIENT is set, every published bundle must be .age.
    recipient="$(env_get AGE_RECIPIENT)"
    case "${newest}" in
        *.age)
            # An encrypted bundle proves the recipient was set WHEN IT WAS
            # WRITTEN, not that it still is. A .env rebuilt without it looks
            # perfect until the next recreate picks up the blank value and the
            # backup after that ships plaintext to Drive.
            if [ -n "${recipient}" ]; then
                ok "bundles are encrypted"
            else
                bad "newest bundle is encrypted but AGE_RECIPIENT is now EMPTY — the next backup after a recreate will write plaintext to Drive"
            fi
            ;;
        *)
            if [ -n "${recipient}" ]; then
                bad "AGE_RECIPIENT is set but $(basename "${newest}") is NOT encrypted — it predates the setting, or encryption is failing"
            else
                note "AGE_RECIPIENT is empty — bundles are plaintext, and Hybrid Backup Sync uploads them to Drive that way"
            fi
            ;;
    esac
fi

# ---------------------------------------------------------------------------

printf '\n'
if [ "${fail}" -gt 0 ]; then
    printf '\033[31m%d problem(s)\033[0m, %d warning(s).\n' "${fail}" "${warn}"
    [ "${FIX}" = "1" ] || printf 'Some of these are repairable: re-run with --fix\n'
    exit 1
fi
printf '\033[32mAll checks passed\033[0m (%d warning(s)).\n' "${warn}"
exit 0
