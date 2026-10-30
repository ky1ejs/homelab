# syntax=docker/dockerfile:1
#
# homelab-report: the weekly email. See main.go.
#
# Only this image is built here. Beszel's hub and agent, and the socket proxy,
# are upstream images pinned by digest in docker-compose.yml -- this repo builds
# the part that does not exist upstream, not a second copy of the parts that do.
#
# The builder is pinned to the same digest as dashboard/Dockerfile, so one
# `imagetools inspect` refreshes both. Refresh deliberately:
#   docker buildx imagetools inspect golang:1.26-bookworm
ARG GO_IMAGE=golang:1.26-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599

FROM ${GO_IMAGE} AS build
WORKDIR /src

# Stdlib only, so there is no go.sum and nothing to download. Same rule as
# dashboard/, asserted in CI.
COPY go.mod ./
COPY *.go ./

ARG VERSION=dev

# The tests cover the refusals that matter -- a non-readonly account, SMTP
# without TLS, a header with a newline in it -- so an image that skipped them
# does not reach the NAS even past a red CI run.
RUN go vet ./... && go test ./...

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/homelab-report .

# Scratch: the binary makes two kinds of outbound connection (the hub over the
# compose network, SMTP over the internet) and needs nothing from an OS but the
# CA bundle for the second. No shell, so there is nothing to exec into.
#
# No tzdata either: the local zone comes from the host's /etc/localtime,
# mounted read-only by docker-compose.yml, which Go reads directly.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/homelab-report /homelab-report

# Overridden by docker-compose.yml with APP_UID:APP_GID, like every other stack.
# Set here too so the image is not root if run any other way.
USER 65534:65534

ENTRYPOINT ["/homelab-report"]
