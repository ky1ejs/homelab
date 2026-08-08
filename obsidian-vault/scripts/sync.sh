#!/usr/bin/env bash
#
# vault-sync: `ob sync --continuous` plus an hourly snapshot backstop.
#
# The hooks in vault-claude-settings.json bracket agent runs, but they never
# observe human edits made on the Mac or phone. This loop is the backstop for
# those, and only those. See INITIAL_PLAN.md §2.3.

set -euo pipefail

VAULT_DIR="${VAULT_DIR:-/vault}"
SNAPSHOT_INTERVAL="${SNAPSHOT_INTERVAL:-3600}"
SNAPSHOT_ENABLED="${SNAPSHOT_ENABLED:-1}"
POLL="${SYNC_POLL_INTERVAL:-30}"

HERE="$(cd "$(dirname "$0")" && pwd)"

log() { printf '[sync] %s\n' "$*" >&2; }

if [ ! -d "${VAULT_DIR}" ]; then
    log "vault directory ${VAULT_DIR} does not exist"
    exit 1
fi

cd "${VAULT_DIR}"

log "starting ob sync --continuous"
ob sync --continuous &
ob_pid=$!

# shellcheck disable=SC2329,SC2317  # invoked via trap; code varies by shellcheck version
cleanup() {
    if kill -0 "${ob_pid}" 2>/dev/null; then
        log "stopping ob (pid ${ob_pid})"
        kill "${ob_pid}" 2>/dev/null || true
        wait "${ob_pid}" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM

if [ "${SNAPSHOT_ENABLED}" != "1" ]; then
    log "snapshot backstop disabled; following ob only"
    wait "${ob_pid}"
    exit $?
fi

log "snapshot backstop every ${SNAPSHOT_INTERVAL}s"

elapsed=0
while kill -0 "${ob_pid}" 2>/dev/null; do
    # Sleep in short slices so a dying ob is noticed promptly rather than up to
    # SNAPSHOT_INTERVAL later.
    sleep "${POLL}"
    elapsed=$(( elapsed + POLL ))

    if [ "${elapsed}" -lt "${SNAPSHOT_INTERVAL}" ]; then
        continue
    fi
    elapsed=0

    stamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if ! "${HERE}/snapshot.sh" "snapshot: ${stamp}" human; then
        log "backstop snapshot failed; continuing"
    fi
done

log "ob exited; shutting down"
wait "${ob_pid}" 2>/dev/null || true
exit 1
