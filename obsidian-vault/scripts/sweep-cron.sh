#!/usr/bin/env bash
#
# vault-research-sweep: run scratch-sweep.sh on a schedule.
#
# Same shape and the same reasoning as cron.sh — the schedule lives in git
# rather than QNAP's crontab, and supercronic runs the job directly over the one
# volume this container mounts rather than reaching for the Docker socket.
#
# It is a SEPARATE service from vault-cron rather than a second line in that
# one's crontab, because vault-cron mounts /snapshots and /backups. The only
# thing in this repository that deletes should be able to reach exactly one
# volume, and putting it in vault-cron would have given it three.

set -euo pipefail

# Standard five-field cron. Default 04:00 UTC daily — an hour after the backup,
# so the two are never competing for the NAS's disk.
SCHEDULE="${SCRATCH_SWEEP_SCHEDULE:-0 4 * * *}"
CRONTAB="/tmp/sweep.crontab"

log() { printf '[sweep-cron] %s\n' "$*" >&2; }

printf '%s /usr/local/lib/vault/scratch-sweep.sh\n' "${SCHEDULE}" > "${CRONTAB}"

if ! supercronic -test "${CRONTAB}" >/dev/null 2>&1; then
    log "FAILED: SCRATCH_SWEEP_SCHEDULE is not a valid cron expression: '${SCHEDULE}'"
    log "FAILED: expected five fields, e.g. '0 4 * * *'"
    exit 1
fi

# Fail at startup rather than at 04:00, on the value that decides how much gets
# deleted. --dry-run touches nothing, so this is safe to run on every boot, and
# it exercises the same validation the real run does.
if ! /usr/local/lib/vault/scratch-sweep.sh --dry-run >/dev/null; then
    log "FAILED: scratch-sweep.sh refused its configuration — see the lines above."
    exit 1
fi

log "schedule: ${SCHEDULE} (UTC)"
log "retention: ${SCRATCH_RETENTION_DAYS:-7} days"

exec supercronic -passthrough-logs "${CRONTAB}"
