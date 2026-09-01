#!/usr/bin/env bash
#
# preflight.sh — assert the dashboard is set up the way its authentication assumes.
#
#   ./preflight.sh          # check only, change nothing
#
# Run it on the NAS, from the directory holding docker-compose.yml and .env.
# Safe to run repeatedly — that is the point.
#
# THIS SCRIPT EXISTS BECAUSE THIS STACK'S SECURITY IS A PREMISE RATHER THAN A
# SECRET. There is no DASH_TOKEN any more: who you are comes from the
# Tailscale-User-Login header that `tailscale serve` adds, and that header is a
# credential only while Serve is the sole route to the listener. Every check
# below is one clause of that premise.
#
# It is NOT the only enforcement, and must not be treated as one. The agent asks
# the daemon the same question on every mutating action and refuses them all if
# the answer is wrong (agent.go's exposure()), so a stack published wrongly
# breaks rather than opens. What this adds is telling you BEFORE you find out by
# pressing a button — and catching the parts the agent cannot see, like whether
# `tailscale serve` is running at all.
#
# There is deliberately no --fix, unlike obsidian-vault's. Everything wrong here
# is either a line in a file under version control or a `tailscale serve`
# invocation, and a script that silently rewrites the thing that decides who may
# deploy is a worse idea than a script that tells you to.
#
# Exit codes: 0 all good, 1 problems remain, 2 could not run the checks.

set -euo pipefail

cd "$(dirname "$0")/.."

ENV_FILE="${ENV_FILE:-./.env}"

fail=0
warn=0

ok()   { printf '  \033[32mok\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; fail=$((fail + 1)); }
note() { printf '  \033[33mwarn\033[0m  %s\n' "$*"; warn=$((warn + 1)); }
head_() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# Parse one key out of .env. Deliberately NOT `. .env`, the same trap
# obsidian-vault/scripts/deploy.sh documents at length: values with spaces are
# read as a command, and `set -e` turns that into an aborted run. bin/homelab and
# both other scripts parse by key for this reason; the four must stay in step.
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

docker_bin() {
    if command -v docker >/dev/null 2>&1; then
        printf 'docker'
        return 0
    fi
    # The PATH trap the root README documents: Container Station's docker is not
    # on a non-interactive SSH session's PATH.
    local cs
    cs="$(getcfg container-station Install_Path -f /etc/config/qpkg.conf 2>/dev/null)" || cs=""
    if [ -n "${cs}" ] && [ -x "${cs}/bin/docker" ]; then
        printf '%s/bin/docker' "${cs}"
        return 0
    fi
    return 1
}

# ---------------------------------------------------------------------------

head_ "Config"

if [ ! -f "${ENV_FILE}" ]; then
    bad "no ${ENV_FILE} — copy .env.example and fill it in"
    printf '\n%d problem(s).\n' "${fail}"
    exit 2
fi
ok ".env is present"

mode="$(stat -c '%a' "${ENV_FILE}" 2>/dev/null || stat -f '%Lp' "${ENV_FILE}" 2>/dev/null || printf '')"
if [ "${mode}" = "600" ]; then
    ok ".env is 0600"
else
    note ".env is 0${mode:-???}, want 0600 — it holds DASH_AGENT_TOKEN"
fi

if [ -n "$(env_get DASH_TOKEN)" ]; then
    # Not merely unused: someone who set it believes it is protecting something.
    note "DASH_TOKEN is set and is no longer read by anything. Identity comes from
        the tailnet now; delete the line so it cannot be mistaken for a control."
fi

repo="$(env_get REPO_HOST_PATH)"
if [ -z "${repo}" ]; then
    bad "REPO_HOST_PATH is not set"
elif [ ! -d "${repo}/.git" ]; then
    bad "REPO_HOST_PATH=${repo} is not a git checkout — the page reads .git for its version line"
else
    ok "REPO_HOST_PATH points at a checkout"
fi

if [ -z "$(env_get DASH_AGENT_TOKEN)" ]; then
    bad "DASH_AGENT_TOKEN is empty — the agent refuses to start without one"
else
    ok "DASH_AGENT_TOKEN is set"
fi

head_ "Who may press the buttons"

allowed="$(env_get DASH_ALLOWED_LOGINS)"
any="$(env_get DASH_ALLOW_ANY_TAILNET_USER)"
readonly_mode="$(env_get DASH_READ_ONLY)"

case "${any}" in 1|true|yes|TRUE|YES) any=1 ;; *) any=0 ;; esac
case "${readonly_mode}" in 1|true|yes|TRUE|YES) readonly_mode=1 ;; *) readonly_mode=0 ;; esac

