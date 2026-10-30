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
# THE CONTAINER IS THE RESTART LOOP. In server mode remote-control exits on the
# failures it cannot recover from by itself — the server unreachable for about
# ten minutes, persistent errors for about ten minutes, a login it cannot
# refresh — and `restart: unless-stopped` starts a fresh one. The phone finds
# the new session on its own (README.md, "Deploy a new version"). Read from the
# 2.1.312 binary on 2026-10-30, and stated in
# code.claude.com/docs/en/remote-control under "Extended network outage".
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
# The pane is a TUI, so colour codes are stripped by capture-pane (no -e) and
# what is left is the text a person attached to it would have read.
report_exit() {
    local status
    status="$(tmux display-message -p -t "${SESSION}" '#{pane_dead_status}' 2>/dev/null || true)"
    log "claude remote-control exited with status ${status:-unknown}. Its last screen:"
    tmux capture-pane -p -t "${SESSION}" -S "-${TAIL_LINES}" 2>/dev/null \
        | sed -e 's/[[:space:]]*$//' -e '/^$/d' \
        | while IFS= read -r line; do log "  | ${line}"; done
}

if tmux has-session -t "${SESSION}" 2>/dev/null; then
    log "tmux session ${SESSION} already exists, reusing"
else
    log "starting claude remote-control in tmux session ${SESSION}"
    # One tmux invocation, so remain-on-exit is set on the server BEFORE the
    # pane exists. Setting it after `new-session` leaves a window in which a
    # fast exit — an expired login fails in under a second — closes the pane
    # first, and that is exactly the case whose reason matters most.
    tmux start-server \; \
        set-option -g remain-on-exit on \; \
        new-session -d -s "${SESSION}" -c "${DIR}" "claude remote-control"
fi

log "attach with: docker exec -it \$(hostname) tmux attach -t ${SESSION}"

# Hold the container open for as long as the agent lives. A dead pane is an
# exit, even though the session is still there: remain-on-exit keeps it only so
# report_exit can read it.
while tmux has-session -t "${SESSION}" 2>/dev/null; do
    if [ "$(tmux display-message -p -t "${SESSION}" '#{pane_dead}' 2>/dev/null || true)" = 1 ]; then
        report_exit
        exit 1
    fi
    sleep "${POLL}"
done

log "tmux session ended"
exit 1
