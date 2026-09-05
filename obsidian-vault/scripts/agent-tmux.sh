#!/usr/bin/env bash
# shellcheck shell=bash
#
# Shared tmux supervision for the two agent entrypoints, agent.sh and
# research.sh. Sourced, not executed: it defines run_agent_session and the
# helpers that report on a session that has ended.
#
# WHY THIS EXISTS. Both scripts start `claude remote-control` in a detached tmux
# session and hold the container open for as long as that session lives. When
# the agent exits during startup, tmux closes the window and takes the pane's
# scrollback with it, so the entire event used to reach the operator as:
#
#   [agent] starting claude remote-control in tmux session vault
#   [agent] attach with: docker exec -it $(hostname) tmux attach -t vault
#   [agent] tmux session ended
#
# repeated every few seconds by `restart: unless-stopped`, with the reason
# discarded each time and `homelab attach claude` finding nothing to attach to.
# The agent's own error message existed and was printed; nothing kept it. An
# expired Claude login is the documented case — README.md's monitoring row calls
# it "that last one silently stops the agent" — and this is the "silently".
#
# So the pane is watched two ways, and when the session ends a bounded,
# sanitised extract is logged. The reason then survives in `docker logs
# vault-claude`, which is also what the dashboard's (authentication-gated)
# `logs` button shows.
#
# TWO CAPTURES, because the two failures need different evidence:
#
#   the transcript  pipe-pane, attached with the session, bounded to the FIRST
#                   ${AGENT_TRANSCRIPT_MAX} bytes. This is the crash-loop case:
#                   an agent that never got past startup printed everything it
#                   ever printed inside that bound.
#   the snapshot    capture-pane, re-taken on every poll. remote-control is a
#                   TUI that redraws, so a session running for days fills any
#                   byte bound with repainted frames long before it dies; what
#                   is worth having then is its last screen, not its first.
#
# Which one is reported is decided by how long the session lived, not by which
# happens to be populated: a session that died in three seconds has a nearly
# empty snapshot and a complete transcript, and picking the fuller-looking file
# would report the emptier one.
#
# THE TRANSCRIPT IS DUMPED ONLY AFTER THE SESSION HAS ENDED, which is what makes
# it safe to put in a log: the one secret remote-control prints is the pairing
# URL, and a pairing URL for a session that no longer exists is not a
# credential. Nothing here logs a pane that is still live.
#
# The caller supplies log(); this file uses it so each agent's lines keep their
# own prefix. See DECISIONS.md#saying-why-the-agent-stopped.
#
# Knobs, all with working defaults and all deliberately absent from .env.example
# for the reason AGENT_POLL_INTERVAL already is: they are seams for testing this
# file outside an image, not operator configuration.
#
#   AGENT_POLL_INTERVAL     10      seconds between liveness checks
#   AGENT_FAST_EXIT_SECONDS 60      under this, a session counts as a crash loop
#   AGENT_FAILURE_BACKOFF   30      seconds to wait before exiting on one
#   AGENT_TRANSCRIPT_MAX    262144  bytes of pane output kept from startup
#   AGENT_TRANSCRIPT_LINES  40      lines logged, and lines of screen captured

# Set by run_agent_session so the trap can find the session, and so the report
# can tell "the agent died" from "the container is being stopped".
AGENT_SESSION=""
AGENT_SHUTTING_DOWN=0

# shellcheck disable=SC2329,SC2317  # invoked via trap; code varies by shellcheck version
agent_cleanup() {
    AGENT_SHUTTING_DOWN=1
    if [ -n "${AGENT_SESSION}" ] && tmux has-session -t "${AGENT_SESSION}" 2>/dev/null; then
        log "killing tmux session ${AGENT_SESSION}"
        tmux kill-session -t "${AGENT_SESSION}" 2>/dev/null || true
    fi
    return 0
}

