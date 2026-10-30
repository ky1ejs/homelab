# syntax=docker/dockerfile:1
#
# gmail-mcp: one Gmail account as a remote MCP server, with send-as.
# See README.md.
#
# PINNED by digest, and the same digests as vault-mcp/Dockerfile: this binary
# holds a refresh token that reads and sends your mail, and it is reachable
# from the internet. Refresh both stacks together, deliberately:
#   docker buildx imagetools inspect golang:1.26-bookworm
#   docker buildx imagetools inspect debian:bookworm-slim
ARG GO_IMAGE=golang:1.26-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599
ARG RUNTIME_IMAGE=debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

FROM ${GO_IMAGE} AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

# The tests are the send-as refusal, the audience and subject checks, the
# header-injection guards and the sealed token store. Running them here means
# an image that reaches the NAS cannot have a hole those tests would catch,
# even if someone pushes past a red CI run.
RUN go vet ./... && go test ./...

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/gmail-mcp .

FROM ${RUNTIME_IMAGE}

# Same account as every other stack. See
# obsidian-vault/DECISIONS.md#identity-1002-not-1000.
ARG APP_UID=1002
ARG APP_GID=100

ENV DEBIAN_FRONTEND=noninteractive

# ca-certificates -> every call this server makes is HTTPS to Google or WorkOS
# tini            -> PID 1 signal handling, so compose stop is not a SIGKILL
# No curl: `gmail-mcp -healthcheck` is the container healthcheck. No tzdata:
# nothing here stamps a local time; mail dates are passed through as written.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
 && rm -rf /var/lib/apt/lists/*

RUN if ! getent group "${APP_GID}" >/dev/null; then groupadd -g "${APP_GID}" app; fi \
 && useradd -u "${APP_UID}" -g "${APP_GID}" -m -d /home/app -s /bin/bash app

COPY --from=build /out/gmail-mcp /usr/local/bin/gmail-mcp

RUN gmail-mcp -version

ENV MCP_ADDR=:8080 \
    TOKEN_FILE=/data/google-token.json

# /data holds only the sealed refresh token. The key that opens it is in .env,
# so a copy of this directory alone opens nothing.
RUN mkdir -p /data && chown "${APP_UID}:${APP_GID}" /data && chmod 700 /data

USER app
WORKDIR /data

# 8080 is the MCP listener. 8765 is used only by `gmail-mcp -login`, and only
# when that run publishes it to the NAS's loopback; see README.md#signing-in.
EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/gmail-mcp"]
