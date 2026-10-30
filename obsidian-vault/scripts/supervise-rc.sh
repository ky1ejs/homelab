#!/usr/bin/env bash
#
# Run `claude remote-control` in tmux and hold the container open while it lives.
#
#   supervise-rc.sh <log-tag> <tmux-session> <directory>
#
# agent.sh and research.sh exec into this once their own guards have passed, so
# the two agents are supervised by one piece of code rather than two copies that
# drift. Everything that differs between them happens before the exec.
#
# THE CONTAINER IS THE RESTART LOOP. When remote-control exits,
# `restart: unless-stopped` starts a fresh one, and the phone finds the new
# session on its own (README.md, "Deploy a new version"). Where each exit comes
# from, and how sure we are of it:
#
#   - no usable login: exits at startup. Documented.
#   - server unreachable for about ten minutes: exits. Documented, under
#     "Extended network outage" in code.claude.com/docs/en/remote-control.
#     EXCEPT while it serves a project thread session, when the binary logs
#     "still retrying" and does not exit. Read from the binary only.
#   - persistent errors for about ten minutes: exits. Read from the binary only.
#
# So this script has two jobs: make sure an exit becomes a container restart,
# and make sure the REASON survives the restart. Before 2026-10-30 it did only
# the first. tmux closes a pane when its process exits, and the pane is the
# only place remote-control says why it stopped — so every disconnect left a
# fresh container, a working agent, and no record of what had gone wrong.
#
# remain-on-exit keeps the dead pane open. This script reads its last screen
# into the container log, then exits. `docker logs vault-claude` after a
# disconnect now shows what remote-control printed last.
#
# Everything targets the claude PANE by its id, never the session. A session
# target means the session's active pane, and a person attached to pair the
# phone may have opened a shell beside claude. Watching "the session" would
# then read the shell's state as claude's: miss a dead claude while the shell
# was the active pane, or report a dead shell as claude and kill a healthy agent.

set -euo pipefail

if [ "$#" -ne 3 ]; then
    printf 'usage: %s <log-tag> <tmux-session> <directory>\n' "${0##*/}" >&2
    exit 2
fi

TAG="$1"
SESSION="$2"
DIR="$3"
POLL="${AGENT_POLL_INTERVAL:-10}"
# Enough for the status block and the timestamped lines above it. The pane is
# the terminal's height, so asking for more mostly returns blank history.
TAIL_LINES=60

log() { printf '[%s] %s\n' "${TAG}" "$*" >&2; }

# shellcheck disable=SC2329,SC2317  # invoked via trap; code varies by shellcheck version
cleanup() {
    if tmux has-session -t "${SESSION}" 2>/dev/null; then
        log "killing tmux session ${SESSION}"
        tmux kill-session -t "${SESSION}" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM

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
log "starting claude remote-control in tmux session ${SESSION}"
# One tmux invocation, so remain-on-exit is set on the server BEFORE the pane
# exists. Setting it after `new-session` leaves a window in which a fast exit —
# a missing login fails in under a second — closes the pane first, and that is
# exactly the case whose reason matters most. -P -F prints the new pane's id.
#
# The global setting is then narrowed to claude's pane alone, still inside the
# same invocation, so a shell someone opens beside it closes normally on exit.
PANE="$(tmux start-server \; \
    set-option -g remain-on-exit on \; \
    new-session -d -P -F '#{pane_id}' -s "${SESSION}" -c "${DIR}" "claude remote-control" \; \
    set-option -p remain-on-exit on \; \
    set-option -g remain-on-exit off)"

log "attach with: docker exec -it \$(hostname) tmux attach -t ${SESSION}"

# Hold the container open for as long as claude lives. A dead pane is an exit,
# even though tmux still shows it: remain-on-exit keeps it only so report_exit
# can read it. A pane that has gone altogether — killed by hand — is an exit too.
while :; do
    dead="$(tmux display-message -p -t "${PANE}" '#{pane_dead}' 2>/dev/null || true)"
    case "${dead}" in
        1) report_exit; exit 1 ;;
        0) ;;
        *) log "the claude pane ${PANE} is gone"; exit 1 ;;
    esac
    sleep "${POLL}"
done
