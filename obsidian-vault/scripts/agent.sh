#!/usr/bin/env bash
#
# vault-claude: `claude remote-control` inside tmux.
#
# remote-control is a TTY application that prints a pairing QR and URL — not a
# daemon. tmux lets you detach, reattach over `docker exec`, and restart the
# agent without recreating the container. See INITIAL_PLAN.md §4.
#
# To pair the phone:
#   docker exec -it vault-claude tmux attach -t vault
#   (detach with Ctrl-b d — do NOT Ctrl-c, that kills the agent)

set -euo pipefail

VAULT_DIR="${VAULT_DIR:-/vault}"
SESSION="${TMUX_SESSION:-vault}"
POLL="${AGENT_POLL_INTERVAL:-10}"

log() { printf '[agent] %s\n' "$*" >&2; }

if [ ! -d "${VAULT_DIR}" ]; then
    log "vault directory ${VAULT_DIR} does not exist"
    exit 1
fi

cd "${VAULT_DIR}"

if [ ! -f "${VAULT_DIR}/.claude/settings.json" ]; then
    log "WARNING: ${VAULT_DIR}/.claude/settings.json is missing."
    log "WARNING: snapshot hooks will not fire. See INITIAL_PLAN.md Phase 2."
fi

# shellcheck disable=SC2329  # invoked via trap, which shellcheck cannot see
cleanup() {
    if tmux has-session -t "${SESSION}" 2>/dev/null; then
        log "killing tmux session ${SESSION}"
        tmux kill-session -t "${SESSION}" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM

if tmux has-session -t "${SESSION}" 2>/dev/null; then
    log "tmux session ${SESSION} already exists, reusing"
else
    log "starting claude remote-control in tmux session ${SESSION}"
    tmux new-session -d -s "${SESSION}" -c "${VAULT_DIR}" "claude remote-control"
fi

log "attach with: docker exec -it \$(hostname) tmux attach -t ${SESSION}"

# Hold the container open for as long as the agent session lives. Exiting when
# tmux dies means `restart: unless-stopped` brings back a wedged agent.
while tmux has-session -t "${SESSION}" 2>/dev/null; do
    sleep "${POLL}"
done

log "tmux session ended"
exit 1
