#!/bin/bash
set -e

# Start Xvfb (virtual display) so Chromium runs HEADED, not headless. A headed
# browser passes anti-bot checks (real plugins, window.chrome, no "HeadlessChrome"
# UA) that headless trips.
Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp &
sleep 1

# Debian's Chromium ignores --remote-debugging-address and binds DevTools to
# 127.0.0.1 only (unreachable cross-container). So run Chrome's DevTools on
# loopback:9223 and put nginx in front on 0.0.0.0:9222 proxying to 127.0.0.1:9223
# with Host rewritten to localhost (Chrome's DNS-rebinding protection rejects
# non-localhost Host headers). mcp-service connects to chrome:9222 (nginx).
# Use the REAL binary (/usr/lib/chromium/chromium); the /usr/bin/chromium
# wrapper also drops --remote-debugging-address. --disable-blink-features=
# AutomationControlled hides navigator.webdriver.
/usr/lib/chromium/chromium \
  --no-sandbox \
  --no-zygote \
  --no-first-run \
  --disable-gpu \
  --disable-dev-shm-usage \
  --disable-blink-features=AutomationControlled \
  --remote-debugging-port=9223 \
  --remote-allow-origins=* \
  --user-data-dir=/tmp/chrome-data \
  --window-size=1920,1080 \
  --lang=zh-CN \
  --user-agent="Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36" \
  --disable-default-apps \
  --disable-extensions \
  about:blank &
CHROME_PID=$!

# Wait for Chrome's DevTools to come up on 127.0.0.1:9223.
for i in $(seq 1 30); do
  if (echo > /dev/tcp/127.0.0.1/9223) 2>/dev/null; then break; fi
  sleep 0.5
done

# nginx proxies 0.0.0.0:9222 -> 127.0.0.1:9223 (Host rewritten to localhost).
nginx

# Keep the container alive with Chrome as the primary process.
wait $CHROME_PID
