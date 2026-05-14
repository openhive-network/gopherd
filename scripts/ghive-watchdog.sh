#!/usr/bin/env bash
# Restart ghive up to MAX_RESTARTS times on crash. Each invocation
# loads from a CLEAN snapshot (configurable below) so a long historical
# catch-up doesn't redo work after every crash.
set -uo pipefail
WORK=${WORK:-/home/drov/projects/gopherd/run}
CONFIG=${CONFIG:-$WORK/config.ini}
SNAP_DIR=${SNAP_DIR:-$WORK/snapshots-clean5/block-5000000}
LOG=${LOG:-$WORK/ghive-p2p.log}
SNAP_OUT=${SNAP_OUT:-$WORK/p2p-snapshots}
GHIVE_BIN=${GHIVE_BIN:-/home/drov/projects/gopherd/bin/ghive}
MAX_RESTARTS=${MAX_RESTARTS:-20}

restarts=0
while (( restarts < MAX_RESTARTS )); do
  echo "[watchdog $(date +%H:%M:%S)] start attempt $((restarts+1))/$MAX_RESTARTS" >&2
  GHIVE_SNAPSHOT_DIR=$SNAP_OUT \
    "$GHIVE_BIN" --config="$CONFIG" --load-snapshot="$SNAP_DIR" >> "$LOG" 2>&1
  ec=$?
  echo "[watchdog $(date +%H:%M:%S)] ghive exited ec=$ec" >> "$LOG"
  echo "[watchdog $(date +%H:%M:%S)] ghive exited ec=$ec" >&2
  restarts=$((restarts+1))
  # Clean shutdown (SIGTERM/SIGINT/zero) → stop.
  if [[ $ec -eq 0 || $ec -eq 143 || $ec -eq 130 ]]; then
    echo "[watchdog] clean exit; stopping" >&2
    exit 0
  fi
  sleep 2
done
echo "[watchdog] reached MAX_RESTARTS=$MAX_RESTARTS; giving up" >&2
