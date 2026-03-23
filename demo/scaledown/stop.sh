#!/usr/bin/env bash
# stop.sh — Stops the AIP demo stack started by start.sh
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"
NAMESPACE="${NAMESPACE:-default}"

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
echo "Cleaning up demo resources..."
kubectl delete -f "${DEMO_DIR}/k8s/payment-api.yaml" --namespace "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 && echo "  ✓ payment-api deleted" || true
kubectl delete agentrequests --all -n "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 && echo "  ✓ AgentRequests deleted" || true

echo ""
