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
# Wired up in vault-claude-settings.json, which ships in this image and is
# installed into <vault>/.claude/settings.json on every agent start by
# install-settings.sh. See README.md#the-agent-stamp.
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
# Honours both halves of the shared contract, as vault.go does: temp-then-rename
# so no reader sees a torn note, AND a re-check of the bytes immediately before
# the rename so a read-modify-write cannot discard an edit `ob sync` landed in
# the meantime. This is the one place in this container where a second in-place
# writer would otherwise appear, and the note it rewrites is the one
# `ob sync --continuous` is most likely to be reading right now.
#
# And it checks its own work: a stamped note must be the note the agent wrote
# plus the stamp lines, or nothing is written at all. See "The invariant" below
# and DECISIONS.md#agent-stamps-in-frontmatter.
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

# The same shape vault-mcp validates at startup: ^[A-Za-z0-9][A-Za-z0-9._-]*$.
# The name is written into YAML unquoted, so a value carrying a colon or a
# newline would rewrite the note's properties rather than one of them. The
# leading-alphanumeric half was missing here while .env.example documented it,
# which is exactly the drift between the two implementations this file warns
# about at the top.
case "${AGENT_NAME}" in
    ""|[!A-Za-z0-9]*|*[!A-Za-z0-9._-]*) exit 0 ;;
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
# Read the exit code rather than negating it. `ls-files --error-unmatch` says
# untracked with 1, but says "I cannot answer" with 128 — not a repository, no
# readable index, a half-restored vault.git — and negation collapses the two.
# Every unanswerable case would then claim the agent created the note.
#
# A repository with no HEAD is the same kind of "cannot answer": nothing is
# tracked yet, so every note in the vault would look new.
created=0
if [ -d "${SNAPSHOT_DIR}/vault.git" ] &&
   git --git-dir="${SNAPSHOT_DIR}/vault.git" rev-parse --verify --quiet HEAD >/dev/null 2>&1; then
    git --git-dir="${SNAPSHOT_DIR}/vault.git" --work-tree="${vault_real}" \
        ls-files --error-unmatch -- "${rel}" >/dev/null 2>&1
    case "$?" in
        1) created=1 ;;   # definitively untracked: new since the pre-agent commit
        *) created=0 ;;   # tracked, or unanswerable
    esac
fi

now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
dir="$(dirname "${real}")"

# Both temp files are named *.tmp because that is what snapshot.sh writes into
# the repo's info/exclude. Without the suffix an orphan — a SIGKILL on the hook
# timeout, say — is committed on the next snapshot, into a repo DECISIONS.md
# says cannot be shrunk again without rewriting history.
pre=""
tmp=""

# shellcheck disable=SC2329  # invoked via trap
cleanup() {
    if [ -n "${pre}" ]; then rm -f -- "${pre}"; fi
    if [ -n "${tmp}" ]; then rm -f -- "${tmp}"; fi
    return 0
}
# Single quotes: the trap body is re-parsed by bash when the shell exits, so a
# path expanded into it now is executed as source code then. A note under a
# directory named  a'$(...)'b  was arbitrary command execution, from the one
# thing in this container that runs outside the agent's permission system. An
# apostrophe alone — "Kyle's notes" — broke the trap outright and leaked a temp
# file per write.
trap cleanup EXIT

pre="$(mktemp "${dir}/.vault-stamp-XXXXXX.tmp" 2>/dev/null || true)"
tmp="$(mktemp "${dir}/.vault-stamp-XXXXXX.tmp" 2>/dev/null || true)"
if [ -z "${pre}" ] || [ -z "${tmp}" ]; then
    exit 0
fi

# Work from a snapshot of the note, so the bytes awk read can be compared
# against the file just before the rename. See the verifyUnchanged note below.
if ! cp -- "${real}" "${pre}"; then
    exit 0
fi

