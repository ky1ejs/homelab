#!/usr/bin/env bash
#
# Materialise an agent's tool policy, hooks and local MCP servers into its
# working directory -- the vault for vault-claude, the scratch volume for
# vault-research.
#
#   install-settings.sh          # copy the image's copies into <vault>/
#
# Two files, installed the same way and for the same reason:
#
#   vault-claude-settings.json -> <vault>/.claude/settings.json
#   vault-claude-mcp.json      -> <vault>/.mcp.json
#
# ...plus whatever VAULT_DOCS names, which is how an agent's STANDING
# INSTRUCTIONS ship from this repo rather than being authored in the volume the
# agent can write. Added 2026-09-05, when vault-claude needed to be told about
# the jobs handoff and there was nowhere repo-managed to tell it: the research
# agent's CLAUDE.md had shipped from the image since it existed, and the vault
# agent's instructions lived only in <vault>/AGENTS.md, which is the operator's
# own note. That asymmetry is why the jobs directory landed with a manual
# "paste this into AGENTS.md" step, and this is the fix.
#
# The split it creates in the vault is worth stating: <vault>/CLAUDE.md is
# MANAGED and belongs to this repo, <vault>/AGENTS.md is YOURS and holds the
# vault's own conventions. Claude Code reads both. Nothing here ever writes
# AGENTS.md.
#
# Called by agent.sh before the agent starts, so `docker compose up -d` is the
# only step: the files in this repo are the source of truth, and the copies in
# the vault are build artifacts of them rather than something a human remembers
# to keep in sync. See DECISIONS.md#shipping-the-tool-policy-with-the-image.
#
# The first file is a SECURITY POLICY, not a preference — it is where Bash,
# WebFetch and WebSearch are denied. A deny rule added upstream did nothing
# until somebody re-ran a `cp` by hand, which is the failure this removes: the
# repo said the agent could not reach the network and the running agent could.
#
# The second is what gives the agent a move tool. Claude Code has none, and Bash
# is denied, so without it the agent cannot file a note at all — it can write a
# copy at the new path and nothing in the vault may delete the original. It
# registers vault-mcp's own binary as a local stdio MCP server, which is why the
# move enforces exactly the deny list the first file states rather than a second
# implementation of it. See DECISIONS.md#giving-the-agent-a-move.
#
# The MCP file is installed at the VAULT ROOT, not inside .claude/: Claude Code
# reads project MCP servers from <project>/.mcp.json and nowhere else. It is
# dotted, so Obsidian's indexer ignores it exactly as it ignores .claude — and
# the agent's own deny list refuses to read or write it, because an entry added
# there is a command every future session would execute.
#
# WHY A COPY AND NOT A SYMLINK OR A BIND MOUNT. Claude Code needs to read real
# files inside the vault, and the vault is also the work tree `vault-sync`
# commits. A symlink would be committed and restored as a dangling link — the
# repo checkout is not mounted into these containers at all. A bind mount would
# make the files the agent obeys differ from the files on disk, so the snapshot
# repo and an SMB browse would disagree with the running policy. A copy keeps
# one truth on disk, and keeps ARCHITECTURE.md#snapshots' promise that the tool
# policy restores along with the notes.
#
# Set VAULT_SETTINGS_MANAGED=0 to pin hand-edited files in the vault. Drift is
# then reported rather than corrected, because that is what you asked for. Only
# an exact "0" pins them — a typo leaves them managed, which overwrites what you
# were pinning, because the other direction leaves a security policy stale on
# the strength of a misspelling.
#
# VAULT_SETTINGS_SOURCE and VAULT_MCP_SOURCE select WHICH agent's files to
# install, and are deliberately absent from .env.example.
#
# Revised 2026-09-03. They began as internal seams for testing outside an image.
# research.sh then made them the mechanism that picks a second agent's entire
# security policy, so the old description ("not operator knobs") stopped being
# true while still reading as verified reasoning. Both callers now ASSIGN them
# rather than defaulting from the environment, which is what keeps a single line
# in the shared .env from pointing both agents at one policy file.
#
# So: this script installs whichever pair it is given, and the CALLER decides.
# agent.sh takes the defaults below (vault-claude's); research.sh pins the
# research pair. Neither reads them from .env.

set -uo pipefail

VAULT_DIR="${VAULT_DIR:-/vault}"
SOURCE="${VAULT_SETTINGS_SOURCE:-/usr/local/lib/vault/vault-claude-settings.json}"
MCP_SOURCE="${VAULT_MCP_SOURCE:-/usr/local/lib/vault/vault-claude-mcp.json}"
MANAGED="${VAULT_SETTINGS_MANAGED:-1}"
# Space-separated src:dst pairs, empty by default so a caller that names none
# installs none. ASSIGNED by the caller like the two SOURCE variables above and
# for the same reason -- both agents load one .env, so a value read from there
# would hand one agent the other's instructions. Neither src nor dst may contain
# a colon; every path this repo passes is a fixed one under /usr/local/lib/vault
# or the agent's working directory.
DOCS="${VAULT_DOCS:-}"

log() { printf '[settings] %s\n' "$*" >&2; }

