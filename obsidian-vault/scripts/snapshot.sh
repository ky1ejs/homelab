#!/usr/bin/env bash
#
# Commit the current vault state into a git repo that lives OUTSIDE the vault.
#
#   snapshot.sh <message> <agent|human>
#
# The repo is at $SNAPSHOT_DIR/vault.git with the vault as its work tree, so no
# .git or .gitignore ever appears inside the vault. Obsidian's indexer and the
# sync client never see them, and nothing git-shaped propagates to the phone.
# Exclusions live in $GIT_DIR/info/exclude. See ARCHITECTURE.md#snapshots.
#
# Exits 0 when there was nothing to commit, or when another writer holds the
# lock — both are normal, not failures.

set -euo pipefail

VAULT_DIR="${VAULT_DIR:-/vault}"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-/snapshots}"
LOCK_TIMEOUT="${SNAPSHOT_LOCK_TIMEOUT:-120}"
# Falls back to 1 (markdown only) while .env.example ships 0. Deliberate, not a
# mismatch: the two defaults guard different things. .env.example is edited and
# reviewed by a human, so it states what this vault actually does. This fallback
# only applies when the variable is missing entirely — a dropped line, a partly
# restored .env — and there it should fail toward the REVERSIBLE mistake.
# Excluding wrongly is fixed by flipping the flag; including wrongly writes blobs
# git keeps permanently, and undoing it means rewriting history and re-issuing
# every bundle.
EXCLUDE_ATTACHMENTS="${EXCLUDE_ATTACHMENTS:-1}"

AGENT_NAME="${AGENT_GIT_NAME:-Claude Code}"
AGENT_EMAIL="${AGENT_GIT_EMAIL:-agent@vault.local}"
HUMAN_NAME="${HUMAN_GIT_NAME:-Vault}"
HUMAN_EMAIL="${HUMAN_GIT_EMAIL:-human@vault.local}"

GIT_DIR="${SNAPSHOT_DIR}/vault.git"
LOCK_FILE="${SNAPSHOT_DIR}/.snapshot.lock"

export GIT_DIR
export GIT_WORK_TREE="${VAULT_DIR}"

# One log format for the whole image: `[snapshot] LEVEL message` on stderr, with
# the timestamp left to Docker's log driver. See log.sh.
# shellcheck source=scripts/log.sh
# shellcheck disable=SC1091  # sourced from the same directory at runtime
. "$(dirname "$0")/log.sh"
log_init snapshot

if [ "$#" -lt 2 ]; then
    log "usage: snapshot.sh <message> <agent|human>"
    exit 2
fi

message="$1"
kind="$2"

case "${kind}" in
    agent) author_name="${AGENT_NAME}"; author_email="${AGENT_EMAIL}" ;;
    human) author_name="${HUMAN_NAME}"; author_email="${HUMAN_EMAIL}" ;;
    *)     log "unknown author kind: ${kind}"; exit 2 ;;
esac

if [ ! -d "${VAULT_DIR}" ]; then
    fatal "vault directory ${VAULT_DIR} does not exist"
    exit 1
fi

mkdir -p "${SNAPSHOT_DIR}"

# Serialise against the hourly backstop and the session hooks. A missed lock is
# not an error: whoever holds it is committing the same tree we would have.
exec 9>"${LOCK_FILE}"
if ! flock -w "${LOCK_TIMEOUT}" 9; then
    log "lock busy after ${LOCK_TIMEOUT}s, skipping this snapshot"
    exit 0
fi

if [ ! -d "${GIT_DIR}" ]; then
    log "initialising ${GIT_DIR}"
    # `git init --bare <path>` refuses to run while GIT_WORK_TREE is exported
    # ("GIT_WORK_TREE not allowed without specifying GIT_DIR"), so do the init
    # in a subshell with both unset.
    ( unset GIT_DIR GIT_WORK_TREE; git init --bare --quiet "${SNAPSHOT_DIR}/vault.git" )
    # --bare sets core.bare=true, which conflicts with having a work tree.
    git config core.bare false
    git config gc.auto 256
fi

# Rewritten every run so changing EXCLUDE_ATTACHMENTS takes effect on the next
# snapshot rather than needing the repo to be recreated.
{
    echo "# Managed by snapshot.sh — edits here will be overwritten."
    echo ".obsidian/workspace.json"
    echo ".obsidian/workspace-mobile.json"
    echo ".obsidian/cache"
    echo ".trash/"
    echo ".DS_Store"
    echo "*.tmp"
    if [ "${EXCLUDE_ATTACHMENTS}" = "1" ]; then
        # Git keeps every version of every blob permanently and the repo cannot
        # be shrunk later without rewriting history. See DECISIONS.md#image-and-packaging.
        echo "*.png"
        echo "*.jpg"
        echo "*.jpeg"
        echo "*.gif"
        echo "*.webp"
        echo "*.svg"
        echo "*.pdf"
        echo "*.mp3"
        echo "*.mp4"
        echo "*.mov"
        echo "*.wav"
        echo "*.zip"
    fi
} > "${GIT_DIR}/info/exclude"

git add -A

# `if` disables errexit for the condition, so a non-zero "there are changes"
# exit does not kill the script. Do NOT rewrite this as `git diff --quiet && ...`
# — that form aborts under `set -e`. See DECISIONS.md#traps-found-while-building.
if git diff --cached --quiet; then
    log "no changes, nothing to commit"
    exit 0
fi

git \
    -c "user.name=${author_name}" \
    -c "user.email=${author_email}" \
    commit --quiet --no-verify -m "${message}"

log "committed as ${author_name} <${author_email}>: ${message}"
