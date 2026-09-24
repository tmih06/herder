# syntax=docker/dockerfile:1
#
# Herder controller image (SPEC section 45: Docker deployment).
#
# Herder owns orchestration: tasks, issue integration, scheduling,
# sandboxes, policy, credentials, validation, delivery, durable state.
# Herdr owns the runtime: terminal sessions, agent detection/lifecycle,
# panes, prompting, human attachment. Herder reaches Herdr only through
# its public CLI/socket API — never a fork — and workers never see the
# raw socket.
#
# This container is the control plane only: it holds the Docker socket,
# the Herdr socket, and the GitHub credentials. Sandboxed workers never
# inherit any of them (SPEC sections 28-29, 66).

# --- build -------------------------------------------------------------------
FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# CGO off is safe: modernc.org/sqlite is pure Go.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/herder ./cmd/herder

# --- docker CLI ---------------------------------------------------------------
# Client only: the daemon stays on the host, reached through the mounted
# /var/run/docker.sock. Sandboxes are provisioned by the controller, never
# by workers.
FROM docker:27-cli AS dockercli

# --- gh CLI -------------------------------------------------------------------
# Delivery runs controller-side: clones, pushes, and PR/issue operations go
# through gh so no token enters argv or the sandbox.
FROM debian:bookworm-slim AS ghcli
ARG GH_VERSION=2.101.0
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz" \
       | tar -xz --strip-components=2 -C /usr/local/bin "gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh"

# --- herdr CLI ----------------------------------------------------------------
# Client only: the Herdr server runs on the host; the container reaches it
# through HERDR_SOCKET_PATH. The installer verifies a SHA-256 checksum from
# herdr.dev/latest.json, so this tracks the herdr release channel — rebuild
# the image to pick up a new release.
FROM debian:bookworm-slim AS herdrcli
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && HERDR_INSTALL_DIR=/usr/local/bin sh -c "curl -fsSL https://herdr.dev/install.sh | sh"

# --- final --------------------------------------------------------------------
FROM debian:bookworm-slim
# ca-certificates: TLS to GitHub/herdr.dev. git: provider clones and
# delivery staging run controller-side. openssh-client: every worker is
# a herdr SSH machine — machine add and forwarded calls go through ssh,
# and ssh-keygen mints the controller keypair (issue #19).
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git openssh-client \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1000 --create-home --shell /usr/sbin/nologin herder
COPY --from=build /out/herder /usr/local/bin/herder
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=ghcli /usr/local/bin/gh /usr/local/bin/gh
COPY --from=herdrcli /usr/local/bin/herdr /usr/local/bin/herdr
# GH_TOKEN covers clones AND pushes: gh answers git's credential prompts.
RUN git config --system credential.helper "!gh auth git-credential"
USER herder
ENTRYPOINT ["herder"]
CMD ["daemon"]
