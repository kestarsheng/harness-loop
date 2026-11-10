# Desktop container for Computer Use: Xvfb + XFCE + xdotool + scrot, with a Go
# sidecar that exposes the desktop primitives over HTTP for mcp-service.
#
# NOTE (per plan): Xvfb permissions and screenshot stability in a headless
# container must be verified at deploy time. The package list is the standard
# debian bookworm set; confirm the build succeeds on the target host.
FROM golang:1.22-bookworm AS builder
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /desktop-sidecar ./services/desktop/cmd

FROM debian:bookworm-slim

RUN --mount=type=cache,id=agent-platform-apt,target=/var/cache/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
    && apt-get -o Acquire::http::Pipeline-Depth=0 -o Acquire::Retries=10 update \
    && apt-get -o Acquire::http::Pipeline-Depth=0 -o Acquire::Retries=10 install -y --no-install-recommends \
    xvfb \
    xauth \
    xdotool \
    scrot \
    xfce4 \
    xfce4-terminal \
    fonts-noto-cjk \
    dumb-init \
    procps \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV DISPLAY=:99
EXPOSE 9100

COPY --from=builder /desktop-sidecar /usr/local/bin/desktop-sidecar
COPY docker/desktop/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/usr/bin/dumb-init", "--", "/entrypoint.sh"]
