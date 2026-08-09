#!/usr/bin/env bash
#
# vault-backup: git bundle -> verify -> GFS rotate -> prune.
#
# A bundle is one file, written atomically, verified before it is published, and
# restored with a plain `git clone`. A file-sync of the live repo would
# eventually capture a torn state (packfiles, refs, index.lock) and produce a
# clone that fails fsck, silently, until you need it. See INITIAL_PLAN.md §2.4.
#
# One bundle is simultaneously a full vault copy AND its complete history.
#
# Restore:
#   git clone /path/to/vault-<stamp>.bundle restored-vault
#   # encrypted: age -d -i key.txt vault-<stamp>.bundle.age > v.bundle first

set -euo pipefail

SNAPSHOT_DIR="${SNAPSHOT_DIR:-/snapshots}"
BACKUP_DIR="${BACKUP_DIR:-/backups}"
PREFIX="${BACKUP_PREFIX:-vault}"

KEEP_HOURLY="${BACKUP_KEEP_HOURLY:-24}"
KEEP_DAILY="${BACKUP_KEEP_DAILY:-7}"
KEEP_WEEKLY="${BACKUP_KEEP_WEEKLY:-4}"
KEEP_MONTHLY="${BACKUP_KEEP_MONTHLY:-12}"

DAILY_HOUR="${BACKUP_DAILY_HOUR:-03}"  # UTC hour that owns the daily slot
WEEKLY_DOW="${BACKUP_WEEKLY_DOW:-7}"   # 1=Mon .. 7=Sun
MONTHLY_DOM="${BACKUP_MONTHLY_DOM:-01}"

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

mkdir -p "${BACKUP_DIR}/hourly" "${BACKUP_DIR}/daily" \
         "${BACKUP_DIR}/weekly" "${BACKUP_DIR}/monthly"

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

# Tiering is keyed to the CLOCK, not to run count, so it behaves identically
# whether this fires hourly or daily.
#
# Exactly one run per day — the one at DAILY_HOUR — owns the daily slot and is
# the only one eligible for weekly/monthly promotion. Every other run lands in
# hourly/ instead. Without that gate, an hourly schedule would copy all 24 of
# Sunday's runs into weekly/, prune to 4, and leave four bundles from the same
# Sunday: the tier would still look healthy while meaning nothing.
#
# A run is published to exactly one base tier, never both, so a daily schedule
# aligned to DAILY_HOUR leaves hourly/ empty rather than duplicating everything.
hour="$(date -u +%H)"
dow="$(date -u +%u)"
dom="$(date -u +%d)"

if [ "${hour}" = "${DAILY_HOUR}" ]; then
    published_path="${BACKUP_DIR}/daily/${published}"
    mv "${tmp}" "${published_path}"
    trap - EXIT
    log "published ${published_path}"

    if [ "${dow}" = "${WEEKLY_DOW}" ]; then
        cp -p "${published_path}" "${BACKUP_DIR}/weekly/${published}"
        log "promoted to weekly"
    fi

    if [ "${dom}" = "${MONTHLY_DOM}" ]; then
        cp -p "${published_path}" "${BACKUP_DIR}/monthly/${published}"
        log "promoted to monthly"
    fi
else
    published_path="${BACKUP_DIR}/hourly/${published}"
    mv "${tmp}" "${published_path}"
    trap - EXIT
    log "published ${published_path}"
fi

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
prune_dir "${BACKUP_DIR}/hourly"  "${KEEP_HOURLY}"
prune_dir "${BACKUP_DIR}/daily"   "${KEEP_DAILY}"
prune_dir "${BACKUP_DIR}/weekly"  "${KEEP_WEEKLY}"
prune_dir "${BACKUP_DIR}/monthly" "${KEEP_MONTHLY}"

log "done"
