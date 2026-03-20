#!/usr/bin/env bash
# stop.sh — Stops the AIP demo stack started by start.sh
set -euo pipefail

echo ""
echo "Stopping AIP demo stack..."

for name in controller gateway dashboard; do
  pidfile="/tmp/aip-${name}.pid"
  if [[ -f "$pidfile" ]]; then
    pid=$(cat "$pidfile")
    if kill "$pid" 2>/dev/null; then
      echo "  ✓ Stopped ${name} (PID ${pid})"
    else
      echo "  - ${name} was not running"
    fi
    rm -f "$pidfile"
  else
    echo "  - ${name} not started by start.sh"
  fi
done

echo ""
