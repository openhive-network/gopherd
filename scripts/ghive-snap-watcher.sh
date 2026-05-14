#!/usr/bin/env bash
# Send SIGUSR1 to ghive every time the head crosses a new BUCKET-size
# milestone (default 1M). Pairs with cmd/ghive's SIGUSR1 handler, which
# writes a directory snapshot to $GHIVE_SNAPSHOT_DIR.
set -uo pipefail
WORK=${WORK:-/home/drov/projects/gopherd/run}
LOG=${LOG:-$WORK/ghive-p2p.log}
BUCKET=${BUCKET:-1000000}
START=${START:-5000000}
last_milestone=$START

while true; do
  pid=$(pgrep -f "bin/ghive --config" | head -1)
  if [[ -z "$pid" ]]; then
    echo "[snap_watcher] ghive gone — exiting"
    exit 0
  fi
  head=$(grep -oE "head=[0-9]+" "$LOG" 2>/dev/null | tail -1 | cut -d= -f2)
  if [[ -n "$head" ]]; then
    next=$((last_milestone + BUCKET))
    if (( head >= next )); then
      kill -SIGUSR1 "$pid" 2>/dev/null && echo "[snap_watcher] SIGUSR1 sent at head=$head (milestone=$next)"
      last_milestone=$next
    fi
  fi
  sleep 5
done
