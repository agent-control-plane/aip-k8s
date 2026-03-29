#!/usr/bin/env bash
# start.sh — Starts the full AIP demo stack in background processes.
# Run this ONCE before ./run.sh
#
# Usage:
#   ./demo/scaledown/start.sh          # from repo root
#   ./start.sh                         # from demo/scaledown/
#
# Stops with: ./demo/scaledown/stop.sh (or kill the PIDs in /tmp/aip-demo-*.pid)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

GATEWAY_PORT="${GATEWAY_PORT:-8080}"
DASHBOARD_PORT="${DASHBOARD_PORT:-8082}"

GATEWAY_LOG="/tmp/aip-gateway.log"
DASHBOARD_LOG="/tmp/aip-dashboard.log"
CONTROLLER_LOG="/tmp/aip-controller.log"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  AIP Demo Stack — Starting"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# ── Preflight ──────────────────────────────────────────────────────────────────

echo "[ 1/4 ] Checking Kubernetes cluster..."
if ! kubectl cluster-info > /dev/null 2>&1; then
  echo "  ✗ No cluster found. Start KIND first:"
  echo "    kind create cluster"
  exit 1
fi
echo "  ✓ Cluster ready"

echo "[ 2/4 ] Installing CRDs..."
cd "${ROOT_DIR}"
make install > /dev/null 2>&1
echo "  ✓ CRDs installed"

# ── Kill any stale processes ───────────────────────────────────────────────────

for pidfile in /tmp/aip-controller.pid /tmp/aip-gateway.pid /tmp/aip-dashboard.pid; do
  if [[ -f "$pidfile" ]]; then
    pid=$(cat "$pidfile")
    kill "$pid" 2>/dev/null || true
    rm -f "$pidfile"
  fi
done

# ── Start controller ──────────────────────────────────────────────────────────

echo "[ 3/4 ] Starting AIP controller..."
cd "${ROOT_DIR}"
go run ./cmd/main.go \
  --metrics-bind-address=0 \
  --health-probe-bind-address=:8081 \
  > "${CONTROLLER_LOG}" 2>&1 &
echo $! > /tmp/aip-controller.pid

# Wait for controller health probe
for i in $(seq 1 20); do
  if curl -sf http://localhost:8081/healthz > /dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! curl -sf http://localhost:8081/healthz > /dev/null 2>&1; then
  echo "  ✗ Controller failed to start. Check ${CONTROLLER_LOG}"
  exit 1
fi
echo "  ✓ Controller running (PID $(cat /tmp/aip-controller.pid))"

# ── Start gateway ──────────────────────────────────────────────────────────────

echo "[ 4/4 ] Starting AIP Gateway and Dashboard..."
cd "${ROOT_DIR}"
go run ./cmd/gateway --addr=":${GATEWAY_PORT}" \
  > "${GATEWAY_LOG}" 2>&1 &
echo $! > /tmp/aip-gateway.pid

go run ./cmd/dashboard/main.go --port="${DASHBOARD_PORT}" \
  > "${DASHBOARD_LOG}" 2>&1 &
echo $! > /tmp/aip-dashboard.pid

# Wait for gateway
for i in $(seq 1 20); do
  if curl -sf "http://localhost:${GATEWAY_PORT}/healthz" > /dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! curl -sf "http://localhost:${GATEWAY_PORT}/healthz" > /dev/null 2>&1; then
  echo "  ✗ Gateway failed to start. Check ${GATEWAY_LOG}"
  exit 1
fi

# Wait for dashboard
for i in $(seq 1 20); do
  if curl -sf "http://localhost:${DASHBOARD_PORT}" > /dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! curl -sf "http://localhost:${DASHBOARD_PORT}" > /dev/null 2>&1; then
  echo "  ✗ Dashboard failed to start. Check ${DASHBOARD_LOG}"
  exit 1
fi

echo "  ✓ Gateway   running → http://localhost:${GATEWAY_PORT}"
echo "  ✓ Dashboard running → http://localhost:${DASHBOARD_PORT}"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Stack is ready."
echo ""
echo "  Logs:"
echo "    Controller : ${CONTROLLER_LOG}"
echo "    Gateway    : ${GATEWAY_LOG}"
echo "    Dashboard  : ${DASHBOARD_LOG}"
echo ""
echo "  Next step:"
echo "    ./demo/scaledown/run.sh"
echo ""
echo "  To stop everything:"
echo "    ./demo/scaledown/stop.sh"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
