#!/usr/bin/env bash
#
# preflight.sh — assert the NAS is in the state the stack expects.
#
#   ./preflight.sh          # check only, change nothing
#   ./preflight.sh --fix    # additionally create/chown/chmod what is wrong
#
# Run it on the NAS, as root, from the directory holding docker-compose.yml and
# .env. Safe to run repeatedly — that is the point. Run it before the first
# `up -d`, and again after any QNAP change (new account, new share, volume
# migration), because most of what it checks is drift you cannot see by looking.
#
# It reads .env rather than hardcoding paths or ids, so this script and the
# compose file cannot disagree about what the setup is.
#
# Exit codes: 0 all good, 1 problems remain, 2 could not run the checks.
#
# QTS ships bash, but if yours does not, run it out of the image instead:
#   docker run --rm -u 0 -v /share/<vol>:/share/<vol> -v "$PWD:/cfg" -w /cfg \
#     --entrypoint /usr/local/lib/vault/preflight.sh \
#     ghcr.io/ky1ejs/homelab/obsidian-vault:latest

set -euo pipefail

ENV_FILE="${ENV_FILE:-./.env}"
FIX=0
[ "${1:-}" = "--fix" ] && FIX=1 || true   # NOTE: `|| true` is load-bearing under set -e

fail=0
warn=0

ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; fail=$((fail + 1)); }
note() { printf '  \033[33mwarn\033[0m  %s\n' "$*"; warn=$((warn + 1)); }
did()  { printf '  \033[36mfixed\033[0m %s\n' "$*"; }
head_() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# Parse one key out of .env. Deliberately NOT `. .env` — that file contains
# values with spaces (AGENT_GIT_NAME=Claude Code) which sourcing would execute
# as a command. docker compose parses env_file itself and does not use a shell;
# so do we.
env_get() {
    local key="$1" line value
    line="$(grep -E "^[[:space:]]*${key}=" "${ENV_FILE}" 2>/dev/null | tail -n1)" || true
    if [ -z "${line}" ]; then
        printf ''
        return 0
    fi
    value="${line#*=}"
    value="${value%%[[:space:]]#*}"          # strip ` # inline comment`
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    printf '%s' "${value}"
}

# ---------------------------------------------------------------------------

head_ "Config"

if [ ! -f "${ENV_FILE}" ]; then
    bad "${ENV_FILE} not found — copy .env.example to .env first"
    printf '\nCannot continue without .env.\n'
    exit 2
fi
ok ".env present"

env_mode="$(stat -c '%a' "${ENV_FILE}" 2>/dev/null || printf '???')"
if [ "${env_mode}" = "600" ]; then
    ok ".env is 0600"
elif [ "${FIX}" = "1" ]; then
    chmod 600 "${ENV_FILE}"
    did ".env 0${env_mode} -> 0600"
else
    bad ".env is 0${env_mode}, want 0600  (--fix, or: chmod 600 ${ENV_FILE})"
fi

APP_UID="$(env_get APP_UID)"
APP_GID="$(env_get APP_GID)"
IMAGE="$(env_get IMAGE)"

if [ -z "${APP_UID}" ] || [ -z "${APP_GID}" ]; then
    bad "APP_UID / APP_GID not set in ${ENV_FILE}"
    exit 2
fi
ok "identity ${APP_UID}:${APP_GID}"

VAULT="$(env_get VAULT_HOST_PATH)"
SNAPSHOTS="$(env_get SNAPSHOT_HOST_PATH)"
BACKUPS="$(env_get BACKUP_HOST_PATH)"
SYNC_HOME="$(env_get SYNC_HOME_HOST_PATH)"
AGENT_HOME="$(env_get AGENT_HOME_HOST_PATH)"
# The research surface. Absent from an .env written before it existed, so these
# two are checked further down rather than in the required loop below — an older
# .env should report "not configured", not fail the whole preflight.
SCRATCH="$(env_get SCRATCH_HOST_PATH)"
RESEARCH_HOME="$(env_get RESEARCH_HOME_HOST_PATH)"

for v in VAULT SNAPSHOTS BACKUPS SYNC_HOME AGENT_HOME; do
    if [ -z "${!v}" ]; then
        bad "${v}_HOST_PATH is empty in ${ENV_FILE}"
        exit 2
    fi
