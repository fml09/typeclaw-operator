#!/bin/sh
# Personal Desktop guest session bootstrap (Linux/X11).
#
# The XFCE autostart entry runs this script, so the Guest Desktop Agent
# inherits the session's DISPLAY and keyring instead of running as a system
# service that has no desktop to drive.
#
# The operator substitutes @@SCREEN_WIDTH@@ and @@SCREEN_HEIGHT@@ with
# spec.personalDesktop.screen before writing the script into cloud-init.
set -eu

SCREEN_WIDTH="@@SCREEN_WIDTH@@"
SCREEN_HEIGHT="@@SCREEN_HEIGHT@@"
DESKTOP_AGENT="${DESKTOP_AGENT_PATH:-/usr/local/bin/desktop-agent.py}"

# Best effort: a guest whose display cannot take the requested mode still has
# to reach the agent, and the Gateway reports the real geometry from /health.
# xrandr and xset ship in x11-xserver-utils, which the XFCE metapackage only
# recommends, so their absence is reported instead of being swallowed: a
# desktop that silently ignores spec.personalDesktop.screen otherwise leaves
# the operator status and the model's frame disagreeing with no clue why.
if command -v xrandr >/dev/null 2>&1; then
    xrandr -s "${SCREEN_WIDTH}x${SCREEN_HEIGHT}" ||
        echo "desktop-agent: xrandr could not select ${SCREEN_WIDTH}x${SCREEN_HEIGHT}" >&2
else
    echo "desktop-agent: xrandr is missing (install x11-xserver-utils);" \
         "the display keeps its default mode" >&2
fi

# An unattended desktop must never blank or lock: a blanked screen returns
# black frames to the model and a locked session refuses every typed action.
if command -v xset >/dev/null 2>&1; then
    xset s off || echo "desktop-agent: xset s off failed" >&2
    xset -dpms || echo "desktop-agent: xset -dpms failed" >&2
    xset s noblank || echo "desktop-agent: xset s noblank failed" >&2
else
    echo "desktop-agent: xset is missing (install x11-xserver-utils);" \
         "the screen may blank and hide the desktop from the model" >&2
fi

exec "$DESKTOP_AGENT"
