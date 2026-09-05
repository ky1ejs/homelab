# syntax=docker/dockerfile:1
#
# One image, four roles: vault-sync, vault-claude, vault-research, vault-backup.
# See ARCHITECTURE.md#services and DECISIONS.md#runtime-node-not-bun-or-go.
#
# THE BUILD CONTEXT IS THE REPOSITORY ROOT, not this directory, so every COPY
# below is prefixed with obsidian-vault/. It has to be: vault-claude's move tool
# is vault-mcp's binary, and a context rooted here could not reach ../vault-mcp.
# See DECISIONS.md#giving-the-agent-a-move. Build it by hand with:
#   docker build -f obsidian-vault/Dockerfile .
#
# PINNED (DECISIONS.md#image-and-packaging). Verified by a real build on 2026-08-08.
# To refresh deliberately:
#   docker buildx imagetools inspect node:22-bookworm-slim   # -> digest
#   npm view @anthropic-ai/claude-code version
#   npm view obsidian-headless version
ARG NODE_IMAGE=node:22-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

# The move tool. Claude Code has no way to move a file and Bash is denied, so
# the agent gets one as a local stdio MCP server — and that server is the SAME
# binary and the SAME Go source vault-mcp serves to voice from. That is the
# point of building it here rather than writing a small mover in Node: the deny
# list in vault-claude-settings.json has exactly one enforcement in code, and a
# rule added there cannot end up honoured on one surface and not the other.
#
# Kept in step with vault-mcp/Dockerfile — same base, same digest, same reason
# for pinning it. Refresh both together:
#   docker buildx imagetools inspect golang:1.26-bookworm
ARG GO_IMAGE=golang:1.26-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599

FROM ${GO_IMAGE} AS vault-tools
WORKDIR /src

# Dependencies first so a source-only change does not re-download the module
# cache. go.sum is committed, so `go mod download` is reproducible.
COPY vault-mcp/go.mod vault-mcp/go.sum ./
RUN go mod download

