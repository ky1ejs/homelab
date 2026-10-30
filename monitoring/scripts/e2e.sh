#!/usr/bin/env bash
#
# End-to-end check of the monitoring stack against the PINNED upstream images,
# run by .github/workflows/e2e-monitoring.yml and runnable on a Docker host that
# has no `monitoring` compose project at all:
#
#   bash monitoring/scripts/e2e.sh
#
# Why this exists: README.md's trust-boundary tables were checked by hand,
# once, against a running stack. This makes them checks that run on every
# change -- so a Beszel or socket-proxy upgrade that changes what is refused,
# or breaks `homelab setup monitoring`, fails here instead of on the NAS.
#
# What it covers, in order: setup's refusals (no explicit port, the dashboard's
# 443, a port already serving something else, no password) and that none of
# them reach Serve; a first run, with Serve only after the claim; a second run
# that must change nothing; the agent connecting; the identity-header and
# socket-proxy tables; the reporter; and a restore from an empty hub.
#
# `tailscale` is a fake on PATH that records calls, so Serve's ordering is
# checked without a tailnet.
#
# It runs in a COPY of the repo in a temp directory, with its own .env and data
# directory, so it never touches a real monitoring/.env. It refuses to start if
# a `monitoring` compose project exists on this host in ANY state, or if port
# 8091 is taken: bin/homelab pins that project name and the containers have
# fixed names, so a stopped real stack would be recreated and then deleted by
# this script's cleanup.
#
# Needs: docker with compose, curl, python3, and permission to chown a
# directory to 1002:100 (root, or passwordless sudo, as on a CI runner).

set -euo pipefail

SRC="$(cd "$(dirname "$0")/../.." && pwd)"
# Pinned like every other image here. Only ever run with --network none or on
# the stack's own network, against the stack's own sockets.
CURL_IMAGE="curlimages/curl:8.16.0@sha256:463eaf6072688fe96ac64fa623fe73e1dbe25d8ad6c34404a669ad3ce1f104b6"
OWNER="owner@example.com"
# Deliberately not the owner: the hub's user_settings hook writes the owner's
# address on create, and a test with both equal cannot see that.
NOTIFY="ops@example.com"
PASSWORD="e2e-$(date +%s)-password"

fail=0
pass() { printf '  [ok  ] %s\n' "$*"; }
bad()  { printf '  [FAIL] %s\n' "$*"; fail=$((fail + 1)); }
die()  { printf 'e2e: %s\n' "$*" >&2; exit 1; }
as_root() { if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi; }
has()  { if printf '%s' "$2" | grep -q -- "$3"; then pass "$1: $3"; else bad "$1: no '$3'"; fi; }
hasnt(){ if printf '%s' "$2" | grep -q -- "$3"; then bad "$1: unexpected '$3'"; else pass "$1: no '$3'"; fi; }

if docker ps -a --filter label=com.docker.compose.project=monitoring --format '{{.Names}}' | grep -q .; then
    die "a 'monitoring' compose project exists here (running or stopped) - not touching it"
fi
if docker volume ls --format '{{.Name}}' | grep -q '^monitoring_'; then
    die "monitoring_* volumes exist here - not touching them"
fi
if (exec 3<>/dev/tcp/127.0.0.1/8091) 2>/dev/null; then
    die "something is listening on 127.0.0.1:8091 - not starting a second hub"
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

# The fake tailscale: `serve status` prints a state file, `serve --bg` appends
# to it and to a call log.
mkdir -p "${WORK}/bin"
export FAKE_TS_STATE="${WORK}/ts-state" FAKE_TS_LOG="${WORK}/ts-log"
: > "${FAKE_TS_STATE}"; : > "${FAKE_TS_LOG}"
cat > "${WORK}/bin/tailscale" <<'EOF'
#!/usr/bin/env bash
case "$1 ${2:-}" in
    "serve status") cat "${FAKE_TS_STATE}" ;;
    "serve --bg")   printf '%s\n' "$*" >> "${FAKE_TS_LOG}"
                    printf 'https://e2e.ts.net:%s (tailnet only)\n|-- / proxy %s\n' "$4" "$5" >> "${FAKE_TS_STATE}" ;;
    *)              printf '%s\n' "$*" >> "${FAKE_TS_LOG}" ;;
esac
EOF
chmod +x "${WORK}/bin/tailscale"
export PATH="${WORK}/bin:${PATH}"

printf 'building the reporter image\n'
docker build -q -t homelab-report:e2e "${REPO}/monitoring" >/dev/null

