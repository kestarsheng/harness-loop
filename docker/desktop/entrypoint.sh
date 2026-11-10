#!/bin/bash
set -e

# Start the virtual display (1920x1080, 24-bit). xdotool/scrot target $DISPLAY.
Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp &
sleep 1

# Start a lightweight XFCE session so GUI apps have a window manager and panel.
# Logs go to /tmp/xfce.log for debugging.
startxfce4 >/tmp/xfce.log 2>&1 &
sleep 2

# Run the HTTP sidecar (forwards desktop primitives to mcp-service).
exec /usr/local/bin/desktop-sidecar
