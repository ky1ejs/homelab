#!/usr/bin/env bash
#
# Claude Code PostToolUse hook. Stamps the note the agent just wrote with the
# vault-wide agent frontmatter, so a note carries who last changed it even where
# git history cannot follow:
#
#   agent-created:  2026-08-20T09:11:03Z   set once, never rewritten
#   agent-modified: 2026-08-29T14:02:11Z   last agent write of any kind
#   agent:          claude-agent           who made that write
#
# Wired up in vault-claude-settings.json, which is copied to
# <vault>/.claude/settings.json. See README.md#the-agent-stamp.
#
# This is the SECOND implementation of a rule vault-mcp also enforces, in
# vault-mcp/stamp.go. The names and the semantics must not drift: the point of a
# generic `agent:` key is that every writer fills in the same three properties,
# and a surface that stamps differently is worse than one that does not stamp at
# all, because a query over the vault then silently misses its writes. The root
# README.md shared contract is the registry.
#
# Two things it deliberately mirrors from the agent's own tool policy rather
# than trusting it: the vault boundary and the deny list. Hooks run OUTSIDE the
# permission system — a hook that stamped CLAUDE.md would be writing to a file
# the agent itself is denied, which is the bypass the policy exists to prevent.
#
# Writes temp-then-rename, per the shared contract. This is the one place in
# this container where a second in-place writer would otherwise appear, and the
# note it is rewriting is the one `ob sync --continuous` is most likely to be
# reading right now.
#
# ALWAYS exits 0. A stamping failure must never fail an agent's tool call — the
# worst case is an unstamped note, and the snapshot commit still records the write.

set -uo pipefail

VAULT_DIR="${VAULT_DIR:-/vault}"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-/snapshots}"
AGENT_NAME="${VAULT_AGENT_NAME:-claude-agent}"
STAMP_ENABLED="${VAULT_STAMP:-1}"

# Only an explicit 0 turns this off. A blank or misspelled value leaves stamping
# ON, deliberately: this hook cannot refuse to start the way vault-mcp does, so
# the only place a typo can land is silently, and silently-off is the direction
# that cannot be reconstructed afterwards. Same judgement snapshot.sh makes
# about EXCLUDE_ATTACHMENTS — fail toward the recoverable mistake.
if [ "${STAMP_ENABLED}" = "0" ]; then
    exit 0
fi

# The same shape vault-mcp validates at startup. The name is written into YAML
# unquoted, so a value carrying a colon or a newline would rewrite the note's
# properties rather than one of them.
case "${AGENT_NAME}" in
    ""|*[!A-Za-z0-9._-]*) exit 0 ;;
esac

payload=""
# -t 5 so a hook invoked without stdin cannot hang the session, matching
# hook-snapshot.sh.
if ! IFS= read -r -t 5 -d '' payload; then
    : # empty or timed out; nothing to stamp
fi
if [ -z "${payload}" ]; then
    exit 0
fi

file_path="$(printf '%s' "${payload}" | jq -r '.tool_input.file_path // empty' 2>/dev/null || true)"
cwd="$(printf '%s' "${payload}" | jq -r '.cwd // empty' 2>/dev/null || true)"
if [ -z "${file_path}" ]; then
    exit 0
fi

# file_path may be relative to the session's cwd, which is the vault.
case "${file_path}" in
    /*) abs="${file_path}" ;;
    *)  abs="${cwd:-${VAULT_DIR}}/${file_path}" ;;
esac

if [ ! -f "${abs}" ]; then
    exit 0   # the write failed, or the tool touched something that is not a file
fi

# Resolve both sides before comparing: a symlinked note, or a vault root that is
# itself a link, would otherwise pass a prefix test while landing outside.
real="$(readlink -f "${abs}" 2>/dev/null || true)"
vault_real="$(readlink -f "${VAULT_DIR}" 2>/dev/null || true)"
if [ -z "${real}" ] || [ -z "${vault_real}" ]; then
    exit 0
fi
case "${real}" in
    "${vault_real}"/*) ;;
    *) exit 0 ;;
esac

rel="${real#"${vault_real}"/}"

# --- The deny list, mirrored from vault-claude-settings.json ----------------
#
# Markdown only; nothing under a dotted directory (.claude, .obsidian, .trash);
# never AGENTS.md or CLAUDE.md at any depth. Claude Code reads those two as
# standing instructions, so a writer that can reach them converts one bad note
# into an instruction every future session inherits.
case "$(printf '%s' "${rel}" | tr '[:upper:]' '[:lower:]')" in
    *.md) ;;
    *) exit 0 ;;
esac
case "/${rel}" in
    */.*) exit 0 ;;
esac
case "${rel##*/}" in
    AGENTS.md|CLAUDE.md) exit 0 ;;
esac

