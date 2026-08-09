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

# shellcheck disable=SC1091
. ./.env

IMAGE="${IMAGE:?IMAGE must be set in .env}"

log "pulling ${IMAGE}"
docker compose pull

# Provenance verification means the NAS refuses images that did not come from
# your workflow. Requires the gh CLI, which QNAP does not ship — skipped with a
# loud warning rather than failing, so a fresh NAS is not blocked on it.
if [ "${VERIFY}" = "1" ]; then
    if command -v gh >/dev/null 2>&1; then
        log "verifying build provenance"
        if ! gh attestation verify "oci://${IMAGE}" --owner "${GITHUB_OWNER:?set GITHUB_OWNER in .env}"; then
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
    log "NOTE: this restarts vault-claude. You will need to re-pair the phone."
    docker compose up -d
else
    log "recreating: ${SERVICES[*]}"
    docker compose up -d "${SERVICES[@]}"
fi

docker compose ps

log "done"
log "re-pair with: docker exec -it vault-claude tmux attach -t vault"
