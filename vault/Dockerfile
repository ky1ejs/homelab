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

# Must match the owner of the vault share on the NAS. QNAP is commonly
# admin:everyone = 1000:100, NOT 1000:1000. Check with:
#   ls -n /share/<pool>/<share>
ARG APP_UID=1000
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
