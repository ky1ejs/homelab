#!/usr/bin/env bash
#
# End-to-end check of the monitoring stack against the PINNED upstream images,
# run by .github/workflows/build-monitoring.yml and runnable on any Docker host:
#
#   bash monitoring/scripts/e2e.sh
#
# Why this exists: README.md's trust-boundary tables were checked by hand,
# once, against a running stack. This makes them checks that run on every
# change -- so a Beszel or socket-proxy upgrade that changes what is refused,
# or breaks `homelab setup monitoring`, fails here instead of on the NAS.
#
# It runs in a COPY of the repo in a temp directory, with its own .env and data
# directory, so it never touches a real monitoring/.env. It refuses to start if
# a compose project called `monitoring` is already running on this host,
# because bin/homelab pins that project name and the two would collide.
#
# Needs: docker with compose, curl, python3, and permission to chown a
# directory to 1002:100 (root, or passwordless sudo, as on a CI runner).

set -euo pipefail

SRC="$(cd "$(dirname "$0")/../.." && pwd)"
# Pinned like every other image here. Only ever run with --network none or on
# the stack's own network, against the stack's own sockets.
CURL_IMAGE="curlimages/curl:8.16.0@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6"
OWNER="owner@example.com"
PASSWORD="e2e-$(date +%s)-password"

fail=0
pass() { printf '  [ok  ] %s\n' "$*"; }
bad()  { printf '  [FAIL] %s\n' "$*"; fail=$((fail + 1)); }
die()  { printf 'e2e: %s\n' "$*" >&2; exit 1; }
as_root() { if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi; }

if docker ps --filter label=com.docker.compose.project=monitoring --format '{{.Names}}' | grep -q .; then
    die "a 'monitoring' compose project is already running here - not touching it"
fi

WORK="$(mktemp -d)"
DATA="${WORK}/data"
REPO="${WORK}/repo"
cleanup() {
    ( cd "${REPO}/monitoring" 2>/dev/null && docker compose -p monitoring down -v >/dev/null 2>&1 ) || true
    as_root rm -rf "${WORK}"
}
trap cleanup EXIT

# The copy: everything bin/homelab and the stack read, nothing of git's.
mkdir -p "${REPO}"
tar -C "${SRC}" --exclude=.git --exclude='*/.env' -cf - . | tar -C "${REPO}" -xf -
mkdir -p "${DATA}"/{hub,agent,socket,fs-probe}
as_root chown -R 1002:100 "${DATA}"

printf 'building the reporter image\n'
docker build -q -t homelab-report:e2e "${REPO}/monitoring" >/dev/null

cd "${REPO}/monitoring"
cp .env.example .env
chmod 600 .env
python3 - "${DATA}" <<'EOF'
import sys
data = sys.argv[1]
over = {
    "IMAGE": "homelab-report:e2e",
    "MON_DATA_HOST_PATH": data,
    "MON_APP_URL": "http://127.0.0.1:8091",
    "REPORT_FROM": "nas@example.com",
    "REPORT_TO": "owner@example.com",
    "SMTP_HOST": "smtp.example.com",
    "SMTP_USERNAME": "nas@example.com",
}
lines = []
for line in open(".env"):
    key = line.split("=", 1)[0]
    lines.append(f"{key}={over.pop(key)}\n" if key in over else line)
lines += [f"{k}={v}\n" for k, v in over.items()]
open(".env", "w").writelines(lines)
EOF

run_setup() {
    printf '%s\nhub-smtp-e2e\n' "${PASSWORD}" \
        | SETUP_OWNER_EMAIL="${OWNER}" "${REPO}/bin/homelab" setup monitoring 2>&1
}

printf '\nsetup, first run\n'
first="$(run_setup)" || { printf '%s\n' "${first}"; die "first setup run failed"; }
for want in "claim: created" "system: added" "reporter: created" "smtp: hub mail set" "set MON_HUB_KEY in .env"; do
    if printf '%s' "${first}" | grep -q "${want}"; then pass "first run: ${want}"; else bad "first run: no '${want}'"; fi
done

