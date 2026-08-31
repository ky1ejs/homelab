#!/usr/bin/env bash
#
# vault-research: `claude remote-control` inside tmux, rooted in the scratch
# volume rather than the vault.
#
# This is the surface with web search and web fetch. It exists because the vault
# agent deliberately has neither, and research needs both. The two are separated
# by what they can READ, not by what they can reach:
#
#   vault-claude    the whole vault, no way out
#   vault-research  the open web, and nothing of yours
#
# See ../DECISIONS.md#a-third-surface-for-research and ARCHITECTURE.md#trust-boundary.
#
# To pair the phone:
#   docker exec -it vault-research tmux attach -t research
#   (detach with Ctrl-b d — do NOT Ctrl-c, that kills the agent)
#
# The vault is NOT mounted into this container. If you are reading this because
# you want the research agent to see a note, the answer is to copy that note
# into the scratch volume yourself, deliberately, per task. That copy is the
# decision. Adding the mount would make every future session's blast radius
# your entire vault, silently, with no line in any log.

set -euo pipefail

SCRATCH_DIR="${SCRATCH_DIR:-/scratch}"
SESSION="${TMUX_SESSION:-research}"
POLL="${AGENT_POLL_INTERVAL:-10}"

log() { printf '[research] %s\n' "$*" >&2; }

if [ ! -d "${SCRATCH_DIR}" ]; then
    log "scratch directory ${SCRATCH_DIR} does not exist"
    exit 1
fi

# A hard stop, not a warning. This container has the web tools; if the vault has
# been mounted into it, the split has collapsed into one agent holding private
# data, untrusted content and a way out at the same time — the exact combination
# the whole design exists to prevent. That is not a state to log and continue
# from.
#
# Testing `-d` alone would be WRONG and would never have started this container:
# the Dockerfile creates /vault, /snapshots, /backups and /scratch in the image
# so every role shares one layout, so those directories exist here whether or not
# anything is mounted at them. What distinguishes a real bind mount is its device
# number, which differs from the root filesystem's. A non-empty check backs that
# up for any filesystem where the device test does not hold.
root_dev="$(stat -c %d / 2>/dev/null || echo 0)"
for forbidden in /vault /snapshots; do
    [ -d "${forbidden}" ] || continue

    # `if`, not `[ ... ] && mounted=1`: a false test makes that the last command
    # status and `set -e` exits the script — which would turn this guard into a
    # container that dies silently whenever the vault is ABSENT, the opposite of
    # what it is for. Same trap as preflight.sh's `|| true` on its --fix line.
    mounted=0
    dev="$(stat -c %d "${forbidden}" 2>/dev/null || echo "${root_dev}")"
    if [ "${dev}" != "${root_dev}" ]; then
        mounted=1
    fi
    if [ -n "$(ls -A "${forbidden}" 2>/dev/null)" ]; then
        mounted=1
    fi

    if [ "${mounted}" -eq 1 ]; then
        log "FATAL: ${forbidden} is mounted into vault-research."
        log "FATAL: this container has WebSearch and WebFetch. With the vault"
        log "FATAL: also present it becomes a session that can read your notes"
        log "FATAL: and send them anywhere. Remove the mount from"
        log "FATAL: docker-compose.yml. See DECISIONS.md#a-third-surface-for-research."
        exit 1
    fi
done

cd "${SCRATCH_DIR}"

# Same mechanism as vault-claude: the policy and the MCP registration ship in
# the image and are installed on every start, so `git pull` + deploy is the
# whole update path. Different source files, and the environment names them.
export VAULT_DIR="${SCRATCH_DIR}"
export VAULT_SETTINGS_SOURCE="${VAULT_SETTINGS_SOURCE:-/usr/local/lib/vault/vault-research-settings.json}"
export VAULT_MCP_SOURCE="${VAULT_MCP_SOURCE:-/usr/local/lib/vault/vault-research-mcp.json}"

if ! /usr/local/lib/vault/install-settings.sh; then
    log "WARNING: could not install the tool policy from this image."
fi

# FATAL for the same reason it is fatal in agent.sh, with the polarity flipped.
# There, a missing policy means an agent that can reach the network. Here the
# agent is SUPPOSED to reach the network, and a missing policy means one that
# also has Bash and no deny on its own credentials or its own CLAUDE.md.
if [ ! -f "${SCRATCH_DIR}/.claude/settings.json" ]; then
    log "FATAL: ${SCRATCH_DIR}/.claude/settings.json is missing."
    log "FATAL: refusing to start a web-enabled agent with no tool policy."
    exit 1
fi

# A warning, not fatal: without it the agent can still search, fetch pages and
# write notes. What it loses is fetch_attachment, and with it the ability to
# save an image or a PDF at all — WebFetch returns text and Write cannot produce
# binary. The symptom otherwise is an agent that says it saved a picture and did
# not, which is worth a loud line.
if [ ! -f "${SCRATCH_DIR}/.mcp.json" ]; then
    log "WARNING: ${SCRATCH_DIR}/.mcp.json is missing — no fetch_attachment."
    log "WARNING: the agent can research and write notes but cannot save an"
    log "WARNING: image or a PDF. See DECISIONS.md#fetching-attachments."
fi

# The standing instructions for this folder. Installed rather than authored by
# the agent, because the agent's own policy write-denies it: a fetched page that
# could append here would be writing instructions for every later session.
if [ -f /usr/local/lib/vault/research-CLAUDE.md ] \
   && ! cmp -s /usr/local/lib/vault/research-CLAUDE.md "${SCRATCH_DIR}/CLAUDE.md"; then
    cp /usr/local/lib/vault/research-CLAUDE.md "${SCRATCH_DIR}/CLAUDE.md" \
        && log "installed ${SCRATCH_DIR}/CLAUDE.md"
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
    tmux new-session -d -s "${SESSION}" -c "${SCRATCH_DIR}" "claude remote-control"
fi

log "attach with: docker exec -it \$(hostname) tmux attach -t ${SESSION}"

while tmux has-session -t "${SESSION}" 2>/dev/null; do
    sleep "${POLL}"
done

log "tmux session ended"
exit 1
