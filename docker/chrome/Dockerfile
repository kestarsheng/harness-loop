# Headed Chromium + Xvfb container.
# Provides a non-headless browser over CDP for sites whose anti-bot requires a
# real headed browser (e.g. Xiaohongshu search: the page's axios interceptor
# only generates the x-s-common signature when the event loop runs normally,
# which Obscura's stealth headless engine freezes).
FROM debian:bookworm-slim

RUN --mount=type=cache,id=agent-platform-apt,target=/var/cache/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
    && apt-get -o Acquire::http::Pipeline-Depth=0 -o Acquire::Retries=10 update \
    && apt-get -o Acquire::http::Pipeline-Depth=0 -o Acquire::Retries=10 install -y --no-install-recommends \
    chromium \
    xvfb \
    xauth \
    dumb-init \
    nginx \
    procps \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV DISPLAY=:99
EXPOSE 9222

COPY nginx.conf /etc/nginx/nginx.conf
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/usr/bin/dumb-init", "--", "/entrypoint.sh"]