# --- Did this session create the note? --------------------------------------
#
# The SessionStart hook has already committed the pre-agent state, so a path the
# snapshot repo does not track did not exist when the session began.
#
# Absent repo means we cannot tell, and the failure directions are not
# symmetric: a missed agent-created is invisible and harmless, while a false one
# claims no human ever wrote a note they may well have written. So an
# unanswerable question is answered "no".
created=0
if [ -d "${SNAPSHOT_DIR}/vault.git" ]; then
    if ! git --git-dir="${SNAPSHOT_DIR}/vault.git" --work-tree="${vault_real}" \
         ls-files --error-unmatch -- "${rel}" >/dev/null 2>&1; then
        created=1
    fi
fi

now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

tmp="$(mktemp "$(dirname "${real}")/.vault-stamp-XXXXXX" 2>/dev/null || true)"
if [ -z "${tmp}" ]; then
    exit 0
fi
# shellcheck disable=SC2064  # expand tmp now: it is what this trap must remove
trap "rm -f '${tmp}'" EXIT

# The stamping itself. awk rather than a node one-liner for the same reason
# hook-snapshot.sh uses jq: one less runtime assumption in a path that runs on
# every file the agent writes. It is line surgery on purpose — parsing the YAML
# and re-emitting it would reorder keys, requote strings and drop comments in
# every note the agent touches, because Obsidian's property editor round-trips
# this block itself.
if ! awk -v agent="${AGENT_NAME}" -v now="${now}" -v created="${created}" '
    function chomp(s) { sub(/\r$/, "", s); return s }

    # A top-level key: no leading whitespace, and a space or end-of-line after
    # the colon. "agent:value" is the plain scalar "agent:value" in YAML, not a
    # mapping, and an indented "agent:" belongs to some other property.
    function keyof(s,   t) {
        t = chomp(s)
        if (t ~ /^[A-Za-z0-9_.-]+:([ \t].*)?$/) { sub(/:.*$/, "", t); return t }
        return ""
    }

    { line[NR] = $0 }

    END {
        n = NR
        fmEnd = 0
        if (n >= 2 && chomp(line[1]) == "---") {
            for (i = 2; i <= n; i++) {
                if (chomp(line[i]) == "---") { fmEnd = i; break }
            }
        }

        # Frontmatter and a leading horizontal rule are the same bytes, so the
        # delimiters alone prove nothing: "---\nsome prose\n---" is a rule
        # around a line of text, and inserting properties into it would corrupt
        # the note. Require the first non-empty line to be a key or a comment,
        # which is also when Obsidian parses the block as properties. The false
        # negative is safe — a fresh block goes above, content untouched.
        if (fmEnd > 0) {
            ok = 1
            for (i = 2; i < fmEnd; i++) {
                t = chomp(line[i])
                if (t == "") continue
                if (substr(t, 1, 1) != "#" && keyof(t) == "") ok = 0
                break
            }
            if (!ok) fmEnd = 0
        }

        if (fmEnd == 0) {
            printf "---\n"
            if (created == "1") printf "agent-created: %s\n", now
            printf "agent-modified: %s\n", now
            printf "agent: %s\n", agent
            printf "---\n"
            if (n > 0 && chomp(line[1]) != "") printf "\n"
            for (i = 1; i <= n; i++) print line[i]
            exit
        }

        seenAgent = seenModified = seenCreated = 0
        for (i = 2; i < fmEnd; i++) {
            cr = (line[i] ~ /\r$/) ? "\r" : ""
            k = keyof(line[i])
            if (k == "agent") { line[i] = "agent: " agent cr; seenAgent = 1 }
            else if (k == "agent-modified") { line[i] = "agent-modified: " now cr; seenModified = 1 }
            # agent-created is never rewritten: the first agent write is the
            # claim it makes, and agent-modified already says "touched since".
            else if (k == "agent-created") { seenCreated = 1 }
        }

        for (i = 1; i < fmEnd; i++) print line[i]
        if (created == "1" && !seenCreated) printf "agent-created: %s\n", now
        if (!seenModified) printf "agent-modified: %s\n", now
        if (!seenAgent) printf "agent: %s\n", agent
        for (i = fmEnd; i <= n; i++) print line[i]
    }
' "${real}" > "${tmp}" 2>/dev/null; then
    exit 0
fi

# Already correct: leave the file alone entirely rather than renaming identical
# bytes over it. An unnecessary write is an mtime change Obsidian Sync would
# propagate to every device for nothing.
if cmp -s "${tmp}" "${real}"; then
    exit 0
fi

# Notes must stay readable by the Mac over SMB; mktemp creates 0600.
chmod 0644 "${tmp}" 2>/dev/null || exit 0

# Same directory, so this is rename(2): a concurrent reader — the sync client —
# sees either the old note or the stamped one, never a half-written one.
if ! mv -f "${tmp}" "${real}" 2>/dev/null; then
    exit 0
fi

# Tell the agent the file on disk is no longer the bytes it wrote. Without this
# its next Edit of the same note can be refused as modified-since-read, and it
# has no way to know why.
jq -n --arg note "${rel}" '{
    hookSpecificOutput: {
        hookEventName: "PostToolUse",
        additionalContext: ("Agent stamp updated in the frontmatter of " + $note +
            ". The file on disk now differs from what you wrote; re-read it before editing it again.")
    }
}' 2>/dev/null || true

exit 0