cd "${REPO}/monitoring"
cp .env.example .env
chmod 600 .env
python3 - "${DATA}" "${NOTIFY}" <<'EOF'
import sys
data, notify = sys.argv[1], sys.argv[2]
over = {
    "IMAGE": "homelab-report:e2e",
    "MON_DATA_HOST_PATH": data,
    "MON_APP_URL": "https://e2e.ts.net:8443",
    "REPORT_FROM": "nas@example.com",
    "REPORT_TO": notify,
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

# setup <smtp password> -- the Beszel password is always the same one.
run_setup() {
    printf '%s\n%s\n' "${PASSWORD}" "$1" \
        | SETUP_OWNER_EMAIL="${OWNER}" "${REPO}/bin/homelab" setup monitoring 2>&1
}
set_env() { python3 - "$1" "$2" <<'EOF'
import sys
k, v = sys.argv[1], sys.argv[2]
lines = [f"{k}={v}\n" if l.startswith(k + "=") else l for l in open(".env")]
open(".env", "w").writelines(lines)
EOF
}
refused() {
    local what="$1" want="$2" got
    if got="$(run_setup hub-smtp-e2e)"; then bad "${what}: setup succeeded"; return; fi
    has "${what}" "${got}" "${want}"
}

printf '\nrefusals, before anything changes\n'
set_env MON_APP_URL "https://e2e.ts.net"
refused "no explicit port" "port explicit"
set_env MON_APP_URL "https://e2e.ts.net:443"
refused "the dashboard's 443" "dashboard's"
set_env MON_APP_URL "https://e2e.ts.net:8443"
printf 'https://e2e.ts.net:8443 (tailnet only)\n|-- / proxy http://127.0.0.1:9999\n' > "${FAKE_TS_STATE}"
refused "8443 serving something else" "already served to something else"
: > "${FAKE_TS_STATE}"
if got="$(printf '' | SETUP_OWNER_EMAIL="${OWNER}" "${REPO}/bin/homelab" setup monitoring 2>&1)"; then
    bad "no password: setup succeeded"
else
    has "no password" "${got}" "no password given"
fi
if [ -s "${FAKE_TS_LOG}" ]; then bad "a refused run called tailscale: $(cat "${FAKE_TS_LOG}")"; else pass "no refused run reached Serve"; fi
if docker ps -a --filter label=com.docker.compose.project=monitoring --format '{{.Names}}' | grep -q .; then
    bad "a refused run started containers"
else
    pass "no refused run started a container"
fi

printf '\nsetup, first run\n'
first="$(run_setup hub-smtp-e2e)" || { printf '%s\n' "${first}"; die "first setup run failed"; }
for want in "claim: created" "system: added" "reporter: created" "smtp: hub mail set" \
            "notify: alerts now go to ${NOTIFY}" "set MON_HUB_KEY in .env" "serve: https port 8443"; do
    has "first run" "${first}" "${want}"
done
claim_at="$(printf '%s\n' "${first}" | grep -n 'claim: created' | cut -d: -f1)"
serve_at="$(printf '%s\n' "${first}" | grep -n 'serve: https port' | cut -d: -f1)"
if [ -n "${claim_at}" ] && [ -n "${serve_at}" ] && [ "${claim_at}" -lt "${serve_at}" ]; then
    pass "Serve came after the claim"
else
    bad "Serve was not after the claim (claim line ${claim_at:-?}, serve line ${serve_at:-?})"
fi

printf '\nsetup, second run, SMTP left blank: must change nothing\n'
second="$(run_setup "")" || { printf '%s\n' "${second}"; die "second setup run failed"; }
for want in "claim: hub already has an account" "system: kyles-nas already present" \
            "reporter: reporter@homelab.invalid already works" "notify: alerts already go to ${NOTIFY}" \
            "smtp: no hub password given, mail settings left as they are (enabled)" \
            "alerts: all" ".env already matched the hub"; do
    has "second run" "${second}" "${want}"
done
for unwanted in "created" "added" " set to " "reset" "now go to" "in .env" "serve: https port" "WARNING"; do
    hasnt "second run" "${second}" "${unwanted}"
done

printf '\nthe agent connects over its socket\n'
wait_up() {
    local up=""
    for _ in $(seq 1 60); do
        up="$(curl -sS -H "Tailscale-User-Login: ${OWNER}" http://127.0.0.1:8091/api/collections/systems/records 2>/dev/null \
            | python3 -c 'import json,sys; print(" ".join(s["status"] for s in json.load(sys.stdin)["items"]))' 2>/dev/null)" || up=""
        if [ "${up}" = "up" ]; then break; fi
        sleep 3
    done
    if [ "${up}" = "up" ]; then pass "$1: system kyles-nas is up"; else bad "$1: system status '${up}', want up"; fi
}
wait_up "first install"
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

printf '\nrestore: lose the hub directory, run setup again\n'
old_key="$(grep '^MON_HUB_KEY=' .env | cut -d= -f2-)"
docker compose -p monitoring stop beszel >/dev/null 2>&1
as_root find "${DATA}/hub" -mindepth 1 -delete
restore="$(run_setup hub-smtp-e2e)" || { printf '%s\n' "${restore}"; die "restore setup run failed"; }
for want in "claim: created" "reporter: created" "notify: alerts now go to ${NOTIFY}" "set MON_HUB_KEY in .env" "set REPORT_BESZEL_PASSWORD in .env"; do
    has "restore" "${restore}" "${want}"
done
if [ "$(grep '^MON_HUB_KEY=' .env | cut -d= -f2-)" != "${old_key}" ]; then pass "restore: MON_HUB_KEY replaced"; else bad "restore: MON_HUB_KEY unchanged"; fi
wait_up "after restore"
if report="$(docker compose -p monitoring run --rm --no-deps weekly-report -dry-run 2>&1)" \
        && printf '%s' "${report}" | grep -q '^Subject: \[homelab\] weekly:'; then
    pass "after restore: the reporter's new password works"
else
    printf '%s\n' "${report}"
    bad "after restore: dry run failed"
fi

printf '\n'
if [ "${fail}" -gt 0 ]; then
    printf '%d check(s) failed\n' "${fail}"
    exit 1
fi
printf 'all checks passed\n'
