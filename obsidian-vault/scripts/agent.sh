#!/usr/bin/env bash
#
# vault-claude: `claude remote-control` inside tmux.
#
# remote-control is a TTY application that prints a pairing QR and URL — not a
# daemon. tmux lets you detach, reattach over `docker exec`, and restart the
# agent without recreating the container. See DECISIONS.md#image-and-packaging.
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

# The tool policy and hooks ship in this image and are installed on every start,
# so a `git pull` + deploy is the whole update path — there is no `cp` to
# remember, and no way for the file the agent obeys to lag the one in the repo.
# See DECISIONS.md#shipping-the-tool-policy-with-the-image.
if ! /usr/local/lib/vault/install-settings.sh; then
    log "WARNING: could not install the tool policy from this image."
fi

# Still checked separately: install-settings.sh does nothing when
# VAULT_SETTINGS_MANAGED=0, and a missing file here is the case that matters —
# no policy means no snapshot hooks, no stamping, and an agent holding Bash and
# the web tools. See ARCHITECTURE.md#trust-boundary.
if [ ! -f "${VAULT_DIR}/.claude/settings.json" ]; then
    log "WARNING: ${VAULT_DIR}/.claude/settings.json is missing."
    log "WARNING: hooks will not fire and no tool policy is enforced. See README.md setup step 6."
fi

# shellcheck disable=SC2329,SC2317  # invoked via trap; code varies by shellcheck version
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
