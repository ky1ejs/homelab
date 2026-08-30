# syntax=docker/dockerfile:1
#
# vault-mcp: the vault as a remote MCP server for Claude's voice mode.
# See README.md, and obsidian-vault/DECISIONS.md#runtime-node-not-bun-or-go for
# why this one is Go while the agent image is Node.
#
# This is not the only image that builds this source. obsidian-vault/Dockerfile
# builds the same binary into the agent image, where Claude Code runs it as a
# local stdio MCP server for its one missing tool — moving a note. Keep the two
# Go build stages in step: same base image digest, same `go vet && go test`
# before the build. See README.md#two-surfaces-one-binary.
#
# PINNED by digest, same rule as the agent image: this binary has write access to
# every note you own and is reachable from the internet. Refresh deliberately:
#   docker buildx imagetools inspect golang:1.26-bookworm
#   docker buildx imagetools inspect debian:bookworm-slim
ARG GO_IMAGE=golang:1.26-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599
ARG RUNTIME_IMAGE=debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

FROM ${GO_IMAGE} AS build
WORKDIR /src

# Dependencies first so a source-only change does not re-download the module
# cache. go.sum is committed, so `go mod download` is reproducible.
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

# The tests are the deny list, the traversal guards and the atomic-write check.
# Running them here means an image that reaches the NAS cannot have a hole those
# tests would have caught, even if someone pushes past a red CI run.
RUN go vet ./... && go test ./...

# CGO off: a static binary that does not care what libc the runtime image ships.
# -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/vault-mcp .

FROM ${RUNTIME_IMAGE}

# Identical to the agent image, and for the same reason: a mismatch produces
# writes that appear to work while the Mac sees unreadable files. 1002 is a real
# QNAP account. See obsidian-vault/DECISIONS.md#identity-1002-not-1000.
ARG APP_UID=1002
ARG APP_GID=100

ENV DEBIAN_FRONTEND=noninteractive

# git    -> snapshot commits into /snapshots/vault.git (snapshot.go)
# tini   -> correct signal handling for PID 1, so compose stop is not a SIGKILL
# tzdata -> capture_note stamps a local wall-clock time. Without this the
#           container is UTC, and a thought captured at 9pm on the 9th is
#           written as "2026-08-10 01:48" — a note filed under tomorrow.
# No curl: `vault-mcp -healthcheck` is the container healthcheck instead.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      git \
      tini \
      tzdata \
 && rm -rf /var/lib/apt/lists/*

RUN if ! getent group "${APP_GID}" >/dev/null; then groupadd -g "${APP_GID}" app; fi \
 && useradd -u "${APP_UID}" -g "${APP_GID}" -m -d /home/app -s /bin/bash app

COPY --from=build /out/vault-mcp /usr/local/bin/vault-mcp

# Assert in the build, not at first run — a failing RUN is much cheaper to debug
# than a container that starts and silently does nothing.
RUN vault-mcp -version && git --version

# git refuses to operate on a repository owned by another uid. /snapshots is
# owned by APP_UID on the NAS so this should never fire, but without it a
# uid mismatch surfaces as an opaque "dubious ownership" error on every commit
# rather than as something a preflight check would notice.
RUN git config --system --add safe.directory /snapshots/vault.git

ENV VAULT_DIR=/vault \
    SNAPSHOT_DIR=/snapshots \
    MCP_ADDR=:8080

RUN mkdir -p /vault /snapshots \
 && chown "${APP_UID}:${APP_GID}" /vault /snapshots

USER app
WORKDIR /vault

EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/vault-mcp"]
