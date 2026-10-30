#!/usr/bin/env bash
#
# The compose healthcheck for vault-claude and vault-research.
#
# It judges nothing itself. supervise-rc.sh writes a verdict to /tmp/rc-health
# on every poll; this reads it, and adds the one thing the writer cannot say
# about itself: whether it is still writing. A verdict older than STALE_SECS
# means the supervisor has stopped looking, which is as unhealthy as a dead
# agent. See supervise-rc.sh, "HEALTH".
#
# Exit 0 healthy, 1 unhealthy — Docker's contract. The line printed is what
# `docker inspect` shows as the health log, so it names the reason.

set -euo pipefail

HEALTH_FILE=/tmp/rc-health
# The supervisor polls every AGENT_POLL_INTERVAL (10s by default) but can sleep
# for up to its 300s restart backoff between writes. Allow that plus margin.
STALE_SECS=420

if ! read -r stamp verdict reason 2>/dev/null < "${HEALTH_FILE}"; then
    echo "no verdict at ${HEALTH_FILE}: the supervisor is not running"
    exit 1
fi

age=$(( $(date +%s) - stamp ))
if [ "${age}" -gt "${STALE_SECS}" ]; then
    echo "last verdict is ${age}s old: the supervisor has stopped writing"
    exit 1
fi

echo "${verdict}: ${reason}"
[ "${verdict}" = healthy ]
