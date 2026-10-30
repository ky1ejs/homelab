#!/usr/bin/env bash
#
# The compose healthcheck for vault-claude and vault-research.
#
# It judges nothing itself. supervise-rc.sh writes a verdict to HEALTH_FILE on
# every poll; this reads it, and adds the one thing the writer cannot say about
# itself: whether it is still writing. A verdict older than STALE_SECS means
# the supervisor has stopped looking, which is as unhealthy as a dead agent.
# See supervise-rc.sh, "HEALTH".
#
# THE FILE IS UNTRUSTED INPUT. This runs as the agent's uid, outside Claude
# Code's permission rules, every minute. The file lives where both tool
# policies deny writes, but this script must not depend on that: a line that
# is not exactly "<digits> healthy|unhealthy <text>" is rejected before any of
# it is used. That check is what stands between a forged file and a shell —
# bash evaluates `$(...)` inside an array subscript in arithmetic, so feeding
# an unchecked field to $(( )) runs it. Found in review on 2026-10-30.
#
# Exit 0 healthy, 1 unhealthy — Docker's contract. The line printed is what
# `docker inspect` shows as the health log, so it names the reason.

set -euo pipefail

HEALTH_FILE=/home/app/.rc-health
# The supervisor polls at most every 60s but can sleep for up to its 300s
# restart backoff between writes. Allow that plus margin.
STALE_SECS=420

if ! IFS= read -r line 2>/dev/null < "${HEALTH_FILE}"; then
    echo "no verdict at ${HEALTH_FILE}: the supervisor is not running"
    exit 1
fi

if ! [[ "${line}" =~ ^([0-9]{1,12})\ (healthy|unhealthy)\ ([[:print:]]{0,200})$ ]]; then
    echo "malformed verdict in ${HEALTH_FILE}"
    exit 1
fi
stamp="${BASH_REMATCH[1]}"
verdict="${BASH_REMATCH[2]}"
reason="${BASH_REMATCH[3]}"

# 10# forces base ten, so a stamp with a leading zero is not read as octal.
age=$(( $(date +%s) - 10#${stamp} ))
if [ "${age}" -gt "${STALE_SECS}" ]; then
    echo "last verdict is ${age}s old: the supervisor has stopped writing"
    exit 1
fi

printf '%s: %s\n' "${verdict}" "${reason}"
[ "${verdict}" = healthy ]
