# syntax=docker/dockerfile:1
#
# One image, three roles: brief-agent, brief-voice, brief-publisher. Each
# service in docker-compose.yml runs scripts/cron.sh with its role and is
# given only its own volumes and variables. See README.md#how-it-works.
#
# The build context is this directory. Build it by hand with:
#   docker build daily-brief
#
# PINNED, with the same digests as obsidian-vault/Dockerfile, for the same
# reasons. Refresh deliberately, and both stacks together:
#   docker buildx imagetools inspect golang:1.26-bookworm
#   docker buildx imagetools inspect node:22-bookworm-slim
#   npm view @anthropic-ai/claude-code dist-tags.stable
ARG GO_IMAGE=golang:1.26-bookworm@sha256:6c5605ab3a9a9fb3c4eafe5b3d63cdbf3881caf113262b67862547b54a9db599
ARG NODE_IMAGE=node:22-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

FROM ${GO_IMAGE} AS build
WORKDIR /src

# Standard library only, so there is no go.sum and nothing to download. CI
# fails if go.mod gains a require: this binary holds the Gmail grant.
COPY go.mod ./
COPY *.go ./
COPY testdata/ ./testdata/

# The tests are the issue validator, the sealed token store, the header and
# MIME encoding and the publisher's at-most-once state machine. Running them
# here means an image that reaches the NAS cannot have a hole they would
# catch, even if someone pushes past a red CI run.
RUN go vet ./... && go test ./...

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/daily-brief .

# Spotify's own CLI for uploading to a show you own, pinned by version and
# sha256. The zip also holds agent skills; only the binary is kept. Refresh:
#   gh release view --repo spotify/save-to-spotify --json tagName,assets
ARG SAVE_TO_SPOTIFY_VERSION=v0.2.1
ARG SAVE_TO_SPOTIFY_SHA256=4bd0ebd6a3fa17d6bced32879727f3d67b27de1be6e3a5614384b1537980eb25
ADD --checksum=sha256:${SAVE_TO_SPOTIFY_SHA256} \
    https://github.com/spotify/save-to-spotify/releases/download/${SAVE_TO_SPOTIFY_VERSION}/save-to-spotify-linux-amd64-${SAVE_TO_SPOTIFY_VERSION}.zip \
    /tmp/save-to-spotify.zip
