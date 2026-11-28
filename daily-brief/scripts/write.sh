#!/usr/bin/env bash
#
# brief-agent: write one day's issue with Claude Code, check it, publish it.
#
#   write.sh [--final] [DATE]   # DATE is YYYY-MM-DD; today in TZ when left out
#
# cron.sh runs it on BRIEF_SCHEDULE, and with --final on BRIEF_RETRY. For a
# first run, or to see it work:
#   docker compose exec brief-agent /usr/local/lib/daily-brief/write.sh
#
# Once published/<date>/ exists, every later run for that day exits at once,
# so the retry writes no second issue. A run that fails publishes nothing
# unless it is --final, so the day stays open for the retry, and the
# publisher keeps waiting for it. Only the final run turns a failure into
# published/<date>/failed.txt.
#
# One run is up to three Claude Code sessions: the first writes
# drafts/<date>.json, and up to two more fix what `daily-brief validate`
# refused, each a fresh session told only what to fix. The draft that passes
# is copied, checked again as the copy, and moved into published/<date>/ in
# one rename, so the voice job and the publisher never see half an issue. A
# day whose final run never passes gets published/<date>/failed.txt instead,
# and the publisher emails a short notice saying why.
#
# See DECISIONS.md#a-daily-brief-by-email-and-podcast and agent-settings.json
# for why the session is shaped the way it is.

set -euo pipefail

ISSUES_DIR="${ISSUES_DIR:-/issues}"
# BRIEF_LIB is a seam for test-write.sh, not an operator knob: compose names
# this container's variables one by one and does not pass it.
LIB="${BRIEF_LIB:-/usr/local/lib/daily-brief}"
INTERESTS="${BRIEF_INTERESTS:-/config/interests.md}"
TIMEOUT="${BRIEF_TIMEOUT:-45m}"
MAX_RUNS=3
MAX_INTERESTS_BYTES=16384
DRAFT_RETENTION_DAYS=14

log() { printf '[write] %s\n' "$*" >&2; }

# --- What this container must not have --------------------------------------
#
# A hard stop, as in obsidian-vault/scripts/research.sh and for the same
# reason: this session reads the open web and can send what it reads anywhere
# a URL goes. /state holds the Gmail grant, /podcast the Spotify login, /audio
# is the voice job's, and the last four are the vault stack's. The Dockerfile
# creates /state, /podcast and /audio so all four roles share one layout, so a
# plain -d test would always fire; a bind mount shows as a different device
# number, and the non-empty test backs that up where it does not.
root_dev="$(stat -c %d / 2>/dev/null || echo 0)"
for forbidden in /state /podcast /audio /vault /snapshots /backups /scratch; do
    [ -d "${forbidden}" ] || continue
    mounted=0
    dev="$(stat -c %d "${forbidden}" 2>/dev/null || echo "${root_dev}")"
    if [ "${dev}" != "${root_dev}" ]; then
        mounted=1
    fi
    if [ -n "$(ls -A "${forbidden}" 2>/dev/null)" ]; then
        mounted=1
    fi
    if [ "${mounted}" -eq 1 ]; then
        log "FATAL: ${forbidden} is mounted into brief-agent, which has web"
        log "FATAL: search and web fetch. Remove the mount from docker-compose.yml."
        exit 1
    fi
done

# --- The day ------------------------------------------------------------------

last_run=0
day=""
for arg in "$@"; do
    case "${arg}" in
        --final) last_run=1 ;;
        -*) log "usage: write.sh [--final] [DATE]"; exit 2 ;;
        *)
            if [ -n "${day}" ]; then
                log "usage: write.sh [--final] [DATE]"
                exit 2
            fi
            day="${arg}"
            ;;
    esac
