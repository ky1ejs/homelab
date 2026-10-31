#!/usr/bin/env bash
#
# Run `claude remote-control` in tmux, restart it when it exits, and report how
# it is doing to the container healthcheck.
#
#   supervise-rc.sh <log-tag> <tmux-session> <directory> <session-name>
#
# <session-name> is passed to `claude remote-control --name`, and is what the
# Claude app lists the session as. Without it Remote Control names the session
# after the directory, so the two agents appeared as "vault" and "scratch":
# true, but it names the mount, not what each one is for or what it can reach.
#
# agent.sh and research.sh exec into this once their own guards have passed, so
# the two agents are supervised by one piece of code rather than two copies that
# drift. Everything that differs between them happens before the exec.
#
# WHY remote-control EXITS, and how sure we are of each:
#
#   - no usable login: exits at startup. Documented.
#   - server unreachable for about ten minutes: exits. Documented, under
#     "Extended network outage" in code.claude.com/docs/en/remote-control.
#     EXCEPT while it serves a project thread session, when the binary logs
#     "still retrying" and does not exit. Read from the binary only.
#   - persistent errors for about ten minutes: exits. Read from the binary only.
#
# THIS SCRIPT IS THE RESTART LOOP, inside the container. It used to exit and
# leave restarting to `restart: unless-stopped`, which healed an outage but hid
# the one failure that needs a person: an expired login exits in under a
# second, so the container restarted forever and was never up long enough for
# any healthcheck to call it unhealthy. Restarting claude here keeps the
# container up, so that state is visible. DECISIONS.md#remote-control-disconnects.
#
# What a respawn does NOT redo: the guards in agent.sh and research.sh — the
# tool policy and move tool installed, research's refusal of a vault mount —
# and the policy install. Those run once per container start, and nothing a
# respawn touches changes them: a mount cannot change without a new container.
#
# Each exit's reason goes to the container log first. tmux would otherwise
# close the pane with the process, and the pane is the only place remote-control
# says why it stopped. remain-on-exit keeps the dead pane long enough to read.
#
# Everything targets the claude PANE by its id, never the session. A session
# target means the session's active pane, and a person attached to pair the
# phone may have opened a shell beside claude. Watching "the session" would
# then read the shell's state as claude's: miss a dead claude while the shell
# was the active pane, or report a dead shell as claude and kill a healthy agent.
#
# HEALTH. The verdict is written to HEALTH_FILE on every poll, and rc-health.sh
# (the compose healthcheck) only reads it. Unhealthy means claude has exited
# ALERT_EXITS or more times in the last WINDOW_SECS. Counting exits, not
# timing the current run, is deliberate: the slow failures — unreachable, or
# persistent errors — each take about ten minutes per run, so "has it stayed
# up five minutes" would call every one of those runs recovered halfway
# through, and a lasting outage would flap under the alert forever.
#
#   one exit (a blip that heals, a Ctrl-c)   -> stays healthy, no email
#   an expired login, exiting every restart  -> unhealthy within a minute
#   a lasting outage, exiting every ~10 min  -> unhealthy from the second exit
#
# monitoring/'s Container Health alert emails after ten minutes of Docker
# calling it unhealthy. A healthy verdict only says claude is RUNNING; a claude
# sitting at a startup question, or connected to nothing, reads healthy.
#
# The file is in the agent's home, not /tmp, and that matters. Both tool
# policies deny Edit (and so Write) under /home/app, and allow Write
# elsewhere. The healthcheck runs outside Claude Code's permissions, so a file
# the agent could write would be a file the agent could use to run code — the
# Bash deny, undone. rc-health.sh also refuses any line that is not exactly the
# shape below, for the same reason.

set -euo pipefail

if [ "$#" -ne 4 ]; then
    printf 'usage: %s <log-tag> <tmux-session> <directory> <session-name>\n' "${0##*/}" >&2
    exit 2
fi

