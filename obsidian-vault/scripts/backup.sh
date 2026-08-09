#!/usr/bin/env bash
#
# vault-backup: git bundle -> verify -> GFS rotate -> prune.
#
# A bundle is one file, written atomically, verified before it is published, and
# restored with a plain `git clone`. A file-sync of the live repo would
# eventually capture a torn state (packfiles, refs, index.lock) and produce a
# clone that fails fsck, silently, until you need it. See INITIAL_PLAN.md §2.4.
#
# One bundle is simultaneously a full vault copy AND its complete history. That
# is why there is a single `vault-latest` replaced in place rather than a
# grandfather-father-son pile: every point in time is already inside it.
#
# Restore:
#   age -d -i key.txt vault-latest.bundle.age > v.bundle   # if encrypted
#   git clone v.bundle restored-vault
#   git -C restored-vault checkout 'HEAD@{2026-08-01}'     # any point in time
#
# The stamped copies under daily/ and monthly/ are not for finding old notes —
# use the history above for that. They exist so a corrupted or maliciously
# rewritten repo cannot overwrite your only good copy before you notice.

set -euo pipefail

SNAPSHOT_DIR="${SNAPSHOT_DIR:-/snapshots}"
BACKUP_DIR="${BACKUP_DIR:-/backups}"
PREFIX="${BACKUP_PREFIX:-vault}"

KEEP_DAILY="${BACKUP_KEEP_DAILY:-7}"
KEEP_MONTHLY="${BACKUP_KEEP_MONTHLY:-3}"

# --force / BACKUP_FORCE=1 bundles even when HEAD has not moved.
FORCE="${BACKUP_FORCE:-0}"
[ "${1:-}" = "--force" ] && FORCE=1 || true

AGE_RECIPIENT="${AGE_RECIPIENT:-}"

GIT_DIR="${SNAPSHOT_DIR}/vault.git"
export GIT_DIR

log() { printf '[backup] %s\n' "$*" >&2; }

if [ ! -d "${GIT_DIR}" ]; then
    log "no repo at ${GIT_DIR} — nothing to back up yet"
    exit 1
fi

if ! git rev-parse --quiet --verify HEAD >/dev/null 2>&1; then
    # HEAD does not resolve. Two very different situations, and conflating them
    # is how a scheduled backup reports success forever while producing nothing:
    #
    #   no refs at all  -> genuinely fresh repo, nothing to do yet
    #   refs but no HEAD -> damaged repo, must be loud
    # Inspect the filesystem, not git: a damaged HEAD makes git treat the whole
    # directory as not-a-repository, so `git show-ref` fails identically to the
    # fresh-repo case. git cannot be used to diagnose git's own corruption.
    repo_has_refs=0
    if [ -s "${GIT_DIR}/packed-refs" ]; then
        repo_has_refs=1
    elif [ -n "$(find "${GIT_DIR}/refs" -type f -print -quit 2>/dev/null)" ]; then
        repo_has_refs=1
    fi

    if [ "${repo_has_refs}" = "1" ]; then
        log "FAILED: repo has refs but HEAD does not resolve — possible corruption."
        log "FAILED: not publishing. Previous backups are untouched. Investigate:"
        log "        git --git-dir=${GIT_DIR} fsck"
        exit 1
    fi
    log "repo has no commits yet — nothing to back up"
    exit 0
fi

mkdir -p "${BACKUP_DIR}/daily" "${BACKUP_DIR}/monthly"

# Skip when nothing has changed.
#
# A bundle is a full copy every time — `git bundle --all` is not incremental —
# so an unchanged repo produces an identical 100+ MB file. snapshot.sh only
# commits when the vault is dirty, so on an hourly schedule most runs have
# nothing new to say. Re-bundling those costs storage and, more painfully, a
# full re-upload to Drive for zero new information.
#
# State lives in SNAPSHOT_DIR, not BACKUP_DIR: the latter is what Hybrid Backup
# Sync mirrors off-site, and a churning marker file there is noise. Losing this
# file is harmless — the next run simply bundles.
head_sha="$(git rev-parse HEAD)"
state="${SNAPSHOT_DIR}/.last-bundled-sha"

if [ "${FORCE}" != "1" ] && [ -f "${state}" ] && [ "$(cat "${state}")" = "${head_sha}" ]; then
    # Touch it even though the contents are unchanged. This file's mtime is
    # "when the backup job last ran successfully", which is a different question
    # from "when the vault last changed" — and it is the one that detects a
    # stalled schedule. Without this, an idle week would look identical to
    # vault-cron being dead.
    touch "${state}"
    log "HEAD unchanged since last bundle (${head_sha:0:8}) — nothing to do"
    exit 0
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
name="${PREFIX}-${stamp}.bundle"
tmp="${BACKUP_DIR}/.${name}.tmp"

cleanup() { rm -f "${tmp}" "${tmp}.age"; }
trap cleanup EXIT

