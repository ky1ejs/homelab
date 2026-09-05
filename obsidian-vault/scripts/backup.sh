#!/usr/bin/env bash
#
# vault-backup: git bundle -> verify -> GFS rotate -> prune.
#
# A bundle is one file, written atomically, verified before it is published, and
# restored with a plain `git clone`. A file-sync of the live repo would
# eventually capture a torn state (packfiles, refs, index.lock) and produce a
# clone that fails fsck, silently, until you need it. See ARCHITECTURE.md#backups.
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
# The stamped copies under hourly/, daily/ and monthly/ are not for finding old
# notes — use the history above for that. They exist so a corrupted or
# maliciously rewritten repo cannot overwrite your only good copy before you
# notice. The tiers are the granularity of "how fast would you notice".

set -euo pipefail

SNAPSHOT_DIR="${SNAPSHOT_DIR:-/snapshots}"
BACKUP_DIR="${BACKUP_DIR:-/backups}"
PREFIX="${BACKUP_PREFIX:-vault}"

KEEP_HOURLY="${BACKUP_KEEP_HOURLY:-18}"
KEEP_DAILY="${BACKUP_KEEP_DAILY:-7}"
KEEP_MONTHLY="${BACKUP_KEEP_MONTHLY:-3}"

# --force / BACKUP_FORCE=1 bundles even when HEAD has not moved.
FORCE="${BACKUP_FORCE:-0}"
[ "${1:-}" = "--force" ] && FORCE=1 || true

AGE_RECIPIENT="${AGE_RECIPIENT:-}"

GIT_DIR="${SNAPSHOT_DIR}/vault.git"
export GIT_DIR

# One log format for the whole image: `[backup] LEVEL message` on stderr, with
# the timestamp left to Docker's log driver. See log.sh.
# shellcheck source=scripts/log.sh
# shellcheck disable=SC1091  # sourced from the same directory at runtime
. "$(dirname "$0")/log.sh"
log_init backup

# Validate retention up front, loudly — same reasoning as cron.sh validating
# BACKUP_SCHEDULE. A non-numeric value makes the `-gt` test in keep_first_of fail
# its comparison rather than evaluate false, which would silently disable that
# tier and leave a backup job reporting success while retaining nothing.
for _keep_var in KEEP_HOURLY KEEP_DAILY KEEP_MONTHLY; do
    case "${!_keep_var}" in
        ''|*[!0-9]*)
            fatal "BACKUP_${_keep_var} must be a non-negative integer, got '${!_keep_var}'"
            exit 1
            ;;
    esac
done
unset _keep_var

if [ ! -d "${GIT_DIR}" ]; then
    fatal "no repo at ${GIT_DIR} — nothing to back up yet"
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
        fatal "repo has refs but HEAD does not resolve — possible corruption."
        fatal "not publishing. Previous backups are untouched. Investigate:"
        fatal "        git --git-dir=${GIT_DIR} fsck"
        exit 1
    fi
    log "repo has no commits yet — nothing to back up"
    exit 0
fi

mkdir -p "${BACKUP_DIR}/hourly" "${BACKUP_DIR}/daily" "${BACKUP_DIR}/monthly"

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

# Build in SNAPSHOT_DIR, not BACKUP_DIR.
#
# BACKUP_DIR is what Hybrid Backup Sync mirrors off-site, so a work-in-progress
# file there gets picked up and uploaded mid-write — observed in practice as a
# stray `.vault-….bundle.tmp.lock` on Drive. Wasted bandwidth for a file that is
# about to be renamed away, and clutter in the one directory that should contain
# nothing but finished bundles.
#
# Both live on the same volume (asserted by preflight.sh), so the publishing
# `mv` is still a same-filesystem rename and therefore still atomic.
#
# If they ever are NOT on one filesystem, that mv degrades to a copy — which
# would put a growing partial file in BACKUP_DIR, exactly the problem this
# avoids, only worse. So check, and fall back to building in place: an uploaded
# temp file is untidy, a torn published bundle is not.
tmp="${SNAPSHOT_DIR}/.${name}.tmp"
if [ "$(stat -c '%d' "${SNAPSHOT_DIR}")" != "$(stat -c '%d' "${BACKUP_DIR}")" ]; then
    warn "${SNAPSHOT_DIR} and ${BACKUP_DIR} are on different filesystems"
    warn "building in ${BACKUP_DIR} to keep the publish atomic"
    tmp="${BACKUP_DIR}/.${name}.tmp"