TAG="$1"
SESSION="$2"
DIR="$3"
NAME="$4"
# tmux runs this string through a shell, and the name usually has spaces in it.
# %q quotes it once here, so the start and every respawn run the same command.
CLAUDE_CMD="claude remote-control --name $(printf '%q' "${NAME}")"
POLL="${AGENT_POLL_INTERVAL:-10}"
# Capped, because rc-health.sh calls a verdict older than 420s stale. A poll
# longer than that would make both agents permanently unhealthy.
if ! [[ "${POLL}" =~ ^[0-9]+$ ]] || [ "${POLL}" -lt 1 ] || [ "${POLL}" -gt 60 ]; then
    printf '[%s] AGENT_POLL_INTERVAL=%s is not 1-60; using 10\n' "$1" "${POLL}" >&2
    POLL=10
fi
# Read by rc-health.sh. Not a tunable: the two scripts must agree on it. See
# HEALTH above for why it is here and not in /tmp.
HEALTH_FILE=/home/app/.rc-health
# Enough for the status block and the timestamped lines above it. The pane is
# the terminal's height, so asking for more mostly returns blank history.
TAIL_LINES=60
# See HEALTH above. Two exits in half an hour: a single blip stays quiet, and
# two runs of the ten-minute give-up fit inside the window with room to spare.
ALERT_EXITS=2
WINDOW_SECS=1800
# A run shorter than this counts toward the backoff. It is not a health
# threshold — see HEALTH for why run length makes a poor one.
STABLE_SECS=300
# Restart delay: doubles from BACKOFF_MIN per consecutive short run, up to
# BACKOFF_MAX. A dead login retried every few seconds would fill the log and
# ask Anthropic's servers the same question thousands of times a day.
BACKOFF_MIN=10
BACKOFF_MAX=300

log() { printf '[%s] %s\n' "${TAG}" "$*" >&2; }

# shellcheck disable=SC2329,SC2317  # invoked via trap; code varies by shellcheck version
cleanup() {
    rm -f -- "${HEALTH_FILE}"
    if tmux has-session -t "${SESSION}" 2>/dev/null; then
        log "killing tmux session ${SESSION}"
        tmux kill-session -t "${SESSION}" 2>/dev/null || true
    fi
}
# On a signal, exit at once; the EXIT trap does the cleaning. The sleeps below
# run in the background and are waited for, because bash defers a trap until a
# foreground command returns, and a 300s backoff would outlast `docker stop`'s
# ten seconds and end in a SIGKILL with the tmux session never closed.
trap cleanup EXIT
trap 'exit 143' INT TERM

# "<epoch> healthy|unhealthy <reason>". The epoch lets rc-health.sh tell a
# verdict from a supervisor that has stopped writing them. Written to a temp
# file and renamed, so the healthcheck never reads half a line.
write_health() {
    if ! { printf '%s %s\n' "$(date +%s)" "$*" > "${HEALTH_FILE}.tmp" \
            && mv -f "${HEALTH_FILE}.tmp" "${HEALTH_FILE}"; }; then
        log "WARNING: could not write ${HEALTH_FILE}"
    fi
}

# Print the dead pane's last screen to the container log, blank lines dropped.
# capture-pane without -e drops colour codes, so what is left is the text a
# person attached to it would have read. -J joins lines tmux wrapped at the
# pane's width — a detached pane is 80 columns, which splits the session URL
# and most error messages in two.
report_exit() {
    local status
    # pane_dead_status is empty when a signal killed the process, and the
    # likeliest signal here is an OOM kill under mem_limit. Name the signal.
    status="$(tmux display-message -p -t "${PANE}" \
        '#{?pane_dead_signal,signal #{pane_dead_signal},#{pane_dead_status}}' 2>/dev/null || true)"
    log "claude remote-control exited (${status:-status unknown}). Its last screen:"
    tmux capture-pane -pJ -t "${PANE}" -S "-${TAIL_LINES}" 2>/dev/null \
        | sed -e 's/[[:space:]]*$//' -e '/^$/d' \
        | while IFS= read -r line; do log "  | ${line}"; done
}

