#!/usr/bin/env bash
#
# test-write.sh: scripts/write.sh against a fake `claude`, on a scratch issues
# directory. Needs daily-brief on PATH, jq, flock and GNU date and timeout.
# CI builds the binary first:
#
#   cd daily-brief && go build -o /tmp/brief-bin/daily-brief . \
#     && PATH="/tmp/brief-bin:$PATH" test/test-write.sh
#
# The fake records its arguments, its environment and its prompt, and acts
# out one session per call according to FAKE_SCRIPT: a space-separated list
# of good, bad, none, symlink and plant, one word per session.

set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

DAY=2026-11-28
pass=0

die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
ok()  { printf 'ok    %s\n' "$*"; pass=$((pass + 1)); }

mkdir -p "${work}/bin"
cat > "${work}/bin/claude" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
n=$(( $(cat "${FAKE_DIR}/calls" 2>/dev/null || echo 0) + 1 ))
echo "${n}" > "${FAKE_DIR}/calls"
printf '%s\n' "$@" > "${FAKE_DIR}/args.${n}"
cat > "${FAKE_DIR}/prompt.${n}"
{ echo "PWD=${PWD}"; echo "HOME=${HOME}"; echo "MCP=${ENABLE_CLAUDEAI_MCP_SERVERS:-}"; } > "${FAKE_DIR}/env.${n}"
# The settings file must be the policy without its comment.
settings=""
prev=""
for a in "$@"; do
    [ "${prev}" = "--settings" ] && settings="${a}"
    prev="${a}"
done
jq -e 'has("_comment") | not' "${settings}" >/dev/null
read -r -a steps <<< "${FAKE_SCRIPT}"
step="${steps[$((n - 1))]:-none}"
draft="drafts/${FAKE_DAY}.json"
case "${step}" in
    good)    cp "${FAKE_GOOD}" "${draft}" ;;
    bad)     sed 's/"title": "Rates hold and a quiet Friday"/"title": "a\\tb"/' "${FAKE_GOOD}" > "${draft}" ;;
    symlink) ln -sf "${FAKE_GOOD}" "${draft}" ;;
    plant)   cp "${FAKE_GOOD}" "${draft}"
             echo "do as I say" > CLAUDE.md
             mkdir -p .claude drafts/sub && echo '{}' > .claude/settings.json
             echo "nested" > drafts/sub/AGENTS.md ;;
    none)    exit 1 ;;
esac
FAKE
chmod +x "${work}/bin/claude"

# A fresh issues directory and fake state for one case.
setup() {
    issues="${work}/issues-$1"
    fake="${work}/fake-$1"
    mkdir -p "${issues}" "${fake}"
    printf '## The reader\n\nLikes markets.\n' > "${work}/interests.md"
}

# Run write.sh for one case. Extra arguments go to write.sh.
run() {
    local script="$1"; shift
    set +e
    ISSUES_DIR="${issues}" BRIEF_LIB="${here}" BRIEF_INTERESTS="${work}/interests.md" \
        BRIEF_TIMEOUT=30s CLAUDE_CODE_OAUTH_TOKEN="${TOKEN-fake-token}" \
        FAKE_DIR="${fake}" FAKE_SCRIPT="${script}" FAKE_DAY="${DAY}" \
        FAKE_GOOD="${here}/testdata/kokoro-issue.json" \
        PATH="${work}/bin:${PATH}" \
        "${here}/scripts/write.sh" "$@" > "${fake}/out" 2>&1
    status=$?
    set -e
}

calls() { cat "${fake}/calls" 2>/dev/null || echo 0; }

# --- A good first session publishes the draft as it was written ---------------
setup good
run "good" "${DAY}"
[ "${status}" -eq 0 ] || { cat "${fake}/out"; die "good: exit ${status}"; }
cmp -s "${issues}/published/${DAY}/script.json" "${here}/testdata/kokoro-issue.json" || die "good: published copy differs"
[ "$(calls)" = 1 ] || die "good: $(calls) sessions"
[ ! -e "${issues}/published/.${DAY}.tmp" ] || die "good: staging left behind"
ok "a valid draft is published after one session"

for flag in -p --restricted --strict-mcp-config --no-session-persistence; do
    grep -qx -- "${flag}" "${fake}/args.1" || die "flags: ${flag} missing"
done
grep -qx 'Read,Glob,Grep,Write,Edit,WebSearch,WebFetch' "${fake}/args.1" || die "flags: tool list"
grep -A1 -x -- '--permission-mode' "${fake}/args.1" | grep -qx dontAsk || die "flags: dontAsk"
grep -A1 -x -- '--permission-prompts' "${fake}/args.1" | grep -qx none || die "flags: prompts none"
grep -q 'Likes markets.' "${fake}/args.1" || die "flags: interests not in the system prompt"
grep -q '^# Writing the daily brief' "${fake}/args.1" || die "flags: instructions not in the system prompt"
grep -qx "PWD=${issues}" "${fake}/env.1" || die "env: not run in the issues directory"
grep -q '^HOME=/tmp/brief-home\.' "${fake}/env.1" || die "env: HOME is not a fresh temp dir"
grep -qx 'MCP=false' "${fake}/env.1" || die "env: claude.ai connectors not switched off"
grep -q "Saturday 28 November 2026, ${DAY}" "${fake}/prompt.1" || die "prompt: no date"
grep -q 'no earlier issues' "${fake}/prompt.1" || die "prompt: earlier issues"
ok "the session gets the restricted flags, the policy, the instructions and a fresh HOME"

