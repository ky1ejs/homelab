#!/usr/bin/env bash
#
# cron.sh ROLE: run one role's job on its schedule, with supercronic.
#
#   cron.sh agent       write.sh on BRIEF_SCHEDULE (default 05:30), and
#                       write.sh --final on BRIEF_RETRY (default 08:30)
#   cron.sh voice       voice.py every five minutes
#   cron.sh podcast     daily-brief podcast every five minutes
#   cron.sh publisher   daily-brief publish every five minutes
#
# A blank BRIEF_RETRY means no retry, and BRIEF_SCHEDULE's runs are final.
# Only a final run turns a failed day into a notice: see write.sh.
#
# The schedule lives here rather than in QNAP's crontab for the reasons in
# obsidian-vault/scripts/cron.sh: that one is untracked and firmware updates
# rewrite it. Times are in TZ, which docker-compose.yml sets from BRIEF_TZ.
#
# The voice job, the podcast job and the publisher poll: each tick is cheap
# when there is nothing to do, and polling means none needs to be told
# another finished, so no container can signal another. Each job holds its
# own lock, so a slow tick and the next never overlap.

set -euo pipefail

role="${1:-}"
CRONTAB="/tmp/brief.crontab"

log() { printf '[cron] %s\n' "$*" >&2; }

# check_schedule NAME VALUE: stop with the variable's name when a value from
# .env does not parse as a schedule, rather than with a crontab error.
check_schedule() {
    local test="${CRONTAB}.test"
    if [[ "$2" == *$'\n'* ]] || ! printf '%s true\n' "$2" > "${test}" \
        || ! supercronic -test "${test}" >/dev/null 2>&1; then
        log "FAILED: $1 is not a valid cron schedule: '$2'"
        log "FAILED: expected five fields, e.g. '30 5 * * *'"
        exit 1
    fi
    rm -f -- "${test}"
}

write=/usr/local/lib/daily-brief/write.sh
case "${role}" in
    agent)
        schedule="${BRIEF_SCHEDULE:-30 5 * * *}"
        retry="${BRIEF_RETRY-30 8 * * *}"
        check_schedule BRIEF_SCHEDULE "${schedule}"
        if [ -n "${retry}" ]; then
            check_schedule BRIEF_RETRY "${retry}"
            printf '%s %s\n%s %s --final\n' "${schedule}" "${write}" "${retry}" "${write}" > "${CRONTAB}"
        else
            printf '%s %s --final\n' "${schedule}" "${write}" > "${CRONTAB}"
        fi
        ;;
    voice | podcast | publisher)
        case "${role}" in
            # -I: isolated mode, see voice.py.
            voice) job="/opt/venv/bin/python -I /usr/local/lib/daily-brief/voice/voice.py" ;;
            podcast)
                if [ -z "${SPOTIFY_SHOW_ID:-}" ]; then
                    # No show, no podcast: the publisher sends the email
                    # alone. Stay up so compose does not restart this in a
                    # loop.
                    log "podcast: SPOTIFY_SHOW_ID is blank, so there is nothing to upload; idle"
                    exec sleep infinity
                fi
                job="/usr/local/bin/daily-brief podcast"
                ;;
            publisher) job="/usr/local/bin/daily-brief publish" ;;
        esac
        printf '*/5 * * * * %s\n' "${job}" > "${CRONTAB}"
        ;;
    *)
        log "usage: cron.sh agent|voice|podcast|publisher"
        exit 2
        ;;
esac

if ! supercronic -test "${CRONTAB}" >/dev/null 2>&1; then
    log "FAILED: the crontab does not parse:"
    cat -- "${CRONTAB}" >&2
    exit 1
fi

log "${role} (${TZ:-UTC}):"
sed 's/^/[cron]   /' "${CRONTAB}" >&2

# exec so supercronic is the process tini signals.
exec supercronic -passthrough-logs "${CRONTAB}"
