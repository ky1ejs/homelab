#!/usr/bin/env bash
#
# vault-research: delete scratch topic folders once they are old enough.
#
# This is the only thing in this repository that deletes a file's CONTENT, and
# the only thing at all that removes anything from the scratch volume.
# Everything else refuses to: `move_file` will not overwrite, and the snapshot
# repo keeps history precisely so nothing is lost. vault-mcp gained two removals
# on 2026-09-05 and neither is one of these — `trash_file` is a rename into
# <vault>/.trash that destroys nothing, `delete_empty_folder` removes a
# directory that by definition holds nothing, and vault-mcp serves NEITHER on
# the research surface, so this script's claim over SCRATCH_DIR is unchanged.
# See ../DECISIONS.md#deleting. So this script is written to be paranoid
# in a way the others do not need to be, and every guard below exists because of
# what a bad glob would cost.
#
# WHY IT IS SAFE TO HAVE AT ALL: it only ever runs against SCRATCH_DIR, which is
# its own volume and is NOT the vault. That was the deciding factor in putting
# the scratch volume outside the vault rather than at <vault>/Research/_scratch,
# where it would have synced to the phone for free. A sweeper with a bad path
# inside the vault deletes notes, and Obsidian Sync propagates that to every
# device before anyone notices. The free handoff was not worth it.
# See ../DECISIONS.md#a-third-surface-for-research.
#
# WHAT IT DELETES: immediate subdirectories of SCRATCH_DIR in which NOTHING
# ANYWHERE INSIDE has been modified for SCRATCH_RETENTION_DAYS. Not the folder's
# own mtime, which does not change when files inside it are edited -- see the
# note above the find loop. Not files at the root, not dotted entries, and never
# SCRATCH_DIR itself.
#
#   scratch-sweep.sh             # delete what is due
#   scratch-sweep.sh --dry-run   # list what would go, delete nothing

set -euo pipefail

SCRATCH_DIR="${SCRATCH_DIR:-/scratch}"
DAYS="${SCRATCH_RETENTION_DAYS:-7}"
DRY=0
[ "${1:-}" = "--dry-run" ] && DRY=1

log() { printf '[sweep] %s\n' "$*" >&2; }

# Same reasoning as backup.sh validating its retention counts. A non-numeric
# value makes find's -mtime argument invalid, and the difference between "swept
# nothing because nothing was due" and "swept nothing because the job errored"
# is invisible in a log unless it is checked here.
case "${DAYS}" in
    ''|*[!0-9]*)
        log "FAILED: SCRATCH_RETENTION_DAYS must be a non-negative integer, got '${DAYS}'"
        exit 1
        ;;
esac

# 0 would mean "delete every topic folder on every run", including the one the
# agent is working in right now. If you want the sweeper off, stop the service;
# do not express it as a retention of nothing.
if [ "${DAYS}" -eq 0 ]; then
    log "FAILED: SCRATCH_RETENTION_DAYS=0 would delete every topic folder immediately."
    log "FAILED: to disable sweeping, stop vault-research-sweep instead."
    exit 1
fi

if [ ! -d "${SCRATCH_DIR}" ]; then
    log "no scratch directory at ${SCRATCH_DIR} — nothing to sweep"
    exit 0
fi

# Refuse to run against anything that looks like the vault or a git repo, even
# if SCRATCH_DIR has been pointed there by a bad .env. This should be impossible
# given the compose file, which is exactly when a check like this earns its
# keep: the failure it guards against is a configuration mistake, and those do
# not announce themselves.
if [ -d "${SCRATCH_DIR}/.git" ] || [ -d "${SCRATCH_DIR}/.obsidian" ]; then
    log "FAILED: ${SCRATCH_DIR} looks like a vault or a git repo, not a scratch volume."
    log "FAILED: refusing to delete anything. Check SCRATCH_DIR in .env."
    exit 1
fi

# Resolve and re-check, so a symlinked SCRATCH_DIR cannot aim the sweep
# somewhere else. `-xdev` below then keeps find on this one filesystem, so a
# link inside a topic folder pointing at a mounted volume is not followed into.
real="$(cd "${SCRATCH_DIR}" 2>/dev/null && pwd -P)" || {
    log "FAILED: cannot resolve ${SCRATCH_DIR}"
    exit 1
}
case "${real}" in
    /|/vault|/vault/*|/snapshots|/snapshots/*|/backups|/backups/*|/home|/home/*|/usr|/usr/*|/etc|/etc/*)
        log "FAILED: ${SCRATCH_DIR} resolves to ${real}, which is not a scratch volume."
        exit 1
        ;;
esac

log "sweeping ${real} for topic folders older than ${DAYS} days"

swept=0
# -mindepth 1 -maxdepth 1: immediate children only, so a deep tree is removed as
# one unit rather than hollowed out from the inside.
# -type d: files sitting at the scratch root are left alone. They are almost
# certainly something a human put there.
# -name '.*' pruned: .claude and .mcp.json are this agent's installed policy.
# Sweeping those would leave a container that fails its own startup check.
#
# AGE IS THE NEWEST THING INSIDE THE FOLDER, not the folder's own mtime, and the
# difference is not academic. A directory's mtime changes only when an entry is
# added to or removed from THAT directory. Editing flies/patterns.md does not
# touch flies/; adding flies/images/a.jpg touches images/, not flies/. So the
# first version of this script, which selected with `-mtime +${DAYS}` on the
# directory itself, deleted a topic the agent had been appending to for a
# fortnight because the folder was created eight days ago. Verified against a
# tree with an old folder and a file written today: the old rule selected it.
#
# `-newermt` is GNU findutils. This script only ever runs inside the image,
# which is Debian-based; preflight.sh is the one that has to cope with QTS
# busybox, and it does not sweep.
while IFS= read -r -d '' dir; do
    # -print -quit stops at the first recent file, so this does not walk a large
    # topic folder to completion just to learn that it is active.
    if [ -n "$(find "${dir}" -xdev -newermt "-${DAYS} days" -print -quit 2>/dev/null)" ]; then
        continue
    fi

    if [ "${DRY}" -eq 1 ]; then
        log "would delete: ${dir}"
    else
        if rm -rf -- "${dir}"; then
            log "deleted: ${dir}"
        else
            # Never fatal. One undeletable folder must not stop the rest, and a
            # sweeper that exits non-zero puts the service into a restart loop.
            log "WARNING: could not delete ${dir}"
            continue
        fi
    fi
    swept=$((swept + 1))
done < <(find "${real}" -xdev -mindepth 1 -maxdepth 1 -type d ! -name '.*' -print0)

if [ "${DRY}" -eq 1 ]; then
    log "dry run: ${swept} folder(s) would be deleted"
else
    log "swept ${swept} folder(s)"
fi
exit 0