# The stamping itself. awk rather than a node one-liner for the same reason
# hook-snapshot.sh uses jq: one less runtime assumption in a path that runs on
# every file the agent writes. It is line surgery on purpose — parsing the YAML
# and re-emitting it would reorder keys, requote strings and drop comments in
# every note the agent touches, because Obsidian's property editor round-trips
# this block itself.
if ! awk -v agent="${AGENT_NAME}" -v now="${now}" -v created="${created}" '
    function chomp(s) { sub(/\r$/, "", s); return s }

    # Two questions, two patterns — mirroring vault-mcp/stamp.go.
    #
    # keyof identifies OUR keys, for rewriting, and can be strict: only three
    # exact ASCII names ever need to match.
    #
    # isproperty answers "is this a property at all", which decides whether a
    # block is frontmatter, and must accept anything Obsidian would — spaces in
    # the name, non-ASCII, quotes. Using the strict pattern for both was a bug:
    # a note whose first property was `Date created:` had a second block
    # prepended above its own, demoting every real property to body text.
    #
    # Both require no leading whitespace and a space or end-of-line after the
    # colon: "agent:value" is the plain scalar "agent:value" in YAML, not a
    # mapping, and an indented "agent:" belongs to some other property.
    function keyof(s,   t) {
        t = chomp(s)
        if (t ~ /^[A-Za-z0-9_.-]+:([ \t].*)?$/) { sub(/:.*$/, "", t); return t }
        return ""
    }

    # A leading "-" is excluded because a block opening with a list item is a
    # sequence, not a mapping, and Obsidian shows no properties for it.
    function isproperty(s) {
        return chomp(s) ~ /^[^ \t#-][^:]*:([ \t].*)?$/
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
        # the note. Require a real property, which is also when Obsidian parses
        # the block as properties. The false negative is safe — a fresh block
        # goes above, content untouched.
        #
        # Comments are skipped rather than accepted. A YAML comment and a
        # markdown ATX heading are the same bytes, so "#" alone proves nothing
        # either: accepting it on the first line meant a note opening with a
        # rule above its title —
        #
        #     ---
        #     # Midge
        #     ...prose...
        #     ---
        #
        # — was read as frontmatter, and the stamp landed in the body, with a
        # rule further down the note promoted to the closing delimiter.
        # Skipping past comments to the first line that has to decide keeps the
        # comment support this was added for — a block genuinely opening with
        # "# schema v2" is still recognised — without letting a heading vouch
        # for a block that holds no properties at all.
        #
        # A block of nothing but comments is rejected for the same reason. A
        # block with no properties has nothing to lose from the safe reading.
        # An EMPTY block is not the same thing: "---\n---" is what Obsidian
        # leaves behind when the last property is deleted, and it really is
        # frontmatter.
        if (fmEnd > 0) {
            ok = 1          # an empty block, as Obsidian writes it
            seenComment = 0
            decided = 0
            for (i = 2; i < fmEnd; i++) {
                t = chomp(line[i])
                if (t == "") continue
                if (substr(t, 1, 1) == "#") { seenComment = 1; continue }
                ok = isproperty(t)
                decided = 1
                break
            }
            if (!decided && seenComment) ok = 0
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
' "${pre}" > "${tmp}" 2>/dev/null; then
    exit 0
fi

# Already correct: leave the file alone entirely rather than renaming identical
# bytes over it. An unnecessary write is an mtime change Obsidian Sync would
# propagate to every device for nothing.
if cmp -s "${tmp}" "${pre}"; then
    exit 0
fi

# --- The invariant: a stamp adds lines, it never edits the note ---------------
#
# Everything above this point decides WHERE the three lines go. This checks
# WHAT changed, independently of that reasoning, and refuses the write if the
# answer is anything but "the stamp". Every line the note arrived with must
# still be there, byte for byte and in order, with the two lines the stamp
# rewrites in place — `agent:` and `agent-modified:` — the only exceptions.
# `agent-created:` is not exempt: it is never rewritten, so it must survive
# untouched like any other line.
#
# This exists because the failure it guards against is invisible. A stamp in
# the wrong place, or a body line altered on the way past, is not a crash and
# not a diff anyone reads — it is one corrupted note in a vault of hundreds,
# found weeks later, if ever. A frontmatter misreading did exactly that above,
# and the fix for it is one more line of reasoning that can be wrong again.
# This says nothing about whether the reasoning is right; it says the note
# survived it.
#
# The cost of a false alarm is one unstamped note until the next agent write,
# which is the same cost every other refusal in this file pays. Cheap enough
# that the check does not need to be certain to be worth making.
if ! awk '
    function keyof(s,   t) {
        t = s
        sub(/\r$/, "", t)
        if (t ~ /^[A-Za-z0-9_.-]+:([ \t].*)?$/) { sub(/:.*$/, "", t); return t }
        return ""
    }
    FNR == NR { before[++nb] = $0; next }
    { after[++na] = $0 }
    END {
        j = 1
        for (i = 1; i <= nb; i++) {
            k = keyof(before[i])
            if (k == "agent" || k == "agent-modified") continue
            while (j <= na && after[j] != before[i]) j++
            if (j > na) exit 1
            j++
        }
    }
' "${pre}" "${tmp}"; then
    exit 0
fi

# The other half of the shared contract, and the half an atomic rename does not
# provide. This is a read-modify-write, and `ob sync --continuous` may land an
# edit from the Mac or the phone between the read above and the rename below;
# overwriting then discards that edit silently. vault.go closes the same window
# with verifyUnchanged, byte-for-byte rather than by mtime, and this is the
# cheap equivalent: if the note is no longer the bytes awk read, do nothing.
#
# Skipping costs a stamp until the next agent write. Overwriting costs someone's
# edit, recoverable only from the snapshot repo.
if ! cmp -s "${pre}" "${real}"; then
    exit 0
fi

# Notes must stay readable by the Mac over SMB; mktemp creates 0600.
chmod 0644 "${tmp}" 2>/dev/null || exit 0

# Same directory, so this is rename(2): a concurrent reader — the sync client —
# sees either the old note or the stamped one, never a half-written one.
if ! mv -f "${tmp}" "${real}" 2>/dev/null; then
    exit 0
fi
tmp=""   # renamed, not ours to clean up any more

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