printf '\nsetup, second run: must change nothing\n'
second="$(run_setup)" || { printf '%s\n' "${second}"; die "second setup run failed"; }
for want in "claim: hub already has an account" "system: kyles-nas already present" \
            "reporter: reporter@homelab.invalid already works" "alerts: all" ".env already matched the hub"; do
    if printf '%s' "${second}" | grep -q "${want}"; then pass "second run: ${want}"; else bad "second run: no '${want}'"; fi
done

printf '\nthe agent connects over its socket\n'
up=""
for _ in $(seq 1 60); do
    up="$(curl -sS -H "Tailscale-User-Login: ${OWNER}" http://127.0.0.1:8091/api/collections/systems/records 2>/dev/null \
        | python3 -c 'import json,sys; print(" ".join(s["status"] for s in json.load(sys.stdin)["items"]))' 2>/dev/null)" || up=""
    if [ "${up}" = "up" ]; then break; fi
    sleep 3
done
if [ "${up}" = "up" ]; then pass "system kyles-nas is up"; else bad "system status '${up}', want up"; fi
mode="$(docker inspect beszel-agent --format '{{.HostConfig.NetworkMode}}')"
if [ "${mode}" = "none" ]; then pass "agent network mode: none"; else bad "agent network mode: ${mode}"; fi

printf '\nidentity header: README.md#trust-boundary\n'
code() { curl -sS -o /dev/null -w '%{http_code}' "$@"; }
expect() {
    local want="$1" what="$2" got="$3"
    if [ "${got}" = "${want}" ]; then pass "${what}: ${got}"; else bad "${what}: ${got}, want ${want}"; fi
}
expect 200 "header through the loopback publish" \
    "$(code -H "Tailscale-User-Login: ${OWNER}" http://127.0.0.1:8091/api/beszel/info)"
expect 401 "header with an unknown email" \
    "$(code -H "Tailscale-User-Login: nobody@example.com" http://127.0.0.1:8091/api/beszel/info)"
expect 401 "header from a container on monitoring_default" \
    "$(docker run --rm --network monitoring_default "${CURL_IMAGE}" -sS -o /dev/null -w '%{http_code}' \
        -H "Tailscale-User-Login: ${OWNER}" http://beszel:8090/api/beszel/info)"

printf '\nsocket proxy allowlist: README.md#trust-boundary\n'
cid="$(docker inspect beszel --format '{{.Id}}' | cut -c1-12)"
probe() {
    docker run --rm --network none -v monitoring_docker-proxy:/p "${CURL_IMAGE}" \
        -sS -o /dev/null -w '%{http_code}' --path-as-is --unix-socket /p/docker.sock -X "$1" "http://x$2"
}
expect 200 "GET /containers/json"            "$(probe GET /containers/json)"
expect 200 "GET /containers/<id>/stats"      "$(probe GET "/containers/${cid}/stats?stream=0&one-shot=1")"
expect 403 "GET /containers/<id>/json"       "$(probe GET "/containers/${cid}/json")"
expect 403 "GET /containers/<id>/logs"       "$(probe GET "/containers/${cid}/logs?stdout=1")"
expect 403 "GET /v1.47/containers/<id>/json" "$(probe GET "/v1.47/containers/${cid}/json")"
expect 403 "GET /containers/../<id>/json"    "$(probe GET "/containers/../containers/${cid}/json")"
expect 403 "GET /images/json"                "$(probe GET /images/json)"
expect 405 "POST /containers/<id>/restart"   "$(probe POST "/containers/${cid}/restart")"

printf '\nthe reporter, as its readonly account\n'
if report="$(docker compose -p monitoring run --rm --no-deps weekly-report -dry-run 2>&1)" \
        && printf '%s' "${report}" | grep -q '^Subject: \[homelab\] weekly:'; then
    pass "dry run built a report"
else
    printf '%s\n' "${report}"
    bad "dry run failed"
fi

printf '\n'
if [ "${fail}" -gt 0 ]; then
    printf '%d check(s) failed\n' "${fail}"
    exit 1
fi
printf 'all checks passed\n'