log "creating bundle ${name}"
git bundle create "${tmp}" --all

# Verify BEFORE publishing, and before encrypting — an encrypted bundle cannot
# be verified without the private key, which by design does not live here.
log "verifying bundle"
if ! git bundle verify "${tmp}" >/dev/null 2>&1; then
    log "FAILED: bundle did not verify. Keeping the previous good backup."
    exit 1
fi

published="${name}"
if [ -n "${AGE_RECIPIENT}" ]; then
    log "encrypting to ${AGE_RECIPIENT}"
    age -r "${AGE_RECIPIENT}" -o "${tmp}.age" "${tmp}"
    rm -f "${tmp}"
    mv "${tmp}.age" "${tmp}"
    published="${name}.age"
else
    log "WARNING: AGE_RECIPIENT is empty — bundles are plaintext."
    log "WARNING: these will be plaintext personal notes on Google Drive."
fi

# Publish to a FIXED name, replaced in place.
#
# A bundle contains the complete history, so ONE current bundle already provides
# every point in time — clone it and `git checkout 'HEAD@{2026-08-01}'`. A
# grandfather-father-son scheme is the right shape for backups that each hold a
# single point in time; here it is close to pure duplication, and at 100+ MB per
# copy that is gigabytes of Drive storage and upload buying nothing.
#
# `mv` within one filesystem is atomic, so a sync client never sees a partial
# file even if it runs mid-publish.
latest="${BACKUP_DIR}/${PREFIX}-latest.bundle"
if [ -n "${AGE_RECIPIENT}" ]; then
    latest="${latest}.age"
fi

mv "${tmp}" "${latest}"
trap - EXIT
log "published ${latest}"

# Drop the other-suffix file, so flipping AGE_RECIPIENT never strands a stale
# bundle. A plaintext copy left on Drive after enabling encryption would undo
# the entire point of encrypting, silently.
if [ -n "${AGE_RECIPIENT}" ]; then
    rm -f "${BACKUP_DIR}/${PREFIX}-latest.bundle"
else
    rm -f "${BACKUP_DIR}/${PREFIX}-latest.bundle.age"
fi

# Retention exists to survive backing up a DESTRUCTION — not to recover old note
# versions, which any single bundle can already do. If the repo is corrupted,
# ransomwared, or has its history rewritten, the next run faithfully bundles the
# damage straight over the top. These stamped copies are the window in which you
# can still notice.
#
# Keyed to "first bundle of the day/month" rather than to a clock hour, so it
# behaves correctly at any schedule and does not skip a day just because nothing
# had changed at the hour that used to own the slot.
today="${stamp%%T*}"     # YYYYMMDD
month="${today%??}"      # YYYYMM

keep_first_of() {
    local dir="$1" match="$2" label="$3"
    # find(1) + -print -quit rather than a glob: `ls` over an empty directory
    # returns 2 and takes the script down under pipefail. See INITIAL_PLAN.md §9.
    if [ -n "$(find "${dir}" -maxdepth 1 -type f -name "${PREFIX}-${match}*" -print -quit 2>/dev/null)" ]; then
        return 0
    fi
    cp -p "${latest}" "${dir}/${published}"
    log "kept first bundle of the ${label} (${match})"
}

keep_first_of "${BACKUP_DIR}/daily"   "${today}" "day"
keep_first_of "${BACKUP_DIR}/monthly" "${month}" "month"

# Names carry ISO-8601 stamps, so lexical order is chronological order.
#
# Deliberately find(1) + NUL, not `ls`: `ls` over an empty glob returns 2, and
# under `set -o pipefail` that takes the whole script down — rotation then
# appears to work while never actually running. See INITIAL_PLAN.md §9.
prune_dir() {
    local dir="$1"
    local keep="$2"
    local -a files=()
    local f total remove i

    while IFS= read -r -d '' f; do
        files+=("${f}")
    done < <(find "${dir}" -maxdepth 1 -type f -name "${PREFIX}-*" -print0 | sort -z)

    total="${#files[@]}"
    if [ "${total}" -le "${keep}" ]; then
        log "  $(basename "${dir}"): ${total} kept (limit ${keep})"
        return 0
    fi

    remove=$(( total - keep ))
    log "  $(basename "${dir}"): ${total} present, pruning ${remove}"
    for (( i = 0; i < remove; i++ )); do
        rm -f "${files[${i}]}"
        log "    removed $(basename "${files[${i}]}")"
    done
}

log "rotating"
prune_dir "${BACKUP_DIR}/daily"   "${KEEP_DAILY}"
prune_dir "${BACKUP_DIR}/monthly" "${KEEP_MONTHLY}"

# Only after everything above succeeded. Recording it earlier would mean a run
# that failed to publish still suppressed the next attempt.
printf '%s' "${head_sha}" > "${state}"

log "done"