done

# ---------------------------------------------------------------------------

head_ "Identity"

# The uid must belong to a real account. QNAP allocates sequentially from 1000,
# so an *unallocated* uid is simply the next account someone creates — which
# would then own the 0700 credential directories. See DECISIONS.md#identity-1002-not-1000.
if owner_name="$(getent passwd "${APP_UID}" 2>/dev/null | cut -d: -f1)" && [ -n "${owner_name}" ]; then
    ok "uid ${APP_UID} belongs to a real account (${owner_name})"
else
    bad "uid ${APP_UID} is not allocated to any account — the next QNAP account created will inherit ownership of the credential directories"
fi

# The image bakes APP_UID as a build arg. Overriding `user:` in compose to a uid
# the image does not know makes Docker resolve $HOME to / instead of /home/app,
# and BOTH interactive logins then appear to succeed and fail to persist. This
# is the single most confusing failure in the whole setup, so check it directly.
if [ -n "${IMAGE}" ] && command -v docker >/dev/null 2>&1; then
    if baked="$(docker run --rm --entrypoint /usr/bin/id "${IMAGE}" -u app 2>/dev/null)"; then
        if [ "${baked}" = "${APP_UID}" ]; then
            ok "image's baked uid matches APP_UID (${baked})"
        else
            bad "image bakes uid ${baked} but .env says ${APP_UID} — \$HOME will resolve to / and logins will not persist. Rebuild the image, or run: docker compose pull"
        fi
    else
        note "could not inspect ${IMAGE} (not pulled yet?) — re-run after 'docker compose pull'"
    fi
else
    note "docker not available or IMAGE unset; skipped the baked-uid check"
fi

# ---------------------------------------------------------------------------

head_ "Directories"

check_dir() {
    local path="$1" want_mode="$2" label="$3" mode uid gid

    if [ ! -d "${path}" ]; then
        if [ "${FIX}" = "1" ]; then
            mkdir -p "${path}"
            did "created ${path}"
        else
            bad "${label}: ${path} does not exist  (--fix creates it)"
            return 0
        fi
    fi

    uid="$(stat -c '%u' "${path}")"
    gid="$(stat -c '%g' "${path}")"
    if [ "${uid}:${gid}" = "${APP_UID}:${APP_GID}" ]; then
        ok "${label}: owned ${APP_UID}:${APP_GID}"
    elif [ "${FIX}" = "1" ]; then
        chown -R "${APP_UID}:${APP_GID}" "${path}"
        did "${label}: ${uid}:${gid} -> ${APP_UID}:${APP_GID}"
    else
        bad "${label}: owned ${uid}:${gid}, want ${APP_UID}:${APP_GID}  (--fix)"
    fi

    if [ -n "${want_mode}" ]; then
        mode="$(stat -c '%a' "${path}")"
        if [ "${mode}" = "${want_mode}" ]; then
            ok "${label}: mode 0${want_mode}"
        elif [ "${FIX}" = "1" ]; then
            chmod "${want_mode}" "${path}"
            did "${label}: 0${mode} -> 0${want_mode}"
        else
            bad "${label}: mode 0${mode}, want 0${want_mode}  (--fix)"
        fi
    fi
}

check_dir "${VAULT}"      ""    "vault"
check_dir "${SNAPSHOTS}"  ""    "snapshots"
check_dir "${BACKUPS}"    ""    "backups"
check_dir "${SYNC_HOME}"  "700" "home-sync"
check_dir "${AGENT_HOME}" "700" "home-agent"
# Guarded, because an .env written before the research surface existed has
# neither. The "Research surface" section below reports that as a note.
#
# `if`, not `[ -n "$X" ] && check_dir ...`, because the guarded call reads
# better as a block. Not for the reason an earlier draft of this comment gave:
# `set -e` does NOT exit on a failing test inside an `&&` list, since every
# command in such a list except the last is exempt. The genuine hazard is an
# `&&` list as the final command of a script or function, where its status
# becomes the exit status.
if [ -n "${SCRATCH}" ]; then
    check_dir "${SCRATCH}" "" "scratch"
    # The jobs directory is a MOUNT POINT, not just a folder. vault-claude
    # bind-mounts it read-write nested inside its otherwise read-only /scratch,
    # and Docker cannot create a mount point inside a read-only bind mount — so
    # a missing jobs/ is not a degraded handoff, it is a vault-claude that will
    # not start. Checking it here, on the host, is where the fix belongs.
    # See DECISIONS.md#passing-a-brief-to-the-research-agent.
    check_dir "${SCRATCH}/jobs" "" "scratch/jobs"
