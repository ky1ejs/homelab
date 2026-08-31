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

    # `if` rather than `[ ... ] && mounted=1`, for legibility rather than for
    # safety. An earlier draft of this comment claimed the `&&` form would exit
    # under `set -e`; that is wrong. POSIX and bash exempt every command in an
    # `&&`/`||` list except the one after the final operator, so a failing test
    # mid-script is not fatal — `scratch-sweep.sh`'s own `--dry-run` line is
    # exactly that construct and runs fine. The real hazard is narrower: such a
    # list as the LAST command of a script or function, whose status becomes the
    # exit status. Keeping the `if` is still worth it here, because two
    # independent conditions set one flag and a reader should not have to work
    # out which of them fired.
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
#
# Every path logs, for install-settings.sh's reason: this runs unattended on
# every start, so a silent failure surfaces as an agent quietly following the
# wrong instructions — or none. The first draft had two silent paths. A missing
# source skipped the block without a word, which is exactly the outcome the
# .dockerignore exception and the workflow path filter exist to prevent and the
# one place a runtime check should say so. And `cp ... && log ...` swallowed a
# failed copy: the `&&` short-circuits, nothing prints, and `set -e` does not
# fire because the failing command is not last in the list.
#
# Temp-then-rename like every other write into a mounted volume here. A torn
# CLAUDE.md is a torn set of standing instructions, and the agent reads it at
# the start of the very next session.
research_claude_src=/usr/local/lib/vault/research-CLAUDE.md
research_claude_dst="${SCRATCH_DIR}/CLAUDE.md"

if [ ! -f "${research_claude_src}" ]; then
    log "WARNING: research-CLAUDE.md is not in this image — the agent starts"
    log "WARNING: with no standing instructions. Check the .dockerignore"
    log "WARNING: exception and the build workflow's path filter."
elif ! cmp -s "${research_claude_src}" "${research_claude_dst}"; then
    claude_tmp="$(mktemp "${SCRATCH_DIR}/.CLAUDE-XXXXXX.tmp" 2>/dev/null || true)"
    if [ -z "${claude_tmp}" ]; then
        log "WARNING: cannot write into ${SCRATCH_DIR} — ${research_claude_dst} not installed."
    elif ! cp "${research_claude_src}" "${claude_tmp}"; then
        rm -f -- "${claude_tmp}"
        log "WARNING: could not copy research-CLAUDE.md — ${research_claude_dst} left as it was."
    elif ! mv -f "${claude_tmp}" "${research_claude_dst}"; then
        rm -f -- "${claude_tmp}"
        log "WARNING: could not install ${research_claude_dst}."
    else
        chmod 0644 "${research_claude_dst}" 2>/dev/null || true
        log "installed ${research_claude_dst}"
    fi
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