# No "session already exists, reuse it" branch. The tmux server lives inside
# this container, so a fresh start never finds one — and a reused session would
# come without the pane id everything below depends on.
log "starting claude remote-control as \"${NAME}\" in tmux session ${SESSION}"
# One tmux invocation, so remain-on-exit is set on the server BEFORE the pane
# exists. Setting it after `new-session` leaves a window in which a fast exit —
# a missing login fails in under a second — closes the pane first, and that is
# exactly the case whose reason matters most. -P -F prints the new pane's id.
#
# The global setting is then narrowed to claude's pane alone, still inside the
# same invocation, so a shell someone opens beside it closes normally on exit.
# respawn-pane keeps both the pane id and this pane option.
PANE="$(tmux start-server \; \
    set-option -g remain-on-exit on \; \
    new-session -d -P -F '#{pane_id}' -s "${SESSION}" -c "${DIR}" "${CLAUDE_CMD}" \; \
    set-option -p remain-on-exit on \; \
    set-option -g remain-on-exit off)"

log "attach with: docker exec -it \$(hostname) tmux attach -t ${SESSION}"

started="$(date +%s)"   # when the current run of claude began
exits=0                 # exits since the container started
short_runs=0            # consecutive runs shorter than STABLE_SECS
recent=()               # epoch of each exit within WINDOW_SECS

# Drop exits older than the window, and write the verdict they add up to.
judge() {
    local t kept=()
    for t in "${recent[@]}"; do
        if [ $((now - t)) -lt "${WINDOW_SECS}" ]; then kept+=("${t}"); fi
    done
    recent=("${kept[@]}")
    if [ "${#recent[@]}" -ge "${ALERT_EXITS}" ]; then
        write_health unhealthy "$1; ${#recent[@]} exits in the last $((WINDOW_SECS / 60)) min"
    else
        write_health healthy "$1"
    fi
}

now="${started}"
judge "starting"

while :; do
    dead="$(tmux display-message -p -t "${PANE}" '#{pane_dead}' 2>/dev/null || true)"
    now="$(date +%s)"
    case "${dead}" in
        0)
            if [ $((now - started)) -ge "${STABLE_SECS}" ]; then short_runs=0; fi
            judge "up $((now - started))s"
            sleep "${POLL}" & wait $!
            ;;
        1)
            report_exit
            exits=$((exits + 1))
            recent+=("${now}")
            if [ $((now - started)) -lt "${STABLE_SECS}" ]; then
                short_runs=$((short_runs + 1))
            else
                short_runs=0
            fi
            # BACKOFF_MIN * 2^(short_runs-1), capped. The loop stops doubling
            # at the cap, so a long-dead login cannot overflow the arithmetic.
            delay="${BACKOFF_MIN}"
            for ((i = 1; i < short_runs && delay < BACKOFF_MAX; i++)); do
                delay=$((delay * 2))
            done
            [ "${delay}" -gt "${BACKOFF_MAX}" ] && delay="${BACKOFF_MAX}"
            log "restarting claude remote-control in ${delay}s (exit ${exits}; ${short_runs} short run(s) in a row)"
            judge "claude exited (${exits} since start); restarting in ${delay}s"
            sleep "${delay}" & wait $!
            # clear-history first, so the next exit report shows only the next
            # run's screen and not this one's again.
            tmux clear-history -t "${PANE}" 2>/dev/null || true
            if ! tmux respawn-pane -k -t "${PANE}" -c "${DIR}" "${CLAUDE_CMD}" 2>/dev/null; then
                log "could not respawn the claude pane ${PANE}; exiting for a container restart"
                exit 1
            fi
            started="$(date +%s)"
            ;;
        *)
            # Gone altogether: killed by hand, or the tmux server died. Leave
            # it to `restart: unless-stopped`, which rebuilds both from scratch.
            log "the claude pane ${PANE} is gone"
            exit 1
            ;;
    esac
done
