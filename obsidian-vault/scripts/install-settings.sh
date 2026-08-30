#!/usr/bin/env bash
#
# Materialise the agent's tool policy and hooks into the vault.
#
#   install-settings.sh          # copy the image's copy to <vault>/.claude/settings.json
#
# Called by agent.sh before the agent starts, so `docker compose up -d` is the
# only step: the file in this repo is the source of truth, and the copy in the
# vault is a build artifact of it rather than something a human remembers to
# keep in sync. See DECISIONS.md#shipping-the-tool-policy-with-the-image.
#
# The file this installs is a SECURITY POLICY, not a preference — it is where
# Bash, WebFetch and WebSearch are denied. A deny rule added upstream did
# nothing until somebody re-ran a `cp` by hand, which is the failure this
# removes: the repo said the agent could not reach the network and the running
# agent could.
#
# WHY A COPY AND NOT A SYMLINK OR A BIND MOUNT. Claude Code needs to read a real
# file inside the vault, and the vault is also the work tree `vault-sync`
# commits. A symlink would be committed and restored as a dangling link — the
# repo checkout is not mounted into these containers at all. A bind mount would
# make the file the agent obeys differ from the file on disk, so the snapshot
# repo and an SMB browse would disagree with the running policy. A copy keeps
# one truth on disk, and keeps ARCHITECTURE.md#snapshots' promise that the tool
# policy restores along with the notes.
#
# Set VAULT_SETTINGS_MANAGED=0 to pin a hand-edited file in the vault. Drift is
# then reported rather than corrected, because that is what you asked for. Only
# an exact "0" pins it — a typo leaves the file managed, which overwrites what
# you were pinning, because the other direction leaves a security policy stale
# on the strength of a misspelling.
#
# VAULT_SETTINGS_SOURCE is an internal seam, not an operator knob: it points at
# a path inside the image and exists so this script can be exercised outside
# one. It is deliberately absent from .env.example.

set -uo pipefail

VAULT_DIR="${VAULT_DIR:-/vault}"
SOURCE="${VAULT_SETTINGS_SOURCE:-/usr/local/lib/vault/vault-claude-settings.json}"
MANAGED="${VAULT_SETTINGS_MANAGED:-1}"

log() { printf '[settings] %s\n' "$*" >&2; }

target="${VAULT_DIR}/.claude/settings.json"

if [ "${MANAGED}" = "0" ]; then
    if [ ! -f "${target}" ]; then
        log "WARNING: ${target} is missing and VAULT_SETTINGS_MANAGED=0."
        log "WARNING: the agent will start with NO tool policy — Bash and the web tools are then allowed."
    elif ! cmp -s "${SOURCE}" "${target}" 2>/dev/null; then
        log "unmanaged: ${target} differs from the image's copy, leaving it alone"
    fi
    exit 0
fi

if [ ! -f "${SOURCE}" ]; then
    log "WARNING: ${SOURCE} is not in this image — cannot install the tool policy"
    exit 1
fi

if [ ! -d "${VAULT_DIR}" ]; then
    log "WARNING: ${VAULT_DIR} does not exist — cannot install the tool policy"
    exit 1
fi

# Already current. Checked before writing so a restart of a healthy stack
# touches nothing: this path is inside the work tree `vault-sync` commits, and
# an identical rewrite would still be a change to snapshot.
if cmp -s "${SOURCE}" "${target}" 2>/dev/null; then
    log "up to date: ${target}"
    exit 0
fi

if ! mkdir -p "${VAULT_DIR}/.claude"; then
    log "WARNING: cannot create ${VAULT_DIR}/.claude"
    exit 1
fi

# Temp-then-rename like every other write into the vault. `.claude` is dotted so
# Obsidian's indexer ignores it, but `vault-sync` still commits this path and a
# torn file here is a torn security policy.
# *.tmp because that is what snapshot.sh excludes; .claude is committed, so an
# orphan without the suffix would be committed with it.
tmp=""
# shellcheck disable=SC2329  # invoked via trap
cleanup() {
    if [ -n "${tmp}" ]; then rm -f -- "${tmp}"; fi
    return 0
}
# Single quotes, so the path is expanded when the trap runs rather than spliced
# into a string bash re-parses as source code at exit. VAULT_DIR is operator
# configuration rather than agent-controlled, so this was not the hazard it is
# in hook-stamp.sh — but an apostrophe in a path would still break the cleanup.
trap cleanup EXIT

tmp="$(mktemp "${VAULT_DIR}/.claude/.settings-XXXXXX.tmp" 2>/dev/null || true)"
if [ -z "${tmp}" ]; then
    log "WARNING: cannot write into ${VAULT_DIR}/.claude"
    exit 1
fi

if ! cp "${SOURCE}" "${tmp}"; then
    log "WARNING: cannot copy ${SOURCE}"
    exit 1
fi
chmod 0644 "${tmp}" 2>/dev/null || true
if ! mv -f "${tmp}" "${target}"; then
    log "WARNING: cannot install ${target}"
    exit 1
fi
tmp=""   # renamed, not ours to clean up any more

log "installed ${target} from ${SOURCE}"
exit 0
