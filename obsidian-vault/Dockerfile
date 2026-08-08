# syntax=docker/dockerfile:1
#
# One image, three roles: vault-sync, vault-claude, vault-backup.
# See INITIAL_PLAN.md §2.2 and §4.1.
#
# PINNED (INITIAL_PLAN.md §4). Verified by a real build on 2026-08-08.
# To refresh deliberately:
#   docker buildx imagetools inspect node:22-bookworm-slim   # -> digest
#   npm view @anthropic-ai/claude-code version
#   npm view obsidian-headless version
ARG NODE_IMAGE=node:22-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

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
# wrong later is worse than one that is wrong now. See INITIAL_PLAN.md §10.2.
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

COPY --chmod=0755 scripts/ /usr/local/lib/vault/

# Deliberately NOT setting GIT_DIR/GIT_WORK_TREE globally: that would make every
# git invocation in the container — including the agent's own — target the
# snapshot repo. The scripts export them locally instead.
ENV VAULT_DIR=/vault \
    SNAPSHOT_DIR=/snapshots \
    BACKUP_DIR=/backups \
    PATH=/usr/local/lib/vault:$PATH

# VERIFIED 2026-08-08 against code.claude.com/docs/en/settings. An autoupdate
# inside a container would defeat the pinning above and change the agent without
# a rebuild. Also set in vault-claude-settings.json `env`, which is the
# documented path — this ENV is the belt to that braces.
ENV DISABLE_AUTOUPDATER=1

RUN mkdir -p /vault /snapshots /backups \
 && chown "${APP_UID}:${APP_GID}" /vault /snapshots /backups

USER app
WORKDIR /vault

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/lib/vault/sync.sh"]