RUN apt-get update \
 && apt-get install -y --no-install-recommends unzip \
 && rm -rf /var/lib/apt/lists/* \
 && unzip -j /tmp/save-to-spotify.zip save-to-spotify -d /out \
 && chmod 0755 /out/save-to-spotify

FROM ${NODE_IMAGE}

# The version npm's stable dist-tag named, as obsidian-vault pins it: this
# agent runs unattended and nobody watches it update.
ARG CLAUDE_CODE_VERSION=2.1.308

# The same account as every other stack. See
# obsidian-vault/DECISIONS.md#identity-1002-not-1000.
ARG APP_UID=1002
ARG APP_GID=100

ENV DEBIAN_FRONTEND=noninteractive

# python3, python3-venv -> the voice job (Debian's 3.11, which the wheels in
#                          voice/requirements.txt are built for)
# jq                     -> write.sh strips the policy's comment
# tini                   -> PID 1 signal handling
# tzdata                 -> TZ for supercronic, date(1) and Python
# util-linux             -> flock
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      jq \
      python3 \
      python3-venv \
      tini \
      tzdata \
      util-linux \
 && rm -rf /var/lib/apt/lists/*

# The official Node image ships a `node` user at 1000:1000. Remove it before
# creating ours, as obsidian-vault does.
RUN if id node >/dev/null 2>&1; then userdel -r node >/dev/null 2>&1 || true; fi \
 && if ! getent group "${APP_GID}" >/dev/null; then groupadd -g "${APP_GID}" app; fi \
 && useradd -u "${APP_UID}" -g "${APP_GID}" -m -d /home/app -s /bin/bash app

RUN npm install -g --no-fund --no-audit "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && npm cache clean --force \
 && claude --version

# The voice job's Python packages: every wheel named and hash-pinned, no
# dependency resolution (--no-deps) and no source builds (--only-binary), so
# the set that installs is exactly the set reviewed. See the comment at the
# top of voice/requirements.txt to refresh it.
COPY voice/requirements.txt /tmp/requirements.txt
RUN python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir --no-deps --require-hashes \
      --only-binary=:all: -r /tmp/requirements.txt \
 && /opt/venv/bin/pip check \
 && rm /tmp/requirements.txt

# Kokoro v1.0 in ONNX form: 82M parameters, Apache-2.0, and fast enough on
# the NAS's CPU. Pinned by sha256 like every download here. ADD writes URLs
# as 0600, hence the chmod.
ARG KOKORO_BASE=https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0
ADD --checksum=sha256:7d5df8ecf7d4b1878015a32686053fd0eebe2bc377234608764cc0ef3636a6c5 \
    ${KOKORO_BASE}/kokoro-v1.0.onnx /opt/kokoro/kokoro-v1.0.onnx
ADD --checksum=sha256:bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d \
    ${KOKORO_BASE}/voices-v1.0.bin /opt/kokoro/voices-v1.0.bin
RUN chmod 0755 /opt/kokoro && chmod 0644 /opt/kokoro/*

# Same supercronic, version and sha256 as obsidian-vault/Dockerfile.
ARG SUPERCRONIC_VERSION=v0.2.48
ARG SUPERCRONIC_SHA256=88c1b66b94c486f972fdd1a4d1f901e3e75ff04f749cddd60c5db573e3a33c6c
ADD --checksum=sha256:${SUPERCRONIC_SHA256} \
    https://github.com/aptible/supercronic/releases/download/${SUPERCRONIC_VERSION}/supercronic-linux-amd64 \
    /usr/local/bin/supercronic
RUN chmod 0755 /usr/local/bin/supercronic \
 && printf '* * * * * true\n' > /tmp/t.cron \
 && supercronic -test /tmp/t.cron \
 && rm /tmp/t.cron

COPY --from=build /out/daily-brief /usr/local/bin/daily-brief
COPY --from=build /out/save-to-spotify /usr/local/bin/save-to-spotify

COPY --chmod=0755 scripts/ /usr/local/lib/daily-brief/
COPY --chmod=0644 agent-instructions.md agent-settings.json /usr/local/lib/daily-brief/
COPY --chmod=0644 voice/voice.py /usr/local/lib/daily-brief/voice/voice.py

# The voice job's unit tests, against the pinned numpy, then one sentence read
# aloud with the real model: a model, a wheel and espeak-ng that do not work
# together fail here rather than on the first morning. The tests are not
# shipped.
COPY voice/test_voice.py /tmp/voice-test/test_voice.py
RUN cp /usr/local/lib/daily-brief/voice/voice.py /tmp/voice-test/ \
 && cd /tmp/voice-test \
 && /opt/venv/bin/python -m unittest -v test_voice \
 && cd / && rm -rf /tmp/voice-test \
 && /opt/venv/bin/python -I /usr/local/lib/daily-brief/voice/voice.py --self-test \
 && daily-brief version \
 && jq -e 'del(._comment) | .permissions' /usr/local/lib/daily-brief/agent-settings.json >/dev/null

# One layout for all three roles. write.sh's tripwire tells a real mount from
# these by device number.
RUN mkdir -p /issues /audio /state /config \
 && chown "${APP_UID}:${APP_GID}" /issues /audio /state

ENV ISSUES_DIR=/issues \
    AUDIO_DIR=/audio \
    STATE_DIR=/state \
    KOKORO_DIR=/opt/kokoro \
    DISABLE_AUTOUPDATER=1 \
    SAVE_TO_SPOTIFY_NO_UPDATE_CHECK=1

USER app
WORKDIR /issues

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/lib/daily-brief/cron.sh", "publisher"]
