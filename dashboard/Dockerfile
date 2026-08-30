# syntax=docker/dockerfile:1
#
# homelab-dash: one image, two roles. See main.go for the split and README.md
# for why the web half is not allowed to hold the Docker socket.
#
# The runtime base is the Docker CLI image rather than debian:bookworm-slim like
# vault-mcp, because the agent role's whole job is running `bin/homelab`, which
# runs `docker compose`. Reimplementing deploys against the Engine API would mean
# a second, untested way to deploy these stacks -- see agent.go.
#
# PINNED by digest, same rule as every other image in this repo, and the rule
# matters more here than anywhere else: this is the container that gets the
# daemon socket. Refresh deliberately:
#   docker buildx imagetools inspect golang:1.26-bookworm
#   docker buildx imagetools inspect docker:28.5-cli
ARG GO_IMAGE=golang:1.26-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599
ARG RUNTIME_IMAGE=docker:28.5-cli@sha256:625d9431a9f54c5a2bc90f24f0e1c3d55b1349fd857dd85035f98c2c9acbdd4d

FROM ${GO_IMAGE} AS build
WORKDIR /src

# No third-party modules: this binary is stdlib only, so there is nothing to
# download and go.sum does not exist. That is deliberate for the process holding
# the socket -- see docker.go on not vendoring Docker's client.
COPY go.mod ./
COPY *.go ui.html ./

ARG VERSION=dev

# The tests ARE the trust boundary: the verb allowlist, the argv construction,
# the CSRF requirement and the template escaping. Running them here means an
# image that reaches the NAS cannot have a hole they would have caught, even if
# someone pushes past a red CI run. Same convention as vault-mcp's Dockerfile.
RUN go vet ./... && go test ./...

# CGO off: a static binary that does not care what libc the runtime image ships.
# The runtime base is Alpine (musl) while the builder is Debian (glibc), so this
# is not optional here the way it is elsewhere.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/homelab-dash .

FROM ${RUNTIME_IMAGE}

# Identical to every other stack, and for the same reason: a mismatch produces
# writes that appear to work while the Mac sees unreadable files. 1002 is a real
# QNAP account. See obsidian-vault/DECISIONS.md#identity-1002-not-1000.
#
# The agent role does NOT run as this user -- see docker-compose.yml, which runs
# it as root because the Docker socket is root-owned. The web role does.
ARG APP_UID=1002
ARG APP_GID=100

# bash   -> bin/homelab is #!/usr/bin/env bash, and busybox ash is not enough
#           for it (the QTS compatibility notes in that file assume a real bash)
# tini   -> correct signal handling for PID 1, so `compose stop` is not a SIGKILL
#           partway through a pull
# tzdata -> the page prints local wall-clock times; without this the container is
#           UTC and every timestamp on the dashboard is an hour or more off
# git    -> deliberately ABSENT. bin/homelab's git subcommands borrow a pinned
#           alpine/git container when the host has none, and this container has
#           no business writing to the checkout. See README.md#what-it-cannot-do.
RUN apk add --no-cache bash ca-certificates tini tzdata

RUN if ! getent group "${APP_GID}" >/dev/null; then addgroup -g "${APP_GID}" app; fi \
 && adduser -u "${APP_UID}" -G "$(getent group "${APP_GID}" | cut -d: -f1)" -h /home/app -s /bin/bash -D app

COPY --from=build /out/homelab-dash /usr/local/bin/homelab-dash

# Assert in the build, not at first run. A missing compose plugin surfaces here
# as a failed build rather than as a deploy button that returns "unknown command"
# the first time someone presses it in anger.
RUN homelab-dash -version \
 && docker --version \
 && docker compose version \
 && bash --version

ENV DASH_ADDR=:8080 \
    DASHD_ADDR=:8090 \
    REPO_DIR=/repo

EXPOSE 8080

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/usr/local/bin/homelab-dash", "-role=web"]