COPY vault-mcp/*.go ./

# The same tests vault-mcp's own image runs, run again here. They ARE the deny
# list, the traversal guards and the atomic-write check — an image that reaches
# the NAS with a move tool in it cannot have a hole those tests would catch,
# even if someone pushes past a red CI run.
RUN go vet ./... && go test ./...

# CGO off: a static binary that does not care what libc the runtime image ships.
# -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/vault-mcp .

FROM ${NODE_IMAGE}

ARG CLAUDE_CODE_VERSION=2.1.226
ARG OBSIDIAN_HEADLESS_VERSION=0.0.14

# Must match the owner of the vault directories on the NAS, and must be a REAL
# QNAP account rather than a convenient-looking number. Resolved 2026-08-08:
# `id kylejs` -> uid=1002, gid=100(everyone).
#
# 1002, not 1000, on purpose. QNAP allocates uids sequentially from 1000, and on
# this NAS 1000 is currently UNALLOCATED (1001 is another account). Owning
# home-sync and home-agent as 1000 would therefore work fine right up until the
# next account is created — at which point that account owns a 0700 directory
# holding a live Anthropic OAuth token and the Obsidian Sync login, because 0700
# protects against everyone except the owner. A default that silently becomes
# wrong later is worse than one that is wrong now. See ARCHITECTURE.md#volumes.
#
# gid 100 is `everyone`, which every QNAP account is in, so group bits grant
# nothing here. The owner uid is doing all the access control.
#
# Changing these means rebuilding: Docker resolves $HOME from /etc/passwd, so
# overriding `user:` in compose to a uid this image does not know makes $HOME
# resolve to / and both interactive logins silently fail to persist.
ARG APP_UID=1002
ARG APP_GID=100

ENV DEBIAN_FRONTEND=noninteractive

# util-linux -> flock (snapshot serialisation)
# tini       -> correct signal handling for PID 1
# jq         -> hook stdin parsing, no JS runtime in the hot path
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      age \
      ca-certificates \
      git \
      jq \
      ripgrep \
      tini \
      tmux \
      util-linux \
 && rm -rf /var/lib/apt/lists/*

# The official Node image ships a `node` user at 1000:1000, which collides with
# most QNAP vault-share owners. Remove it before creating ours.
RUN if id node >/dev/null 2>&1; then userdel -r node >/dev/null 2>&1 || true; fi \
 && if ! getent group "${APP_GID}" >/dev/null; then groupadd -g "${APP_GID}" app; fi \
 && useradd -u "${APP_UID}" -g "${APP_GID}" -m -d /home/app -s /bin/bash app

# npm's global prefix here is /usr/local, which survives the /home/app volume
# mount at runtime. This is the whole reason we do not use the curl installer.
# The version asserts catch a broken install at build time rather than leaving a
# container that starts and silently does nothing.
RUN npm install -g --no-fund --no-audit \
      "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
      "obsidian-headless@${OBSIDIAN_HEADLESS_VERSION}" \
 && npm cache clean --force \
 && claude --version \
 && command -v ob

# supercronic: cron built for containers — runs in the foreground, logs to
# stdout, needs no root. The schedule then lives in this repo instead of QNAP's
# /etc/config/crontab, which is untracked, invisible to preflight.sh, and
# regenerated by firmware updates.
#
# Pinned by version AND sha256 per §4. `ADD --checksum` verifies before the
# layer is written, so a tampered or truncated download fails the build rather
# than shipping. Refresh deliberately:
#   curl -sL https://api.github.com/repos/aptible/supercronic/releases/latest | jq -r .tag_name
ARG SUPERCRONIC_VERSION=v0.2.48
ARG SUPERCRONIC_SHA256=88c1b66b94c486f972fdd1a4d1f901e3e75ff04f749cddd60c5db573e3a33c6c
ADD --checksum=sha256:${SUPERCRONIC_SHA256} \
    https://github.com/aptible/supercronic/releases/download/${SUPERCRONIC_VERSION}/supercronic-linux-amd64 \
    /usr/local/bin/supercronic
RUN chmod 0755 /usr/local/bin/supercronic \
 && printf '* * * * * true\n' > /tmp/t.cron \
 && supercronic -test /tmp/t.cron \
 && rm /tmp/t.cron

COPY --chmod=0755 obsidian-vault/scripts/ /usr/local/lib/vault/

# The agent's tool policy and hooks travel WITH the scripts they invoke, so the
# two cannot be deployed out of step. install-settings.sh materialises this into
# <vault>/.claude/settings.json on every agent start; before it shipped in the
# image, updating a deny rule or a hook meant remembering a `cp` on the NAS, and
# forgetting it left the repo claiming a policy the running agent did not have.
# See DECISIONS.md#shipping-the-tool-policy-with-the-image.
COPY --chmod=0644 obsidian-vault/vault-claude-settings.json /usr/local/lib/vault/vault-claude-settings.json

# The move tool's registration, installed by the same script into <vault>/.mcp.json
# — which is the only place Claude Code reads project MCP servers from. It ships
# next to the policy on purpose: the policy's allow rule for
# mcp__vault-tools__move_file and this file's server name are one setting split
# across two files, and deploying either without the other gives an agent that
# either cannot move notes or is told it may use a server that does not exist.
COPY --chmod=0644 obsidian-vault/vault-claude-mcp.json /usr/local/lib/vault/vault-claude-mcp.json

# The vault agent's standing instructions, installed into <vault>/CLAUDE.md on
# every start. It ships here for the reason the policy beside it does: a change
# in this repo has to reach the running agent through a deploy and not through
# somebody remembering to paste it.
#
# <vault>/AGENTS.md is deliberately NOT here. That file is the operator's, holds
# the vault's own conventions, and syncs to their phone as an ordinary note —
# installing over it would destroy their writing on every container start.
# Claude Code reads both, so this file carries only what the infrastructure has
# to say for itself. See DECISIONS.md#passing-a-brief-to-the-research-agent.
COPY --chmod=0644 obsidian-vault/vault-CLAUDE.md /usr/local/lib/vault/vault-CLAUDE.md

# The same pair again, for vault-research. Different agent, different policy:
# that one is denied the web and reads the whole vault, this one has the web and
# cannot see the vault at all. research.sh points install-settings.sh at these
# via VAULT_SETTINGS_SOURCE and VAULT_MCP_SOURCE, so one script installs either
# agent's files and there is no second copy of that logic to drift.
# See DECISIONS.md#a-third-surface-for-research.
COPY --chmod=0644 obsidian-vault/vault-research-settings.json /usr/local/lib/vault/vault-research-settings.json
COPY --chmod=0644 obsidian-vault/vault-research-mcp.json /usr/local/lib/vault/vault-research-mcp.json

# The research agent's standing instructions, installed into <scratch>/CLAUDE.md
# on every start. It ships here rather than being authored in the scratch volume
# because the agent's own policy write-denies it: this session reads pages
# written by strangers, and a page that could append to CLAUDE.md would be
# writing instructions for every later session.
COPY --chmod=0644 obsidian-vault/research-CLAUDE.md /usr/local/lib/vault/research-CLAUDE.md

# The jobs contract, installed into <scratch>/JOBS.md on every start. It ships
# here for the reason above and for one more: BOTH agents read it. It is the
# handoff protocol between them, so it has to sit somewhere neither can author —
# the scratch root, which is write-denied to vault-research by policy and
# read-only to vault-claude at the kernel. A protocol file inside jobs/ itself
# would be in the one directory vault-claude can write.
# See DECISIONS.md#passing-a-brief-to-the-research-agent.
COPY --chmod=0644 obsidian-vault/research-JOBS.md /usr/local/lib/vault/research-JOBS.md

# The binary those two files point at. /usr/local/bin, not /usr/local/lib/vault,
# because it is a program the agent runs rather than a script the container's
# own entrypoints call — and the path is spelled out in vault-claude-mcp.json.
COPY --from=vault-tools /out/vault-mcp /usr/local/bin/vault-mcp

# Assert at build time rather than discovering it when the agent starts and
# quietly has no move tool: an MCP server that fails to launch is a line in a log
# nobody reads, not a visible error.
RUN vault-mcp -version

# Deliberately NOT setting GIT_DIR/GIT_WORK_TREE globally: that would make every
# git invocation in the container — including the agent's own — target the
# snapshot repo. The scripts export them locally instead.
ENV VAULT_DIR=/vault \
    SNAPSHOT_DIR=/snapshots \
    BACKUP_DIR=/backups \
    SCRATCH_DIR=/scratch \
    PATH=/usr/local/lib/vault:$PATH

# VERIFIED 2026-08-08 against code.claude.com/docs/en/settings. An autoupdate
# inside a container would defeat the pinning above and change the agent without
# a rebuild. Also set in vault-claude-settings.json `env`, which is the
# documented path — this ENV is the belt to that braces.
ENV DISABLE_AUTOUPDATER=1

RUN mkdir -p /vault /snapshots /backups /scratch \
 && chown "${APP_UID}:${APP_GID}" /vault /snapshots /backups /scratch

USER app
WORKDIR /vault

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/lib/vault/sync.sh"]
