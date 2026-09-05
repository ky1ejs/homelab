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
#
# /backups is on the list for the same reason as the other two, and is the one
# most easily forgotten: it holds vault-latest plus the hourly/daily/monthly
# bundles, which is the ENTIRE vault, and AGE_RECIPIENT ships empty so those
# bundles are plaintext by default. Mounting it here would put a complete
# readable copy of every note in the container with WebSearch and WebFetch.
# /scratch is deliberately absent: it is the one mount this service should have.
root_dev="$(stat -c %d / 2>/dev/null || echo 0)"
for forbidden in /vault /snapshots /backups; do
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
# whole update path. Different source files, and this script names them.
#
# ASSIGNED, NOT DEFAULTED. These were `${VAULT_SETTINGS_SOURCE:-...}`, which
# made them settable from .env -- and both agents load the same .env, so one
# line there would have pointed vault-claude and vault-research at a single
# policy file. That is the two-agent version of "making them look more alike",
# which vault-research-settings.json calls almost certainly wrong in one of
# them, reachable from a file the operator is told is safe to edit.
#
# install-settings.sh calls these internal seams rather than operator knobs.
# That was true when only vault-claude used them; this script made them the
# mechanism selecting a second agent's entire security policy, so they are
# pinned here and that file's comment now says so.
export VAULT_DIR="${SCRATCH_DIR}"
export VAULT_SETTINGS_SOURCE=/usr/local/lib/vault/vault-research-settings.json
export VAULT_MCP_SOURCE=/usr/local/lib/vault/vault-research-mcp.json

# The two markdown files this agent obeys, installed by the same script and for
# the same reason as the policy: they ship in the image, so `git pull` + deploy
# is the whole update path and there is no copy for a human to forget. Authoring
# them in the volume instead would put the agent's own instructions inside the
# directory it can write, and this session reads pages written by strangers.
#
# CLAUDE.md is this agent's standing instructions. JOBS.md is the contract for
# the jobs/ handoff, which BOTH agents read -- it sits at the scratch root
# rather than inside jobs/ because jobs/ is the one directory vault-claude can
# write to, and a contract kept there would be one agent writing rules for the
# other. DECISIONS.md#passing-a-brief-to-the-research-agent.
export VAULT_DOCS="/usr/local/lib/vault/research-CLAUDE.md:CLAUDE.md /usr/local/lib/vault/research-JOBS.md:JOBS.md"

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

# The jobs directory itself, created here because this is the service that owns
# the scratch volume read-write. vault-claude mounts THIS path from the host
# read-write, nested inside its otherwise read-only /scratch, and Docker cannot
# create a mount point inside a read-only bind mount — so on a host where it is
# missing, vault-claude fails to start rather than starting without the handoff.
# preflight.sh checks for it on the host, which is where the fix belongs; this
# line is what keeps an already-deployed stack from needing one.
#
# Not fatal if it fails. Without it this agent still researches and still writes
# topic folders; what is lost is the handoff, and the warning says so.
if [ ! -d "${SCRATCH_DIR}/jobs" ]; then
    if mkdir -p "${SCRATCH_DIR}/jobs"; then
        log "created ${SCRATCH_DIR}/jobs"
    else
        log "WARNING: could not create ${SCRATCH_DIR}/jobs — the jobs handoff is"
        log "WARNING: unavailable, and vault-claude may fail to start against a"
        log "WARNING: missing mount point. Create it on the host, owned by the"
        log "WARNING: same uid:gid as the rest of the scratch volume."
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