done
day="${day:-$(date +%F)}"
if ! [[ "${day}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] \
    || [ "$(date -d "${day}" +%F 2>/dev/null || true)" != "${day}" ]; then
    log "FAILED: DATE must be a real date as YYYY-MM-DD, got '${day}'"
    exit 2
fi

if [ ! -d "${ISSUES_DIR}" ] || [ ! -w "${ISSUES_DIR}" ]; then
    log "FATAL: ${ISSUES_DIR} is missing or not writable (preflight.sh --fix)"
    exit 1
fi
mkdir -p "${ISSUES_DIR}/drafts" "${ISSUES_DIR}/published"

# One writer at a time: a run can take over two hours, and the retry, or a
# manual run, could otherwise overlap it. The final run waits for the one
# before it instead of skipping, so a day that is still being written when
# the retry fires still ends with an issue or a notice.
exec 9>"${ISSUES_DIR}/.write.lock"
if [ "${last_run}" -eq 1 ]; then
    flock 9
elif ! flock -n 9; then
    log "another run is still writing; skipping"
    exit 0
fi

final="${ISSUES_DIR}/published/${day}"
staging="${ISSUES_DIR}/published/.${day}.tmp"
draft="${ISSUES_DIR}/drafts/${day}.json"

if [ -e "${final}" ]; then
    log "${day}: already published"
    exit 0
fi

home=""
settings="$(mktemp /tmp/brief-settings.XXXXXX)"
# shellcheck disable=SC2329  # run by the EXIT trap
cleanup() {
    rm -rf -- "${settings}" "${staging}"
    if [ -n "${home}" ]; then
        rm -rf -- "${home}"
    fi
}
trap cleanup EXIT

# Publish the staging directory as published/<day>, in one rename.
publish_staging() {
    local f
    for f in "${staging}"/*; do
        sync -- "${f}"
    done
    mv -T -- "${staging}" "${final}"
}

# A day the writer cannot produce still gets an email: the publisher sends a
# notice with this reason, capped and cleaned on its side. Only the final run
# publishes it; an earlier one leaves the day open for the retry.
give_up() {
    if [ "${last_run}" -ne 1 ]; then
        log "${day}: failed: $1"
        log "${day}: not publishing a notice, so the final run can try again"
        exit 1
    fi
    rm -rf -- "${staging}"
    mkdir -- "${staging}"
    printf '%s\n' "$1" > "${staging}/failed.txt"
    publish_staging
    log "${day}: gave up: $1"
    exit 1
}

# --- What the session needs -------------------------------------------------

if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
    give_up "the writer has no Claude Code token: run claude setup-token and set CLAUDE_CODE_OAUTH_TOKEN in .env"
fi
if [ ! -f "${INTERESTS}" ] || [ ! -s "${INTERESTS}" ]; then
    give_up "the writer found no interests file: copy interests.example.md to interests.md beside docker-compose.yml"
fi
if [ "$(stat -c %s "${INTERESTS}")" -gt "${MAX_INTERESTS_BYTES}" ]; then
    give_up "interests.md is over ${MAX_INTERESTS_BYTES} bytes; shorten it"
fi

# The policy, without its comment: see agent-settings.json on why.
if ! jq 'del(._comment)' "${LIB}/agent-settings.json" > "${settings}"; then
    give_up "the writer could not read its tool policy; the image is broken"
fi

instructions="$(cat "${LIB}/agent-instructions.md")"$'\n\n'"$(cat "${INTERESTS}")"

# The two issues before this one, so the agent does not repeat them.
earlier=""
for back in 1 2; do
    d="$(date -d "${day} -${back} day" +%F)"
    if [ -f "${ISSUES_DIR}/published/${d}/script.json" ]; then
        earlier="${earlier} published/${d}/script.json"
    fi
done

first_prompt() {
    printf 'Write the daily brief for %s, %s. Save it as drafts/%s.json.\n' \
        "$(date -d "${day}" '+%A %-d %B %Y')" "${day}" "${day}"
    if [ -n "${earlier}" ]; then
        printf 'The earlier issues to avoid repeating are:%s\n' "${earlier}"
    else
        printf 'There are no earlier issues yet.\n'
    fi
}

fix_prompt() {
    printf 'drafts/%s.json failed validation:\n%s\n\n' "${day}" "$1"
    printf 'Read drafts/%s.json, fix only those problems, and save it again. Do not research again.\n' "${day}"
}

# Claude Code reads these as standing instructions. The agent is denied
# writing them, and they are deleted before every session anyway, so nothing a
# page talked one session into can reach the next. find, because a nested one
# is read too when the agent reads that directory.
clean() {
    find "${ISSUES_DIR}" -mindepth 1 \
        \( -name CLAUDE.md -o -name CLAUDE.local.md -o -name AGENTS.md \
           -o -name .claude -o -name .mcp.json \) \
        -prune -exec rm -rf -- {} +
}

# One session. The prompt goes on stdin: --tools and --mcp-config each take a
# list, and would swallow a prompt passed after them as one more item.
#
# Each session gets a HOME of its own, removed when it ends: Claude Code keeps
# files of its own under ~ (~/.claude.json, and auto-memory, which the policy
# and CLAUDE_CODE_DISABLE_AUTO_MEMORY also turn off), and a fix-up session
# follows one that read hostile pages.
status=0
run_claude() {
    local args=(
        -p
        --restricted
        --tools "Read,Glob,Grep,Write,Edit,WebSearch,WebFetch"
        --permission-mode dontAsk
        --permission-prompts none
        --settings "${settings}"
        --strict-mcp-config
        --mcp-config '{"mcpServers":{}}'
        --no-session-persistence
        --append-system-prompt "${instructions}"
    )
    if [ -n "${BRIEF_MODEL:-}" ]; then
        args+=(--model "${BRIEF_MODEL}")
    fi
    clean
    home="$(mktemp -d /tmp/brief-home.XXXXXX)"
    status=0
    printf '%s\n' "$1" | (
        cd "${ISSUES_DIR}"
        HOME="${home}" ENABLE_CLAUDEAI_MCP_SERVERS=false DISABLE_AUTOUPDATER=1 \
            CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 \
            timeout --kill-after=60s "${TIMEOUT}" claude "${args[@]}"
    ) || status=$?
    rm -rf -- "${home}"
    home=""
    clean
    log "${day}: the session ended with status ${status}"
}

# Copy the draft, then check the copy: the bytes that pass are the bytes that
# are published. Sets problems and returns 1 when there is nothing to publish.
problems=""
check_draft() {
    local out
    rm -rf -- "${staging}"
    mkdir -- "${staging}"
    if [ ! -e "${draft}" ] && [ ! -L "${draft}" ]; then
        if [ "${status}" -eq 124 ] || [ "${status}" -eq 137 ]; then
            problems="the session ran out of time (${TIMEOUT}) and wrote no draft"
        elif [ "${status}" -ne 0 ]; then
            problems="Claude Code exited with status ${status} and wrote no draft"
        else
            problems="the session wrote no draft"
        fi
        return 1
    fi
    if [ -L "${draft}" ] || [ ! -f "${draft}" ]; then
        rm -rf -- "${draft}"
        problems="drafts/${day}.json was not a regular file"
        return 1
    fi
    cp -- "${draft}" "${staging}/script.json"
    if out="$(daily-brief validate "${staging}/script.json" "${day}" 2>&1)"; then
        return 0
    fi
    problems="$(printf '%s\n' "${out}" | grep -v '^daily-brief validate: issue is not valid$' || true)"
    problems="${problems:-the validator refused the draft without saying why}"
    return 1
}

# --- Write it -------------------------------------------------------------------

find "${ISSUES_DIR}/drafts" -maxdepth 1 -type f -name '*.json' \
    -mtime +"${DRAFT_RETENTION_DAYS}" -delete
rm -rf -- "${draft}"

log "${day}: writing"
prompt="$(first_prompt)"
for run in $(seq 1 "${MAX_RUNS}"); do
    run_claude "${prompt}"
    if check_draft; then
        publish_staging
        log "${day}: published after ${run} session(s)"
        exit 0
    fi
    log "${day}: session ${run} of ${MAX_RUNS}: $(printf '%s\n' "${problems}" | head -n1)"
    if [ -f "${draft}" ]; then
        prompt="$(fix_prompt "${problems}")"
    else
        prompt="$(first_prompt)"
    fi
done

give_up "the writer gave up after ${MAX_RUNS} sessions: $(printf '%s\n' "${problems}" | head -n3 | tr '\n' ' ')"
