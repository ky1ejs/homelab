#!/usr/bin/env bash
# shellcheck shell=bash
#
# One log format for every script in this image. Sourced, not executed.
#
#   . "$(dirname "$0")/log.sh"
#   log_init agent
#   info  "starting claude remote-control in tmux session ${SESSION}"
#   warn  "${VAULT_DIR}/.mcp.json is missing - the agent has no move_file"
#   fatal "refusing to start an agent with no tool policy"
#
# Every line is `[prefix] LEVEL message` on stderr:
#
#   [agent]    INFO  starting claude remote-control in tmux session vault
#   [settings] WARN  cannot write into /vault/.claude
#   [agent]    FATAL /vault/.claude/settings.json is missing.
#
# WHY THIS EXISTS. Ten scripts each defined their own `log()` with the same body
# and a different prefix, and severity was whatever text the author typed into
# the message: `WARNING:` in most, `FAILED:` in cron.sh, `FATAL:` in agent.sh.
# So the one question an operator actually asks of a container log — "did
# anything go wrong in here?" — needed a regex over three spellings that any new
# script could add a fourth to. One helper, three levels, and
# `docker logs vault-claude 2>&1 | grep -E 'WARN|FATAL'` is complete by
# construction.
#
# NO TIMESTAMP IS EMITTED, deliberately. Every one of these scripts runs as a
# container's process and its stderr goes to Docker's json-file driver, which
# already records a timestamp per line, at capture time, whether or not anyone
# asks for it. `docker logs -t`, `docker compose logs -t` and `homelab logs`
# print it. Stamping here would put a second, less trustworthy time on every
# line — this one is written when the script gets around to printing, the
# driver's when the line was actually captured — and would double up in exactly
# the view an operator reaches for. If these ever run somewhere with no log
# driver in front of them, add it here rather than in ten call sites.
#
# THE ONE PLACE THAT IS NOT TRUE is a captured pane replayed by agent-tmux.sh:
# those lines are stamped with the moment they were REPLAYED, some seconds after
# the agent printed them, because the transcript keeps no times of its own. They
# are marked with a leading `|` so they read as quoted evidence rather than as
# events happening now.
#
# `fatal` LOGS, it does not exit. Every existing caller prints one or more FATAL
# lines and then exits with a status it chose, sometimes after more cleanup, and
# a helper that exited on its own would silently swallow those. The level names
# the severity; the script keeps the control flow.

# Set by log_init. Defaults to the script's own filename so an un-initialised
# caller still produces a labelled line rather than an empty bracket.
LOG_PREFIX="${LOG_PREFIX:-$(basename "${0%.sh}")}"

# log_init <prefix>
#
# The name in brackets on every line. Use the SERVICE's name, not the script's,
# where they differ: an operator reading `docker compose logs` is looking at
# containers.
log_init() {
    LOG_PREFIX="$1"
}

# The workhorse. Levels are padded to five characters so messages line up in a
# terminal; FATAL is the longest and sets the width.
log_at() {
    local level="$1"
    shift
    printf '[%s] %-5s %s\n' "${LOG_PREFIX}" "${level}" "$*" >&2
}

info()  { log_at INFO  "$@"; }
warn()  { log_at WARN  "$@"; }
fatal() { log_at FATAL "$@"; }

# Kept so a call site this migration missed still logs, at INFO, instead of
# dying with "log: command not found" inside a container nobody is watching.
# New code should use the level helpers; `grep -rn 'log "' scripts/` finds
# whatever is left.
log() { log_at INFO "$@"; }
