#!/usr/bin/env bash
# Boots entity-avalonia inside Xvfb (virtual framebuffer) so the real
# X11 + Skia paint path runs without needing a display. Captures a
# screenshot before the app exits and reports its exit code.
#
# Why: headless Avalonia tests don't exercise the same dispatcher /
# layout depth as a real X11 message loop. This wrapper closes that
# gap as a smoke test — boots the app, lets it render its main
# window + initial paint, captures the frame, kills it. Any X11-
# specific paint/layout bug surfaces here, even when headless tests
# pass.
#
# Usage:
#   ./run-xvfb-smoke.sh [seconds]
#
# Args:
#   seconds — how long to let the app run before closing (default 15).
#
# Env overrides:
#   WB_SMOKE_SCREEN — Xvfb geometry (default 1280x1024x24)
#   WB_SMOKE_OUT    — output dir for screenshot + run.log
#                     (default ./xvfb-smoke-out)
#
# Exit codes:
#   0  — app booted, rendered, exited 0 inside the timer window
#   1  — Xvfb failed to start
#   2  — app crashed (non-zero exit before timer)
#   3  — screenshot capture failed
#
# Artifacts (in $WB_SMOKE_OUT):
#   run.log       — entity-avalonia's full stderr+stdout
#   screenshot.png — frame grab from Xvfb before app close

set -uo pipefail

SECONDS_TO_RUN="${1:-15}"
SCREEN="${WB_SMOKE_SCREEN:-1280x1024x24}"
OUT_DIR="${WB_SMOKE_OUT:-$(pwd)/xvfb-smoke-out}"
mkdir -p "$OUT_DIR"

DISPLAY_NUM=99
LOG="$OUT_DIR/run.log"
SHOT="$OUT_DIR/screenshot.png"

echo "==> Xvfb smoke run"
echo "    geometry: $SCREEN"
echo "    timer:    ${SECONDS_TO_RUN}s"
echo "    out:      $OUT_DIR"

# Start Xvfb on :99. -nolisten tcp keeps it scoped to local Unix
# sockets; -ac disables host access control (we own the display).
Xvfb ":$DISPLAY_NUM" -screen 0 "$SCREEN" -nolisten tcp -ac > "$OUT_DIR/xvfb.log" 2>&1 &
XVFB_PID=$!
trap 'kill $XVFB_PID 2>/dev/null || true' EXIT

# Wait for Xvfb to bind its Unix socket. The socket appearing under
# /tmp/.X11-unix means the server is listening; we don't need
# xdpyinfo (which isn't packaged in fedora 43's minimal X stack).
export DISPLAY=":$DISPLAY_NUM"
ready=0
for _ in $(seq 1 25); do
    if [ -S "/tmp/.X11-unix/X$DISPLAY_NUM" ]; then
        ready=1
        break
    fi
    sleep 0.2
done
if [ "$ready" -ne 1 ]; then
    echo "ERROR: Xvfb didn't come up. xvfb.log:"
    cat "$OUT_DIR/xvfb.log"
    exit 1
fi
echo "    Xvfb ready on :$DISPLAY_NUM"

# Optional window manager. WB_SMOKE_WM=1 starts openbox on the virtual
# display before the app launches.
#
# **This exists for one reason: minimize is a window-MANAGER operation.**
# Setting WindowState.Minimized asks the WM to iconify; with no WM on the
# display nothing acts on it, and the driver's own counter reports zero
# transitions (correctly — see SmokeDriver.StartWindowCycle). The open
# managed stack overflow fires on minimize on a real desktop, and a bare
# Xvfb cannot reach it by construction. Off by default: every other smoke
# target is a paint test that a WM would only add reparenting noise to.
if [ -n "${WB_SMOKE_WM:-}" ]; then
    if command -v openbox >/dev/null 2>&1; then
        openbox > "$OUT_DIR/wm.log" 2>&1 &
        WM_PID=$!
        trap 'kill $WM_PID 2>/dev/null || true; kill $XVFB_PID 2>/dev/null || true' EXIT
        sleep 1
        echo "    window manager: openbox (pid $WM_PID) — minimize can now take effect"
    else
        echo "    WARNING: WB_SMOKE_WM set but openbox is not installed; minimize will be a no-op"
    fi
fi

# Hand the timer to the app so it self-closes inside the window.
# We don't kill from outside — that would skip the Closing handler
# (Bridge.Shutdown) and produce dirty exits.
export WB_SMOKE_EXIT_AFTER_SEC="$SECONDS_TO_RUN"
export WB_PANEL_LOG=1
export DOTNET_EnableDiagnostics=1
export DOTNET_DbgEnableMiniDump=1
export DOTNET_DbgMiniDumpType=4
export DOTNET_DbgMiniDumpName="$OUT_DIR/managed.%d.dmp"
export LD_LIBRARY_PATH=.

# WB_SMOKE_INGEST / WB_SMOKE_CYCLE_PATHS / WB_SMOKE_CYCLE_GAP_MS are
# passed through from the caller's environment. The driver inside
# the app reads them. See SmokeDriver.cs.
if [ -n "${WB_SMOKE_INGEST:-}" ]; then
    echo "    smoke driver: WB_SMOKE_INGEST=$WB_SMOKE_INGEST cycles=${WB_SMOKE_CYCLE_PATHS:-50} gap=${WB_SMOKE_CYCLE_GAP_MS:-150}ms"
fi

# Launch the app in the background so we can capture screenshots
# at intervals during the run. The last few survive on disk; if
# the app crashes mid-run they show what was visible at each
# capture point.
./entity-avalonia > "$LOG" 2>&1 &
APP_PID=$!

# Capture a screenshot every ~2s. Keep all frames; small + cheap.
# Numbered by capture-time so they sort chronologically (frame-00.png
# is t≈2s, frame-01.png is t≈4s, etc.).
shots_dir="$OUT_DIR/frames"
mkdir -p "$shots_dir"
frame_n=0
remaining=$SECONDS_TO_RUN
while [ "$remaining" -gt 2 ] && kill -0 "$APP_PID" 2>/dev/null; do
    sleep 2
    remaining=$((remaining - 2))
    if ! kill -0 "$APP_PID" 2>/dev/null; then
        echo "    app exited mid-capture at frame $frame_n"
        break
    fi
    frame_path="$shots_dir/frame-$(printf '%02d' "$frame_n").png"
    if import -display ":$DISPLAY_NUM" -window root "$frame_path" 2>>"$OUT_DIR/screenshot.err"; then
        echo "    frame $frame_n -> $frame_path"
    fi
    frame_n=$((frame_n + 1))
done

# Final screenshot pinned to the well-known name.
if kill -0 "$APP_PID" 2>/dev/null; then
    if import -display ":$DISPLAY_NUM" -window root "$SHOT" 2>>"$OUT_DIR/screenshot.err"; then
        echo "    final screenshot -> $SHOT"
    else
        echo "WARNING: final screenshot capture failed; see $OUT_DIR/screenshot.err"
    fi
fi

# Wait for the app to close (timer should fire any moment now).
wait "$APP_PID"
EXIT_CODE=$?

echo "==> entity-avalonia exited $EXIT_CODE"
if [ "$EXIT_CODE" -ne 0 ]; then
    echo "==> last 20 log lines:"
    tail -20 "$LOG"
    exit 2
fi

echo "==> smoke run complete"
echo "    log:        $LOG"
echo "    screenshot: $SHOT"
exit 0
