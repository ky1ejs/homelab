#!/usr/bin/env bash
#
# vault-cron: run backup.sh on a schedule, from inside the stack.
#
# The alternative was QNAP's crontab, and it is worse in three specific ways:
# it is untracked, so the schedule is not reviewable or restorable from git;
# `crontab -e` alone does not survive a reboot because QTS regenerates from
# /etc/config/crontab; and firmware updates have been known to rewrite it. A
# backup schedule that can silently disappear is §11.6's failure mode aimed at
# the very thing meant to protect you from it.
#
# This container runs backup.sh DIRECTLY over the same volumes — it does not
# `docker run` anything. That is deliberate: scheduling a sibling container
# would mean mounting /var/run/docker.sock, and anything holding that socket
# has effective root on the NAS. A cron job does not need that, so it does not
# get it.
#
# As of 2026-08-29 one stack in this repo DOES hold that socket — dashboard/,
# whose deploy buttons cannot work without it. That does not soften this: the
# argument here was never "the socket is unmountable", it was "a cron job gains
# nothing from it", and that is still true. See
# ../DECISIONS.md#the-dashboard-and-the-docker-socket.
#
# The `backup` service (profile: manual) still exists for ad-hoc runs.

set -euo pipefail

# Standard five-field cron. Default 03:00 UTC daily.
SCHEDULE="${BACKUP_SCHEDULE:-0 3 * * *}"
CRONTAB="/tmp/vault.crontab"

log() { printf '[cron] %s\n' "$*" >&2; }

# /tmp rather than $HOME: this service mounts no credentials volume, so $HOME is
# the image's own directory and there is no reason to write into it.
printf '%s /usr/local/lib/vault/backup.sh\n' "${SCHEDULE}" > "${CRONTAB}"

if ! supercronic -test "${CRONTAB}" >/dev/null 2>&1; then
    log "FAILED: BACKUP_SCHEDULE is not a valid cron expression: '${SCHEDULE}'"
    log "FAILED: expected five fields, e.g. '0 3 * * *'"
    exit 1
fi

log "schedule: ${SCHEDULE} (UTC)"
log "running: /usr/local/lib/vault/backup.sh"

# -passthrough-logs sends the job's own stdout/stderr straight through instead
# of wrapping each line, so `docker compose logs vault-cron` reads exactly like
# a manual run.
#
# exec so supercronic becomes the process tini signals directly — without it a
# `docker compose stop` would be waiting on this shell.
exec supercronic -passthrough-logs "${CRONTAB}"