if [ "${readonly_mode}" = "1" ]; then
    ok "DASH_READ_ONLY is set: no action will be accepted, and that is a supported mode"
elif [ "${any}" = "1" ]; then
    note "DASH_ALLOW_ANY_TAILNET_USER is set: every account on this tailnet may deploy.
        Reasonable for a tailnet with one human on it, and not the moment you share a node."
elif [ -z "${allowed}" ]; then
    # The web role refuses to start on this, so it is a hard failure rather than
    # a warning: the container will be in a restart loop.
    bad "DASH_ALLOWED_LOGINS is empty and neither override is set — the web container will not start.
        This is deliberate: an empty value must never be the permissive one."
else
    ok "DASH_ALLOWED_LOGINS names $(printf '%s' "${allowed}" | tr ',' '\n' | grep -c .) login(s)"
fi

head_ "How it is published"

port="$(env_get DASH_PORT)"
port="${port:-8088}"

# THE CHECK THIS SCRIPT IS FOR. A non-loopback publish means anything that can
# reach the port can send an identity header of its choosing.
if grep -qE '^[[:space:]]*-[[:space:]]*"127\.0\.0\.1:\$\{DASH_PORT' docker-compose.yml; then
    ok "docker-compose.yml binds the web listener to 127.0.0.1"
else
    bad "docker-compose.yml does not bind the web listener to 127.0.0.1.
        Identity is a header \`tailscale serve\` adds; a listener the LAN can reach
        is a deploy button the LAN can press. The agent will refuse every mutating
        action until this is fixed."
fi

if docker="$(docker_bin)"; then
    published="$("${docker}" ps --filter 'label=com.docker.compose.project=dashboard' \
        --format '{{.Names}} {{.Ports}}' 2>/dev/null || printf '')"
    if [ -z "${published}" ]; then
        note "no dashboard containers are running, so the live binding could not be checked"
    elif printf '%s' "${published}" | grep -qE '0\.0\.0\.0:|:::|\[::\]:'; then
        bad "a running dashboard container publishes beyond loopback:
        ${published}
        Recreate the stack: homelab deploy dashboard"
    else
        ok "the running containers publish on loopback only"
    fi
else
    note "no docker binary found, so the live binding could not be checked"
fi

head_ "Tailscale serve"

if command -v tailscale >/dev/null 2>&1; then
    ts=tailscale
else
    tspath="$(getcfg Tailscale Install_Path -f /etc/config/qpkg.conf 2>/dev/null)" || tspath=""
    if [ -n "${tspath}" ] && [ -x "${tspath}/tailscale" ]; then
        ts="${tspath}/tailscale"
    else
        ts=""
    fi
fi

if [ -z "${ts}" ]; then
    note "tailscale not found on PATH or via qpkg.conf — cannot check the proxy.
        Without it there is no route to the dashboard at all, loopback being loopback."
else
    serve="$("${ts}" serve status 2>/dev/null || printf '')"
    if printf '%s' "${serve}" | grep -q "127.0.0.1:${port}"; then
        ok "tailscale serve is proxying to 127.0.0.1:${port}"
    else
        bad "tailscale serve is not proxying to 127.0.0.1:${port}. Set it up with:
        tailscale serve --bg --https 443 http://127.0.0.1:${port}"
    fi

    # Funnel would put this page on the internet, and the binary refuses
    # funnelled requests for that reason — but a Funnel that exists at all is
    # worth saying out loud rather than leaving to the 403s.
    if printf '%s' "${serve}" | grep -qi 'funnel'; then
        bad "a Funnel is configured on this node. The dashboard refuses funnelled
        requests, but a page listing every container on this host is not the
        second thing in this repo that belongs on the internet.
        Turn it off: tailscale funnel --https 443 off"
    else
        ok "no Funnel on this node"
    fi
fi

# ---------------------------------------------------------------------------

printf '\n'
if [ "${fail}" -gt 0 ]; then
    printf '%d problem(s), %d warning(s).\n' "${fail}" "${warn}"
    exit 1
fi
# `[ ... ] && printf` would return 1 when there are no warnings, and under
# `set -e` that exits 1 from a script that just said everything passed. The
# same trap obsidian-vault/scripts/preflight.sh flags on its --fix line.
if [ "${warn}" -gt 0 ]; then
    printf 'All checks passed, %d warning(s).\n' "${warn}"
else
    printf 'All checks passed.\n'
fi
exit 0
