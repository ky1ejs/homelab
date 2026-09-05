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
#
# The agent's move tool is a local MCP server this script does not launch:
# Claude Code spawns `vault-mcp -stdio` itself from <vault>/.mcp.json, and it
# lives and dies with the session. If moves stop working, look for that server
# in `/mcp` inside the session before looking here.

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

# The tool policy, hooks and the move tool's registration ship in this image and
# are installed on every start, so a `git pull` + deploy is the whole update path
# — there is no `cp` to remember, and no way for the files the agent obeys to lag
# the ones in the repo. See DECISIONS.md#shipping-the-tool-policy-with-the-image.
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

# A WARNING, not fatal, and the asymmetry is the point: a missing tool policy is
# an UNSAFE agent, a missing .mcp.json is only a less capable one. Without it the
# agent has no move_file, so it cannot file or rename anything — Claude Code has
# no move tool of its own and Bash is denied. Attachments are worse than notes
# here: a note it can at least copy to the new path, an image it cannot touch at
# all. That is worth a loud line, because the symptom otherwise is an agent that
# says it filed something and did not.
# See DECISIONS.md#giving-the-agent-a-move.
if [ ! -f "${VAULT_DIR}/.mcp.json" ]; then
    log "WARNING: ${VAULT_DIR}/.mcp.json is missing - the agent will have no move_file,"
    log "WARNING: no trash_file and no delete_empty_folder."
    log "WARNING: it can still read, write and edit notes, but not move or rename"
    log "WARNING: anything, and cannot touch images or PDFs at all."
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