# agent_log_transcript <file> <lines>
#
# The captured pane, made fit for a container log. CSI escape sequences are
# removed and every remaining control character and non-ASCII byte is dropped,
# which also collapses the Unicode half-blocks remote-control draws its pairing
# QR with — a QR code pasted into `docker logs` one line at a time would bury
# the error underneath it. Lines are then truncated and only the last few kept,
# because this runs on a crash loop and must not become the flood it is there
# to explain.
#
# `|| true` on the pipeline: the caller runs under `set -e` with `pipefail`, and
# a diagnostic that aborts the diagnosis is worse than a missing line.
agent_log_transcript() {
    local file="$1" lines="$2" esc line
    esc="$(printf '\033')"

    # `#` as the s/// delimiter: the CSI pattern's parameter and intermediate
    # byte ranges both contain `/`.
    sed "s#${esc}\[[0-9;?]*[ -/]*[@-~]##g" "${file}" 2>/dev/null \
        | tr -cd '\11\12\40-\176' \
        | sed -e 's/[[:space:]]*$//' -e '/^$/d' \
        | cut -c1-400 \
        | tail -n "${lines}" \
        | while IFS= read -r line; do
              log "| ${line}"
          done || true
}

# run_agent_session <session> <workdir>
#
# Starts `claude remote-control` in a detached tmux session rooted at <workdir>,
# holds the container open for as long as it lives, and reports why it ended.
#
# Always returns non-zero: reaching the end means there is no agent any more,
# and the container should exit so `restart: unless-stopped` builds a fresh one.
# Exiting when tmux dies is deliberate — see the note in agent.sh.
run_agent_session() {
    local session="$1" workdir="$2"
    local poll="${AGENT_POLL_INTERVAL:-10}"
    local fast="${AGENT_FAST_EXIT_SECONDS:-60}"
    local backoff="${AGENT_FAILURE_BACKOFF:-30}"
    local cap="${AGENT_TRANSCRIPT_MAX:-262144}"
    local tail_lines="${AGENT_TRANSCRIPT_LINES:-40}"
    local transcript snapshot pipe_cmd started ended ran started_here=1

    AGENT_SESSION="${session}"
    trap agent_cleanup EXIT INT TERM

    # In the container's own filesystem, never in the vault or the scratch
    # volume: those are synced, committed and swept, and a pane transcript is
    # none of their business. It dies with the container, which is why the tail
    # is logged rather than left here to be found.
    #
    # mktemp rather than a fixed name, because /tmp is world-writable and a
    # symlink planted at a predictable path would redirect the capture.
    transcript="$(mktemp "${TMPDIR:-/tmp}/agent-${session}-XXXXXX.log" 2>/dev/null || true)"
    snapshot="$(mktemp "${TMPDIR:-/tmp}/agent-${session}-XXXXXX.screen" 2>/dev/null || true)"

    # `head -c` bounds the file: this is an always-on session that may run for
    # weeks, and an unbounded `cat >>` would fill the container's writable layer
    # with pane output nobody reads. Keeping the FIRST bytes is the right half
    # for what this file is read for — the startup that failed — and when head
    # exits tmux simply stops piping. The snapshot covers the other half.
    pipe_cmd="head -c ${cap} >> '${transcript}'"

    if tmux has-session -t "${session}" 2>/dev/null; then
        # Defensive: nothing in this stack re-runs the entrypoint against a live
        # tmux server, because the server dies with the container. If it ever
        # does, the timings below are this supervisor's, not the session's, and
        # the crash-loop diagnosis has to stay out of it.
        started_here=0
        log "tmux session ${session} already exists, reusing"
        if [ -n "${transcript}" ]; then
            tmux pipe-pane -o -t "${session}" "${pipe_cmd}" 2>/dev/null || true
        fi
    else
        log "starting claude remote-control in tmux session ${session}"
        # ONE tmux invocation, with pipe-pane in the same command list. The
        # capture has to be attached before the agent writes anything — pipe-pane
        # copies what the pane produces from then on, not what it has already
        # printed — and a second `tmux` process would be racing the very startup
        # it exists to observe. Commands separated by `\;` run back to back in
        # the server before it returns to its event loop.
        if [ -n "${transcript}" ]; then
            if ! tmux new-session -d -s "${session}" -c "${workdir}" "claude remote-control" \
                 \; pipe-pane -o -t "${session}" "${pipe_cmd}"; then
                log "FATAL: could not start tmux session ${session}"
                return 1
            fi
        else
            log "WARNING: cannot create a transcript in ${TMPDIR:-/tmp} — if the"
            log "WARNING: agent exits, this log will not be able to say why."
            if ! tmux new-session -d -s "${session}" -c "${workdir}" "claude remote-control"; then
                log "FATAL: could not start tmux session ${session}"
                return 1
            fi
        fi
    fi

    log "attach with: docker exec -it \$(hostname) tmux attach -t ${session}"

    started="$(date +%s)"
    while tmux has-session -t "${session}" 2>/dev/null; do
        # Re-taken every poll and written to a temp file first: the pane can
        # disappear mid-capture, and a half-written screen replacing a whole one
        # would lose the very evidence this is here to keep.
        if [ -n "${snapshot}" ]; then
            if tmux capture-pane -p -t "${session}" -S "-${tail_lines}" > "${snapshot}.new" 2>/dev/null; then
                mv -f "${snapshot}.new" "${snapshot}" 2>/dev/null || true
            fi
            rm -f "${snapshot}.new" 2>/dev/null || true
        fi
        sleep "${poll}"
    done
    ended="$(date +%s)"
    ran=$(( ended - started ))

    # A stop, not a failure: docker sent SIGTERM, the trap killed the session,
    # and this loop noticed. Reporting a crash here would put a made-up
    # diagnosis in the log every time the operator restarts the stack.
    if [ "${AGENT_SHUTTING_DOWN}" = "1" ]; then
        log "tmux session ended (container is stopping)"
        return 1
    fi

    # "within", not "after": the loop polls every ${poll}s, so this is an upper
    # bound on how long the agent actually lived, not a measurement of it.
    if [ "${started_here}" = "1" ]; then
        log "tmux session ended within ${ran}s of starting"
    else
        log "tmux session ended within ${ran}s of this container picking it up"
    fi

    # Chosen by lifetime, not by which file looks fuller. See the header.
    if [ "${ran}" -le "${fast}" ] && [ -n "${transcript}" ] && [ -s "${transcript}" ]; then
        log "what the agent printed before it stopped (last ${tail_lines} lines):"
        agent_log_transcript "${transcript}" "${tail_lines}"
    elif [ -n "${snapshot}" ] && [ -s "${snapshot}" ]; then
        log "the agent's screen as of the last check, up to ${poll}s before it stopped:"
        agent_log_transcript "${snapshot}" "${tail_lines}"
    elif [ -n "${transcript}" ] && [ -s "${transcript}" ]; then
        log "no screen capture; the tail of the first ${cap} bytes it printed:"
        agent_log_transcript "${transcript}" "${tail_lines}"
    else
        log "it printed nothing at all before stopping, which points at the"
        log "process being killed (mem_limit) rather than at an error it hit."
    fi

    # An agent that did not survive its first minute never became something you
    # could pair with, so say so in the words the symptom appears in: the phone
    # cannot connect and `homelab attach` finds no session.
    if [ "${ran}" -le "${fast}" ] && [ "${started_here}" = "1" ]; then
        log "this is a crash loop, not a session waiting for a phone. Usual causes:"
        log "  * the Claude login in this container's /home/app volume has expired"
        log "    or is missing — re-run the interactive login for THIS agent"
        log "    (README.md setup step 5; each agent has its own volume)"
        log "  * both agents pointed at one home volume, which corrupts"
        log "    ~/.claude.json — scripts/preflight.sh checks for this"
        log "  * the container hit mem_limit and the agent was OOM-killed"
        # Docker's own restart backoff resets once a container has run for ten
        # seconds, and this one always does — the poll interval alone outlasts
        # it. Without this the loop restarts as fast as the agent can fail, and
        # the log becomes too noisy to read the reason out of.
        log "waiting ${backoff}s before exiting, to keep this loop readable"
        sleep "${backoff}"
    fi

    return 1
}
