#!/usr/bin/env bash
set -euo pipefail

GATEWAY_URL="${AIP_GATEWAY_URL:-http://localhost:8080}"
DASHBOARD_URL="${AIP_DASHBOARD_URL:-http://localhost:8082}"
NAMESPACE="${NAMESPACE:-default}"
DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "${DEMO_DIR}/../.." && pwd)"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  AIP Demo: Cost Optimizer Blocked by Live Traffic Policy"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# ── Preflight checks ──────────────────────────────────────────────────────────

echo "[ 1/5 ] Checking Kubernetes cluster..."
if ! kubectl cluster-info > /dev/null 2>&1; then
  echo "  ✗ No Kubernetes cluster found. Start KIND:"
  echo "    kind create cluster"
  exit 1
fi
echo "  ✓ Cluster ready"

echo "[ 2/5 ] Checking AIP Gateway at ${GATEWAY_URL}..."
if ! curl -sf "${GATEWAY_URL}/healthz" > /dev/null; then
  echo "  ✗ Gateway not running. Start it:"
  echo "    go run ${ROOT_DIR}/demo/gateway/main.go"
  exit 1
fi
echo "  ✓ Gateway running"

echo "[ 3/5 ] Checking AIP Dashboard at ${DASHBOARD_URL}..."
if ! curl -sf "${DASHBOARD_URL}" > /dev/null 2>&1; then
  echo "  ✗ Dashboard not running. Start it:"
  echo "    go run ${ROOT_DIR}/demo/dashboard/main.go"
  exit 1
fi
echo "  ✓ Dashboard running"

echo "[ 4/5 ] Deploying payment-api (3 replicas) to cluster..."
kubectl apply -f "${DEMO_DIR}/k8s/payment-api.yaml" --namespace "${NAMESPACE}" > /dev/null
echo "  Waiting for pods to be ready..."
kubectl rollout status deployment/payment-api -n "${NAMESPACE}" --timeout=90s
READY=$(kubectl get endpoints payment-api -n "${NAMESPACE}" -o jsonpath='{.subsets[0].addresses}' 2>/dev/null || echo "")
if [[ -n "$READY" ]]; then
  echo "  ✓ payment-api ready — active endpoints detected (live traffic signal)"
else
  echo "  ⚠ payment-api deployed but endpoints not yet active — wait a few seconds"
fi

echo "[ 5/5 ] Applying live-traffic-guard SafetyPolicy..."
kubectl apply -f "${DEMO_DIR}/policies/live-traffic-guard.yaml" --namespace "${NAMESPACE}" > /dev/null
echo "  ✓ live-traffic-guard active"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Dashboard: ${DASHBOARD_URL}"
echo "  When the agent is blocked, open the dashboard and approve."
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
sleep 2

# ── Run agent ─────────────────────────────────────────────────────────────────

go run "${DEMO_DIR}/agent/main.go" \
  --gateway "${GATEWAY_URL}" \
  --dashboard "${DASHBOARD_URL}" \
  --namespace "${NAMESPACE}"

# ── Cleanup prompt ────────────────────────────────────────────────────────────

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Demo complete. To reset:"
echo "  kubectl delete -f ${DEMO_DIR}/k8s/payment-api.yaml"
echo "  kubectl delete -f ${DEMO_DIR}/policies/live-traffic-guard.yaml"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