fi
if [ -n "${RESEARCH_HOME}" ]; then
    check_dir "${RESEARCH_HOME}" "700" "home-research"
fi

# ---------------------------------------------------------------------------

head_ "Volume"

# QNAP marks encrypted volumes with a CE_ prefix on the mount point. Volume
# encryption defends drives that leave the building; it does nothing against a
# running NAS. See ARCHITECTURE.md#trust-boundary.
vol="$(printf '%s' "${VAULT}" | cut -d/ -f1-3)"
case "${vol}" in
    /share/CE_*) ok "on an encrypted volume (${vol})" ;;
    *)           note "${vol} is not a CE_ (encrypted) volume — ARCHITECTURE.md#trust-boundary assumes it is" ;;
esac

# All five must share the volume; one on a plain volume silently drops out of
# the encrypted set, and nothing about the running system would look different.
for p in "${SNAPSHOTS}" "${BACKUPS}" "${SYNC_HOME}" "${AGENT_HOME}"; do
    case "${p}" in
        "${vol}"/*) : ;;
        *) bad "${p} is not on ${vol} — it is outside the encrypted volume" ;;
    esac
done

# ---------------------------------------------------------------------------

head_ "Exposure"

# The credential directories hold a live Anthropic OAuth token and the Obsidian
# Sync login. 0700 protects them from other accounts on the box; an SMB share
# pointing at one hands them out over the network regardless. They should be
# plain directories inside a volume, never QNAP shares.
smb_conf=/etc/config/smb.conf
if [ -r "${smb_conf}" ]; then
    exposed=0
    for p in "${SYNC_HOME}" "${AGENT_HOME}" "${SNAPSHOTS}"; do
        if grep -qF "path = ${p}" "${smb_conf}" 2>/dev/null; then
            bad "${p} is exported as an SMB share — remove it in the QNAP UI"
            exposed=1
        fi
    done
    [ "${exposed}" = "0" ] && ok "no SMB share points at credentials or snapshots" || true
else
    note "cannot read ${smb_conf}; verify by hand that no share points at home-sync, home-agent or snapshots"
fi

# ---------------------------------------------------------------------------

head_ "Agent policy"

# More than snapshot and stamping hooks: this file is also where Bash, WebFetch
# and WebSearch are denied — the tools that turn an injected note into an
# exfiltration. See ARCHITECTURE.md#trust-boundary. agent.sh refuses to start
# without it, so this check is about the file being RIGHT, not about it existing.
settings="${VAULT}/.claude/settings.json"

# vault-claude now installs this itself on every start, from a copy baked into
# the image (install-settings.sh), so an absent file before the first
# `docker compose up -d` is expected rather than a fault. What is checked here
# is what the container cannot check for you: that the file on the HOST — the
# one that gets committed, backed up and restored — says what this checkout says.
#
# --fix installs from a copy it can actually see: beside this script (a repo
# clone) or in the working directory. It stays useful for a NAS with no clone
# and no image pulled yet.
if [ ! -f "${settings}" ] && [ "${FIX}" = "1" ]; then
    src=""
    for cand in "./vault-claude-settings.json" \
                "$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)/vault-claude-settings.json"; do
        if [ -f "${cand}" ]; then
            src="${cand}"
            break
        fi
    done
    if [ -n "${src}" ]; then
        mkdir -p "${VAULT}/.claude"
        cp "${src}" "${settings}"
        chown -R "${APP_UID}:${APP_GID}" "${VAULT}/.claude"
        did "installed settings.json from ${src}"
    fi
fi

if [ -f "${settings}" ]; then
    ok "vault .claude/settings.json present"

    # Installed-but-stale is invisible otherwise, and this file is a security
    # policy: a deny rule added upstream does nothing until the container that
    # carries it is deployed. --fix does not overwrite silently — the difference
    # may be a deliberately pinned file (VAULT_SETTINGS_MANAGED=0).
    repo_copy=""
    for cand in "./vault-claude-settings.json" \
                "$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)/vault-claude-settings.json"; do
        if [ -f "${cand}" ]; then
            repo_copy="${cand}"
            break
        fi
    done
    if [ -n "${repo_copy}" ]; then
        if cmp -s "${repo_copy}" "${settings}"; then
            ok "settings.json matches ${repo_copy}"
        else
            note "settings.json differs from ${repo_copy} — a pinned file (VAULT_SETTINGS_MANAGED=0), or an image older than this checkout. Deploy to reinstall it, or now: cp ${repo_copy} ${settings}"
        fi
    fi
    if command -v jq >/dev/null 2>&1; then
        if jq -e '.permissions.deny | index("Bash")' "${settings}" >/dev/null 2>&1; then
            ok "Bash is denied"
        else
            bad "${settings} does not deny Bash — see vault-claude-settings.json"
        fi

        # Two rule-syntax mistakes Claude Code accepts SILENTLY, both of which
        # leave a deny rule in place that denies nothing. Found on 2026-08-30 in
        # rules that had been inert since 2026-08-08 — DECISIONS.md#the-snapshots-deny-that-was-not-one.
        #
        # 1. A single leading slash anchors at the settings source, so
        #    Read(/snapshots/**) denies <vault>/snapshots. Absolute needs '//'.
        #    '~/' and './' are the other two legitimate forms; a bare relative
        #    path is legitimate too, hence matching on the leading '(/' only.
        # 2. Path rules are consulted for Read and Edit ONLY. Write(path) is
        #    accepted, never checked, and warned about at startup.
        #
        # Checked on the deny list only. The same mistake in an ALLOW rule fails
        # safe — a rule that matches nothing grants nothing — and flagging those
        # would make this noisy without making the vault safer.
        bad_anchor=$(jq -r '.permissions.deny[]? | select(test("^[A-Za-z]+\\(/[^/]"))' "${settings}" 2>/dev/null)
        if [ -n "${bad_anchor}" ]; then
            bad "${settings} has deny rules whose leading '/' anchors at the vault, not the filesystem root — they deny nothing. Use '//' for an absolute path: ${bad_anchor//$'\n'/, }"
        else
            ok "no deny rule mistakes <vault>-relative for absolute"
        fi

        inert_write=$(jq -r '.permissions.deny[]? | select(test("^(Write|NotebookEdit|MultiEdit|Glob)\\("))' "${settings}" 2>/dev/null)
        if [ -n "${inert_write}" ]; then
            bad "${settings} has path deny rules that are never consulted — only Read(path) and Edit(path) are. Rewrite as Edit(...): ${inert_write//$'\n'/, }"
        else
            ok "every path deny rule is written for Read or Edit"
        fi

        # additionalDirectories is a permissions SUB-key. At the top level it is
        # an unrecognised key that settings.json ignores in silence, and the
        # symptom is a permission prompt per read with nothing in any log to
        # explain it. Shipped that way in this file's first draft; machine-
        # checkable, so checked. Third member of the same family as the two
        # assertions above.
        if jq -e 'has("additionalDirectories")' "${settings}" >/dev/null 2>&1; then
            bad "${settings} has additionalDirectories at the top level, where it is silently ignored — it belongs inside \"permissions\""
        else
            ok "additionalDirectories is not misplaced at the top level"
        fi

        # claude.ai connectors, the fourth member of the same family: a way in
        # or out that no rule in the deny list can see. Claude Code logged into
        # a claude.ai subscription fetches that account's connectors and serves
        # them as MCP tools, so on 2026-09-05 this agent had Gmail loaded while
        # its policy denied Bash and both web tools. mcp__Gmail__send_message is
        # egress; the "no way out" guarantee was false for weeks.
        #
        # Checked as a top-level key. Inside "permissions" it is ignored in
        # silence, exactly like additionalDirectories above, and the symptom is
        # the connectors staying on with nothing in any log.
        if jq -e '.disableClaudeAiConnectors == true' "${settings}" >/dev/null 2>&1; then
            ok "claude.ai connectors are disabled"
        elif jq -e '.permissions.disableClaudeAiConnectors' "${settings}" >/dev/null 2>&1; then
            bad "${settings} has disableClaudeAiConnectors inside \"permissions\", where it is silently ignored — it is a TOP-LEVEL key"
        else
            bad "${settings} does not set disableClaudeAiConnectors: true — the agent serves this account's claude.ai connectors (Gmail, Drive, Notion), and Gmail alone defeats the no-egress guarantee. No permission rule covers them."
        fi
    fi
else
    note "${settings} missing — vault-claude installs it on start, and refuses to start without one. Expected before the first 'docker compose up -d'; if the stack is already running, the container is failing: docker compose logs vault-claude | grep -i settings"
fi

# The move tool's registration, installed by the same script under the same
# VAULT_SETTINGS_MANAGED flag. Checked here for the same reason as the policy
# above: the container cannot tell you whether the copy on the HOST — the one
# committed, bundled and restored — matches this checkout.
#
# Every finding here is a note, never a bad: without this file the agent has no
# move_file and cannot file or rename a note or an attachment, which is a less
# capable agent, not
# an unsafe one. The asymmetry with the policy above is deliberate and agent.sh
# makes the same distinction.
mcp_json="${VAULT}/.mcp.json"

repo_mcp=""
for cand in "./vault-claude-mcp.json" \
            "$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)/vault-claude-mcp.json"; do
    if [ -f "${cand}" ]; then
        repo_mcp="${cand}"
        break
    fi
done

if [ ! -f "${mcp_json}" ] && [ "${FIX}" = "1" ] && [ -n "${repo_mcp}" ]; then
    cp "${repo_mcp}" "${mcp_json}"
    chown "${APP_UID}:${APP_GID}" "${mcp_json}"
    did "installed .mcp.json from ${repo_mcp}"
fi

if [ -f "${mcp_json}" ]; then
    ok "vault .mcp.json present"
    if [ -n "${repo_mcp}" ]; then
        if cmp -s "${repo_mcp}" "${mcp_json}"; then
            ok ".mcp.json matches ${repo_mcp}"
        else
            note ".mcp.json differs from ${repo_mcp} — a pinned file (VAULT_SETTINGS_MANAGED=0), or an image older than this checkout. Deploy to reinstall it, or now: cp ${repo_mcp} ${mcp_json}"
        fi
    fi
    # The allow rule and the server name are one setting split across two files.
    # Either half alone gives an agent that cannot move notes, and nothing else
    # reports it: Claude Code just does not offer the tool.
    if command -v jq >/dev/null 2>&1 && [ -f "${settings}" ]; then
        if jq -e '.mcpServers["vault-tools"]' "${mcp_json}" >/dev/null 2>&1 \
           && jq -e '.permissions.allow | index("mcp__vault-tools__move_file")' "${settings}" >/dev/null 2>&1 \
           && jq -e '.enabledMcpjsonServers | index("vault-tools")' "${settings}" >/dev/null 2>&1; then
            ok "move_file is registered and allowed"
        else
            note "the move tool is half-configured: .mcp.json must define the 'vault-tools' server, and settings.json must both allow mcp__vault-tools__move_file and list vault-tools in enabledMcpjsonServers"
        fi
    fi
else
    note "${mcp_json} missing — vault-claude installs it on start and warns without it. The agent can read, write and edit notes but cannot move or rename them, and cannot touch attachments at all: see DECISIONS.md#giving-the-agent-a-move"
fi

# The vault agent's standing instructions, managed by this repo since 2026-09-05.
# A note rather than a failure: without it the agent still reads and writes the
# vault, and what it loses is knowing about the jobs handoff — which surfaces as
# an agent that never writes a brief, with nothing anywhere explaining why.
#
# AGENTS.md is deliberately NOT checked. It is the operator's own note, nothing
# in this repo installs it, and reporting on its contents would suggest otherwise.
vault_claude_md="${VAULT}/CLAUDE.md"
if [ -f "${vault_claude_md}" ]; then
    ok "vault CLAUDE.md present"
else
    note "${vault_claude_md} missing — vault-claude installs it on start. Until it is there the agent has not been told about /scratch/jobs, and research briefs go back to being pasted by hand"
fi

# ---------------------------------------------------------------------------

head_ "Research surface"

# vault-research is the agent WITH the web tools and WITHOUT the vault. Its
# safety is not a deny list, it is the absence of a mount, so the checks here
# are about paths and separation rather than about file contents.
# See DECISIONS.md#a-third-surface-for-research.
if [ -z "${SCRATCH}" ] && [ -z "${RESEARCH_HOME}" ]; then
    note "the research surface is not configured (no SCRATCH_HOST_PATH or RESEARCH_HOME_HOST_PATH in ${ENV_FILE}) — vault-research and vault-research-sweep will not start"
else
    for v in SCRATCH RESEARCH_HOME; do
        if [ -z "${!v}" ]; then
            bad "${v}_HOST_PATH is empty while the other half of the research surface is set — vault-research cannot start"
        fi
    done

    # THE invariant of this surface. The scratch volume inside the vault would
    # mean the web-enabled agent can read your notes, and would point the only
    # deleting script in this repo at a synced tree. Both directions are checked:
    # a vault inside scratch is just as wrong.
    if [ -n "${SCRATCH}" ]; then
        case "${SCRATCH}" in
            "${VAULT}"|"${VAULT}"/*)
                bad "SCRATCH_HOST_PATH (${SCRATCH}) is inside the vault. vault-research has WebSearch and WebFetch: with the vault reachable it becomes one session holding your notes, untrusted web content and a way out. scratch-sweep.sh would also be deleting inside a synced tree. Move it outside ${VAULT}."
                ;;
            *)
                case "${VAULT}" in
                    "${SCRATCH}"/*)
                        bad "VAULT_HOST_PATH (${VAULT}) is inside SCRATCH_HOST_PATH (${SCRATCH}) — same problem in the other direction."
                        ;;
                    *) ok "scratch volume is outside the vault" ;;
                esac
                ;;
        esac
        case "${SCRATCH}" in
            "${SNAPSHOTS}"|"${SNAPSHOTS}"/*|"${BACKUPS}"|"${BACKUPS}"/*)
                bad "SCRATCH_HOST_PATH (${SCRATCH}) is inside the snapshot or backup tree — scratch-sweep.sh deletes there on a schedule"
                ;;
        esac
        if [ -d "${SCRATCH}/.git" ] || [ -d "${SCRATCH}/.obsidian" ]; then
            bad "${SCRATCH} contains .git or .obsidian — scratch-sweep.sh will refuse to run against it, and it should not be a repo or a vault"
        fi

        # jobs/ is the one place on this volume vault-claude can write, and the
        # claim that the handoff is safe rests on no STANDING INSTRUCTIONS
        # living there: CLAUDE.md, AGENTS.md and JOBS.md are read by an agent as
        # rules to follow, and one inside jobs/ would be rules the vault agent
        # could write for the agent that holds WebSearch and WebFetch. The
        # contract is installed at the scratch ROOT precisely because that is
        # read-only to vault-claude at the kernel.
        #
        # A find rather than three tests, because depth is the point — a
        # CLAUDE.md two directories down is read for work in that directory.
        # -quit on the first hit: this should be empty, and the message names
        # what to remove rather than listing every match.
        if [ -d "${SCRATCH}/jobs" ]; then
            stray="$(find "${SCRATCH}/jobs" -type f \
                \( -name 'CLAUDE.md' -o -name 'AGENTS.md' -o -name 'JOBS.md' \) \
                -print -quit 2>/dev/null || true)"
            if [ -n "${stray}" ]; then
                bad "${stray} is a standing-instructions file inside the one directory vault-claude can write to. Whatever it says would be obeyed by vault-research, which has WebSearch and WebFetch. Remove it; the jobs contract belongs at ${SCRATCH}/JOBS.md, which vault-claude mounts read-only."
            else
                ok "no standing-instructions file inside scratch/jobs"
            fi
        fi

        # Installed from the image on every vault-research start. Missing means
        # either the container has not started since this was added or the
        # .dockerignore exception and workflow path filter did not both land —
        # and the symptom is two agents with no shared protocol, which reads as
        # the research agent simply ignoring jobs.
        if [ -n "${SCRATCH}" ] && [ ! -f "${SCRATCH}/JOBS.md" ]; then
            note "${SCRATCH}/JOBS.md is not installed yet — start or restart vault-research. Until it is, neither agent has the jobs handoff contract."
        fi
    fi

    # Two Claude Code instances against one home directory corrupt
    # ~/.claude.json. Sharing the volume does not save a login, it breaks both
    # agents — and the symptom is a login loop, not an error naming this.
    if [ -n "${RESEARCH_HOME}" ] && [ "${RESEARCH_HOME}" = "${AGENT_HOME}" ]; then
        # No backticks in this string: inside double quotes bash runs them, and
        # the first draft of this line silently executed 'claude /login' and
        # printed its output as part of the failure message.
        bad "RESEARCH_HOME_HOST_PATH and AGENT_HOME_HOST_PATH are the same directory (${AGENT_HOME}). Two Claude Code instances against one home corrupt ~/.claude.json — see ARCHITECTURE.md#invariants. Give vault-research its own volume and run 'claude /login' inside it."
    elif [ -n "${RESEARCH_HOME}" ]; then
        ok "research credentials are separate from the vault agent's"
    fi

    # Same permissions posture as the other two credential volumes: this one
    # holds a live Anthropic token for the agent that can reach the open web.
    if [ -d "${RESEARCH_HOME}" ]; then
        mode="$(stat -c '%a' "${RESEARCH_HOME}" 2>/dev/null || echo '')"
        if [ -n "${mode}" ] && [ "${mode}" != "700" ]; then
            if [ "${FIX}" = "1" ]; then
                chmod 700 "${RESEARCH_HOME}" && did "chmod 700 ${RESEARCH_HOME}"
            else
                bad "${RESEARCH_HOME} is ${mode}, expected 700 (--fix)"
            fi
        else
            ok "${RESEARCH_HOME} is 0700"
        fi
    fi

    # The research policy is a security file too, and it is subject to the same
    # two silent syntax mistakes as the vault agent's. Checked against the REPO
    # copy: unlike vault-claude's, this one is installed into a scratch volume
    # this script may not be able to see.
    repo_research=""
    for cand in "./vault-research-settings.json" \
                "$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)/vault-research-settings.json"; do
        if [ -f "${cand}" ]; then
            repo_research="${cand}"
            break
        fi
    done
    if [ -n "${repo_research}" ] && command -v jq >/dev/null 2>&1; then
        bad_anchor=$(jq -r '.permissions.deny[]? | select(test("^[A-Za-z]+\\(/[^/]"))' "${repo_research}" 2>/dev/null)
        inert_write=$(jq -r '.permissions.deny[]? | select(test("^(Write|NotebookEdit|MultiEdit|Glob)\\("))' "${repo_research}" 2>/dev/null)
        if [ -n "${bad_anchor}" ] || [ -n "${inert_write}" ]; then
            bad "${repo_research} has deny rules that deny nothing: ${bad_anchor//$'\n'/, } ${inert_write//$'\n'/, }"
        else
            ok "research deny rules are well-formed"
        fi
        # The whole point of this surface. A web tool missing here means the
        # research agent cannot research, and someone will 'fix' it by adding
        # the vault mount instead.
        if jq -e '.permissions.allow | index("WebSearch")' "${repo_research}" >/dev/null 2>&1 \
           && jq -e '.permissions.allow | index("WebFetch")' "${repo_research}" >/dev/null 2>&1; then
            ok "research agent has the web tools"
        else
            note "${repo_research} does not allow WebSearch and WebFetch — that is the reason this surface exists"
        fi

        # The same connector check, and on this surface it is the more serious
        # of the two. A loaded claude.ai vault connector gives the web-enabled
        # agent read and write access to the notes over HTTPS, needing no mount
        # — so research.sh's mount tripwire, the absent /vault volume and every
        # //vault deny in that file are all looking the wrong way at once.
        if jq -e '.disableClaudeAiConnectors == true' "${repo_research}" >/dev/null 2>&1; then
            ok "research agent's claude.ai connectors are disabled"
        else
            bad "${repo_research} does not set disableClaudeAiConnectors: true — the web-enabled agent serves this account's connectors, including the vault's own. That is private data, untrusted content and egress in one session."
        fi
    fi
fi

# ---------------------------------------------------------------------------

head_ "Backups"

# §11.6: a schedule that stops firing is invisible until you need a restore.
# This is the cheapest useful coverage — assert the newest bundle is recent.
# The live bundle is a single fixed name, replaced in place — its mtime is the
# freshness signal. The stamped copies under hourly/, daily/ and monthly/ are
# corruption insurance, not the current backup, so they are deliberately not
# consulted here: an old stamped copy must never make a stalled schedule look
# healthy. hourly/ is the trap to watch — its name invites treating it as a
# liveness signal, but it retains the last N hours that CHANGED, so on a quiet
# vault its newest entry can be days old while the schedule is perfectly healthy.
#
# Pick by MTIME, not by preferring the encrypted name. If encryption regresses,
# backup.sh writes vault-latest.bundle and leaves the old vault-latest.bundle.age
# untouched beside it — so a name-ordered search reports the stale encrypted file
# and prints "bundles are encrypted" over a plaintext current backup. Found
# 2026-08-11 while recovering a lost .env; every check passed at the time.
enc="${BACKUPS}/vault-latest.bundle.age"
plain="${BACKUPS}/vault-latest.bundle"
newest=""
if [ -f "${enc}" ] && [ -f "${plain}" ]; then
    # Both present is itself wrong: one bundle, replaced in place, is the design.
    note "BOTH ${plain##*/} and ${enc##*/} exist — one is stale. Delete whichever is older once you know which is current."
    if [ "$(stat -c '%Y' "${plain}")" -gt "$(stat -c '%Y' "${enc}")" ]; then
        newest="${plain}"
    else
        newest="${enc}"
    fi
elif [ -f "${enc}" ]; then
    newest="${enc}"
elif [ -f "${plain}" ]; then
    newest="${plain}"
fi
if [ -z "${newest}" ]; then
    note "no bundle yet at ${BACKUPS}/vault-latest.bundle[.age] — run: docker compose --profile manual run --rm backup"
else
    # Two DIFFERENT questions, and conflating them produces false alarms.
    #
    # "Did the job run?" -> the state file, touched on every successful run
    #   including no-op ones. This is what catches a stalled schedule.
    # "When did the vault last change?" -> the bundle's mtime. An old bundle on
    #   an idle vault is correct, not a fault.
    state="${SNAPSHOTS}/.last-bundled-sha"
    if [ -f "${state}" ]; then
        ran_h=$(( ( $(date +%s) - $(stat -c '%Y' "${state}") ) / 3600 ))
        if [ "${ran_h}" -le 48 ]; then
            ok "backup job last ran ${ran_h}h ago"
        else
            bad "backup job has not run for ${ran_h}h — the schedule has stopped firing. Check: docker compose logs vault-cron"
        fi
    else
        note "no ${state} yet — cannot tell when the backup job last ran"
    fi

    age_h=$(( ( $(date +%s) - $(stat -c '%Y' "${newest}") ) / 3600 ))
    ok "bundle is ${age_h}h old ($(basename "${newest}")) — reflects the last vault change, not the last run"

    # An unencrypted bundle is plaintext personal notes on Google Drive. If
    # AGE_RECIPIENT is set, every published bundle must be .age.
    recipient="$(env_get AGE_RECIPIENT)"
    case "${newest}" in
        *.age)
            # An encrypted bundle proves the recipient was set WHEN IT WAS
            # WRITTEN, not that it still is. A .env rebuilt without it looks
            # perfect until the next recreate picks up the blank value and the
            # backup after that ships plaintext to Drive.
            if [ -n "${recipient}" ]; then
                ok "bundles are encrypted"
            else
                bad "newest bundle is encrypted but AGE_RECIPIENT is now EMPTY — the next backup after a recreate will write plaintext to Drive"
            fi
            ;;
        *)
            if [ -n "${recipient}" ]; then
                bad "AGE_RECIPIENT is set but $(basename "${newest}") is NOT encrypted — it predates the setting, or encryption is failing"
            else
                note "AGE_RECIPIENT is empty — bundles are plaintext, and Hybrid Backup Sync uploads them to Drive that way"
            fi
            ;;
    esac
fi

# ---------------------------------------------------------------------------

printf '\n'
if [ "${fail}" -gt 0 ]; then
    printf '\033[31m%d problem(s)\033[0m, %d warning(s).\n' "${fail}" "${warn}"
    [ "${FIX}" = "1" ] || printf 'Some of these are repairable: re-run with --fix\n'
    exit 1
fi
printf '\033[32mAll checks passed\033[0m (%d warning(s)).\n' "${warn}"
exit 0
