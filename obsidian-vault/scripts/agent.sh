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

# FATAL, not a warning. This used to warn and start anyway, which made sense
# while a human was expected to copy the file by hand and might be mid-setup.
# Now that installing it is automatic, reaching this line means the install
# genuinely failed — an unwritable .claude, wrong ownership on the mount, a file
# missing from the image — and starting regardless would run an agent over a
# corpus of clippings and forwarded mail with Bash, WebFetch and WebSearch all
# allowed. That is ARCHITECTURE.md#trust-boundary's exfiltration path, opened by
# a bug, with a log line as the only signal.
#
# `restart: unless-stopped` turns this into a container that visibly crash-loops
# instead. VAULT_SETTINGS_MANAGED=0 still works: that path skips INSTALLING, and
# the operator's pinned file is present, so this check passes.
if [ ! -f "${VAULT_DIR}/.claude/settings.json" ]; then
    log "FATAL: ${VAULT_DIR}/.claude/settings.json is missing."
    log "FATAL: refusing to start an agent with no tool policy - Bash and the web"
    log "FATAL: tools would be allowed. Fix the install, or see README.md setup step 6."
    exit 1
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
