#!/usr/bin/env bash
#
# cron.sh ROLE: run one role's job on its schedule, with supercronic.
#
#   cron.sh agent       write.sh on BRIEF_SCHEDULE (default 05:30 and 08:30)
#   cron.sh voice       voice.py every five minutes
#   cron.sh publisher   daily-brief publish every five minutes
#
# The schedule lives here rather than in QNAP's crontab for the reasons in
# obsidian-vault/scripts/cron.sh: that one is untracked and firmware updates
# rewrite it. Times are in TZ, which docker-compose.yml sets from BRIEF_TZ.
#
# The voice job and the publisher poll: each tick is cheap when there is
# nothing to do, and polling means neither needs to be told the other
# finished, so no container can signal another. Each job holds its own lock,
# so a slow tick and the next never overlap.

set -euo pipefail

role="${1:-}"
CRONTAB="/tmp/brief.crontab"

log() { printf '[cron] %s\n' "$*" >&2; }

case "${role}" in
    agent)
        schedule="${BRIEF_SCHEDULE:-30 5,8 * * *}"
        job=/usr/local/lib/daily-brief/write.sh
        ;;
    voice)
        schedule="*/5 * * * *"
        # -I: isolated mode, see voice.py.
        job="/opt/venv/bin/python -I /usr/local/lib/daily-brief/voice/voice.py"
        ;;
    publisher)
        schedule="*/5 * * * *"
        job="/usr/local/bin/daily-brief publish"
        ;;
    *)
        log "usage: cron.sh agent|voice|publisher"
        exit 2
        ;;
esac

printf '%s %s\n' "${schedule}" "${job}" > "${CRONTAB}"

if ! supercronic -test "${CRONTAB}" >/dev/null 2>&1; then
    log "FAILED: not a valid cron schedule: '${schedule}'"
    log "FAILED: expected five fields, e.g. '30 5,8 * * *'"
    exit 1
fi

log "${role}: ${schedule} (${TZ:-UTC}): ${job}"

# exec so supercronic is the process tini signals.
exec supercronic -passthrough-logs "${CRONTAB}"