fi

cleanup() { rm -f "${tmp}" "${tmp}.age"; }
trap cleanup EXIT

log "creating bundle ${name}"
git bundle create "${tmp}" --all

# Verify BEFORE publishing, and before encrypting — an encrypted bundle cannot
# be verified without the private key, which by design does not live here.
log "verifying bundle"
if ! git bundle verify "${tmp}" >/dev/null 2>&1; then
    fatal "bundle did not verify. Keeping the previous good backup."
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
    warn "AGE_RECIPIENT is empty — bundles are plaintext."
    warn "these will be plaintext personal notes on Google Drive."
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
# Keyed to "first bundle of the hour/day/month" rather than to a clock hour that
# owns each slot, so it behaves correctly at any schedule and does not skip a day
# just because nothing had changed at the hour that used to own the slot.
#
# All three keys are derived from ${stamp}, NOT from a fresh `date` call. stamp
# is taken before bundling, and bundling a large vault is not instant — a second
# `date` can land in the next hour and file the copy under an hour the published
# name does not claim. One clock reading, three prefixes of it.
today="${stamp%%T*}"     # YYYYMMDD
month="${today%??}"      # YYYYMM
hour="${stamp%????Z}"    # YYYYMMDDTHH — drops MMSSZ from YYYYMMDDTHHMMSSZ

keep_first_of() {
    local dir="$1" match="$2" label="$3" keep="$4"
    # keep=0 disables the tier here rather than letting prune_dir clean up after
    # it. BACKUP_DIR is mirrored to Drive, so copy-then-prune would upload a file
    # and delete it minutes later, every single run.
    [ "${keep}" -gt 0 ] || return 0
    # find(1) + -print -quit rather than a glob: `ls` over an empty directory
    # returns 2 and takes the script down under pipefail. See DECISIONS.md#traps-found-while-building.
    if [ -n "$(find "${dir}" -maxdepth 1 -type f -name "${PREFIX}-${match}*" -print -quit 2>/dev/null)" ]; then
        return 0
    fi
    cp -p "${latest}" "${dir}/${published}"
    log "kept first bundle of the ${label} (${match})"
}

keep_first_of "${BACKUP_DIR}/hourly"  "${hour}"  "hour"  "${KEEP_HOURLY}"
keep_first_of "${BACKUP_DIR}/daily"   "${today}" "day"   "${KEEP_DAILY}"
keep_first_of "${BACKUP_DIR}/monthly" "${month}" "month" "${KEEP_MONTHLY}"

# Names carry ISO-8601 stamps, so lexical order is chronological order.
#
# Deliberately find(1) + NUL, not `ls`: `ls` over an empty glob returns 2, and
# under `set -o pipefail` that takes the whole script down — rotation then
# appears to work while never actually running. See DECISIONS.md#traps-found-while-building.
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
prune_dir "${BACKUP_DIR}/hourly"  "${KEEP_HOURLY}"
prune_dir "${BACKUP_DIR}/daily"   "${KEEP_DAILY}"
prune_dir "${BACKUP_DIR}/monthly" "${KEEP_MONTHLY}"

# Only after everything above succeeded. Recording it earlier would mean a run
# that failed to publish still suppressed the next attempt.
printf '%s' "${head_sha}" > "${state}"

log "done"