# The temp file currently in flight, so a signal between mktemp and mv does not
# leave an orphan behind. Single quotes on the trap, so the path is expanded when
# the trap runs rather than spliced into a string bash re-parses as source code
# at exit — VAULT_DIR is operator configuration rather than agent-controlled, so
# this was never the hazard it is in hook-stamp.sh, but an apostrophe in a path
# would still break the cleanup.
tmp=""
# shellcheck disable=SC2329  # invoked via trap
cleanup() {
    if [ -n "${tmp}" ]; then rm -f -- "${tmp}"; fi
    return 0
}
trap cleanup EXIT

# install <source> <target> <what>
#
# Returns non-zero on a failure the caller should report. Every failure path
# logs first: this runs unattended on every agent start, so a silent one would
# surface as an agent quietly missing a policy or a tool.
install_file() {
    local source="$1" target="$2" what="$3" dir

    if [ ! -f "${source}" ]; then
        log "WARNING: ${source} is not in this image — cannot install the ${what}"
        return 1
    fi

    # Already current. Checked before writing so a restart of a healthy stack
    # touches nothing: these paths are inside the work tree `vault-sync`
    # commits, and an identical rewrite would still be a change to snapshot.
    if cmp -s "${source}" "${target}" 2>/dev/null; then
        log "up to date: ${target}"
        return 0
    fi

    dir="$(dirname "${target}")"
    if ! mkdir -p "${dir}"; then
        log "WARNING: cannot create ${dir}"
        return 1
    fi

    # Temp-then-rename like every other write into the vault. A torn file here
    # is a torn security policy, and `vault-sync` commits these paths.
    # *.tmp because that is what snapshot.sh excludes; both targets are
    # committed, so an orphan without the suffix would be committed with them.
    tmp="$(mktemp "${dir}/.install-XXXXXX.tmp" 2>/dev/null || true)"
    if [ -z "${tmp}" ]; then
        log "WARNING: cannot write into ${dir}"
        return 1
    fi

    if ! cp "${source}" "${tmp}"; then
        log "WARNING: cannot copy ${source}"
        cleanup; tmp=""
        return 1
    fi
    chmod 0644 "${tmp}" 2>/dev/null || true
    if ! mv -f "${tmp}" "${target}"; then
        log "WARNING: cannot install ${target}"
        cleanup; tmp=""
        return 1
    fi
    tmp=""   # renamed, not ours to clean up any more

    log "installed ${target} from ${source}"
    return 0
}

settings_target="${VAULT_DIR}/.claude/settings.json"
mcp_target="${VAULT_DIR}/.mcp.json"

if [ "${MANAGED}" = "0" ]; then
    if [ ! -f "${settings_target}" ]; then
        log "WARNING: ${settings_target} is missing and VAULT_SETTINGS_MANAGED=0."
        log "WARNING: the agent will start with NO tool policy — Bash and the web tools are then allowed."
    elif ! cmp -s "${SOURCE}" "${settings_target}" 2>/dev/null; then
        log "unmanaged: ${settings_target} differs from the image's copy, leaving it alone"
    fi
    if [ ! -f "${mcp_target}" ]; then
        log "WARNING: ${mcp_target} is missing and VAULT_SETTINGS_MANAGED=0 — the agent will have no move tool."
    elif ! cmp -s "${MCP_SOURCE}" "${mcp_target}" 2>/dev/null; then
        log "unmanaged: ${mcp_target} differs from the image's copy, leaving it alone"
    fi
    for pair in ${DOCS}; do
        doc_target="${VAULT_DIR}/${pair#*:}"
        if [ ! -f "${doc_target}" ]; then
            log "WARNING: ${doc_target} is missing and VAULT_SETTINGS_MANAGED=0 — the agent is missing standing instructions."
        elif ! cmp -s "${pair%%:*}" "${doc_target}" 2>/dev/null; then
            log "unmanaged: ${doc_target} differs from the image's copy, leaving it alone"
        fi
    done
    exit 0
fi

if [ ! -d "${VAULT_DIR}" ]; then
    log "WARNING: ${VAULT_DIR} does not exist — cannot install the tool policy"
    exit 1
fi

# The policy first, and its failure is this script's exit status: agent.sh turns
# a missing settings.json into a refusal to start, because an agent running
# without it has Bash and the web tools. A missing .mcp.json only costs the move
# tool, so it is reported and does not fail the install — an agent that cannot
# file notes is a degraded agent, not an unsafe one.
rc=0
install_file "${SOURCE}" "${settings_target}" "tool policy" || rc=1
install_file "${MCP_SOURCE}" "${mcp_target}" "move tool" \
    || log "WARNING: the agent will start without move_file — it will not be able to file or rename notes or attachments."

# The standing instructions last, and NEVER fatal: an agent without them is a
# less useful agent, not an unsafe one, which is the same distinction the two
# installs above draw between the policy and the move tool. `rc` is untouched
# here on purpose — agent.sh refuses to start on a non-zero exit, and no missing
# markdown file justifies that.
#
# Unquoted ${DOCS} is the word splitting this loop wants, and shellcheck's SC2086
# is disabled rather than worked around: the value is assigned by agent.sh and
# research.sh from literals in this repo, never from .env and never from
# anything an agent can write.
# shellcheck disable=SC2086
for pair in ${DOCS}; do
    install_file "${pair%%:*}" "${VAULT_DIR}/${pair#*:}" "standing instructions" \
        || log "WARNING: ${VAULT_DIR}/${pair#*:} is not current — the agent may start with stale instructions, or with none."
done

exit "${rc}"
