#!/usr/bin/env bash
#
# Claude Code hook entrypoint. Reads the hook payload on stdin, pulls out
# session_id, and calls snapshot.sh with an attributed message.
#
#   hook-snapshot.sh <prefix>        # e.g. "pre-agent" or "agent"
#
# Wired up in vault-claude-settings.json, which ships in this image and is
# installed into <vault>/.claude/settings.json on every agent start by
# install-settings.sh. See ARCHITECTURE.md#snapshots.
#
# Uses jq rather than a JS one-liner: one less runtime assumption in a path that
# runs on every session start and stop.
#
# ALWAYS exits 0. A snapshot failure must never block or fail an agent session —
# the worst case is a missing commit, which the hourly backstop will cover.

set -uo pipefail

prefix="${1:-snapshot}"
payload=""

# -t 5 so a hook invoked without stdin cannot hang the session.
if ! IFS= read -r -t 5 -d '' payload; then
    : # empty or timed out; fall through to the unknown-session path
fi

session_id=""
if [ -n "${payload}" ]; then
    session_id="$(printf '%s' "${payload}" | jq -r '.session_id // empty' 2>/dev/null || true)"
fi

if [ -z "${session_id}" ]; then
    session_id="unknown-$(date -u +%Y%m%dT%H%M%SZ)"
fi

"$(dirname "$0")/snapshot.sh" "${prefix}: ${session_id}" agent || true

exit 0
