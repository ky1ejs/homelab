#!/usr/bin/env bash
#
# Deliberate deploy. Run this ON THE NAS, over Tailscale SSH.
#
# CI builds and publishes; deployment is manual on purpose. vault-claude holds a
# live tmux session your phone is paired to, and an unattended `pull && up -d`
# would eventually tear that down mid-conversation — the failure looks like your
# phone silently losing the session. See DECISIONS.md#traps-found-while-building.
#
#   ./deploy.sh              # pull + recreate, verifying provenance if possible
#   ./deploy.sh --sync-only  # leave vault-claude alone, update sync only
#   ./deploy.sh --no-verify  # skip attestation (not recommended)

set -euo pipefail

cd "$(dirname "$0")/.."

VERIFY=1
SERVICES=()

while [ "$#" -gt 0 ]; do
    case "$1" in
        --no-verify)  VERIFY=0 ;;
        --sync-only)  SERVICES=(vault-sync) ;;
        -h|--help)    sed -n '2,12p' "$0"; exit 0 ;;
        *)            echo "unknown option: $1" >&2; exit 2 ;;
    esac
    shift
done

log() { printf '[deploy] %s\n' "$*" >&2; }

if [ ! -f .env ]; then
    log "no .env here — copy .env.example and fill it in first"
    exit 1
fi

# Parsed by key, NEVER sourced.
#
# `. ./.env` was what this did, and it aborted the deploy: .env holds
# `AGENT_GIT_NAME=Claude Code`, which a shell reads as the assignment
# `AGENT_GIT_NAME=Claude` followed by the command `Code`, and `set -e` takes the
# 127 as a failure. `BACKUP_SCHEDULE=0 * * * *` is the same trap with a glob.
# Quoting .env would fix the symptom and break the file for the thing that
# actually consumes it, since compose's env_file parser is not a shell.
#
# preflight.sh has parsed by key since it was written, for this exact reason.
# This script kept sourcing, and the two only ever diverged because nothing
# needed a value with a space in it until one did. bin/homelab uses the same
# approach; the three must stay in step.
env_get() {
    local key="$1" line value
    line="$(grep -E "^[[:space:]]*${key}=" .env 2>/dev/null | tail -n1)" || true
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

IMAGE="$(env_get IMAGE)"
GITHUB_OWNER="$(env_get GITHUB_OWNER)"
if [ -z "${IMAGE}" ]; then
    log "IMAGE must be set in .env"
    exit 1
fi

log "pulling ${IMAGE}"
docker compose pull

# Provenance verification means the NAS refuses images that did not come from
# your workflow. Requires the gh CLI, which QNAP does not ship — skipped with a
# loud warning rather than failing, so a fresh NAS is not blocked on it.
if [ "${VERIFY}" = "1" ]; then
    if command -v gh >/dev/null 2>&1; then
        log "verifying build provenance"
        if [ -z "${GITHUB_OWNER}" ]; then
            log "FAILED: GITHUB_OWNER must be set in .env to verify provenance."
            log "FAILED: set it, or re-run with --no-verify."
            exit 1
        fi
        if ! gh attestation verify "oci://${IMAGE}" --owner "${GITHUB_OWNER}"; then
            log "FAILED: provenance verification failed. Not deploying."
            exit 1
        fi
    else
        log "WARNING: gh CLI not found — skipping provenance verification."
        log "WARNING: install gh on the NAS, or re-run with --no-verify to silence this."
    fi
fi

if [ "${#SERVICES[@]}" -eq 0 ]; then
    log "recreating all services"
    # Not "you will need to re-pair the phone", which this said until now and
    # which was disproved on 2026-08-08: Remote Control pairing survives a
    # recreate and a NAS reboot. What a deploy can still do is interrupt an
    # agent run that is mid-conversation. See README.md#deploy-a-new-version.
    log "NOTE: this restarts BOTH agents, vault-claude and vault-research."
    log "NOTE: pairing survives, but a run in progress is interrupted."
    docker compose up -d
else
    log "recreating: ${SERVICES[*]}"
    docker compose up -d "${SERVICES[@]}"
fi

docker compose ps

log "done"
log "attach with: docker exec -it vault-claude   tmux attach -t vault"
log "            docker exec -it vault-research tmux attach -t research"