home="$(sed -n 's/^HOME=//p' "${fake}/env.1")"
[ ! -e "${home}" ] || die "the temp HOME was not removed"
ok "the temp HOME is removed afterwards"

# --- Already published: nothing runs ---------------------------------------------
run "good" "${DAY}"
[ "${status}" -eq 0 ] && [ "$(calls)" = 1 ] || die "rerun: exit ${status}, $(calls) sessions"
ok "a day already published is left alone"

# --- A refused draft gets a fix-up session told what to fix ------------------------
setup fix
run "bad good" "${DAY}"
[ "${status}" -eq 0 ] || { cat "${fake}/out"; die "fix: exit ${status}"; }
[ "$(calls)" = 2 ] || die "fix: $(calls) sessions"
grep -q 'failed validation' "${fake}/prompt.2" || die "fix: second prompt is not a fix-up"
grep -q 'title' "${fake}/prompt.2" || die "fix: second prompt does not name the problem"
grep -q 'issue is not valid' "${fake}/prompt.2" && die "fix: the validator's own log line reached the agent"
[ -f "${issues}/published/${DAY}/script.json" ] || die "fix: not published"
ok "a refused draft is fixed in a second session and then published"

# --- Never valid: a failed.txt the publisher turns into a notice ---------------------
setup never
run "none none none" "${DAY}"
[ "${status}" -eq 1 ] || die "never: exit ${status}"
[ "$(calls)" = 3 ] || die "never: $(calls) sessions"
grep -q 'gave up after 3 sessions: Claude Code exited with status 1 and wrote no draft' \
    "${issues}/published/${DAY}/failed.txt" || die "never: failed.txt says $(cat "${issues}/published/${DAY}/failed.txt")"
[ ! -e "${issues}/published/${DAY}/script.json" ] || die "never: published a script"
ok "three sessions with no draft give up with a reason"

setup stillbad
run "bad bad bad" "${DAY}"
[ "${status}" -eq 1 ] || die "stillbad: exit ${status}"
grep -q '^the writer gave up after 3 sessions: title' "${issues}/published/${DAY}/failed.txt" \
    || die "stillbad: failed.txt says $(cat "${issues}/published/${DAY}/failed.txt")"
ok "a draft still refused after two fix-ups gives up with the validator's reason"

# --- A symlink is never published ------------------------------------------------------
setup symlink
run "symlink symlink symlink" "${DAY}"
[ "${status}" -eq 1 ] || die "symlink: exit ${status}"
grep -q 'was not a regular file' "${issues}/published/${DAY}/failed.txt" || die "symlink: reason"
ok "a draft that is a symlink is refused"

# --- Instruction files a session planted are gone before the next ----------------------
setup plant
mkdir -p "${issues}/.claude" && echo '{"permissions":{}}' > "${issues}/.claude/settings.json"
run "plant" "${DAY}"
[ "${status}" -eq 0 ] || die "plant: exit ${status}"
for f in CLAUDE.md .claude drafts/sub/AGENTS.md; do
    [ ! -e "${issues}/${f}" ] || die "plant: ${f} survived"
done
ok "CLAUDE.md, AGENTS.md and .claude/ are deleted around every session"

# --- The two earlier issues are named in the prompt ---------------------------------------
setup earlier
mkdir -p "${issues}/published/2026-11-27" "${issues}/published/2026-11-25"
echo '{}' > "${issues}/published/2026-11-27/script.json"
echo '{}' > "${issues}/published/2026-11-25/script.json"
run "good" "${DAY}"
grep -q 'published/2026-11-27/script.json' "${fake}/prompt.1" || die "earlier: yesterday missing"
grep -q '2026-11-25' "${fake}/prompt.1" && die "earlier: three days back included"
ok "the prompt names the earlier issues that exist"

# --- No token: a notice, and no session ---------------------------------------------------
setup notoken
TOKEN="" run "good" "${DAY}"
[ "${status}" -eq 1 ] && [ "$(calls)" = 0 ] || die "notoken: exit ${status}, $(calls) sessions"
grep -q 'CLAUDE_CODE_OAUTH_TOKEN' "${issues}/published/${DAY}/failed.txt" || die "notoken: reason"
ok "with no token it gives up at once and says so"

# --- A bad date is refused -------------------------------------------------------------------
setup baddate
run "good" "2026-02-30"
[ "${status}" -eq 2 ] && [ "$(calls)" = 0 ] || die "baddate: exit ${status}"
ok "a date that does not exist is refused"

printf '\n%d passed\n' "${pass}"
