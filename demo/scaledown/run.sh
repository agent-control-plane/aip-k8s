#!/usr/bin/env bash
set -euo pipefail

GATEWAY_URL="${AIP_GATEWAY_URL:-http://localhost:8080}"
DASHBOARD_URL="${AIP_DASHBOARD_URL:-http://localhost:8082}"
NAMESPACE="${NAMESPACE:-default}"
DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "${DEMO_DIR}/../.." && pwd)"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  AIP Demo: Idle Resource Reaper — DataTalks Incident, Prevented"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# ── Preflight checks ──────────────────────────────────────────────────────────

echo "[ 1/6 ] Checking Kubernetes cluster..."
if ! kubectl cluster-info > /dev/null 2>&1; then
  echo "  ✗ No Kubernetes cluster found. Start KIND:"
  echo "    kind create cluster"
  exit 1
fi
echo "  ✓ Cluster ready"

echo "[ 2/6 ] Checking AIP Gateway at ${GATEWAY_URL}..."
if ! curl -sf "${GATEWAY_URL}/healthz" > /dev/null; then
  echo "  ✗ Gateway not running. Start it:"
  echo "    go run ${ROOT_DIR}/cmd/gateway/main.go"
  exit 1
fi
echo "  ✓ Gateway running"

echo "[ 3/6 ] Checking AIP Dashboard at ${DASHBOARD_URL}..."
if ! curl -sf "${DASHBOARD_URL}" > /dev/null 2>&1; then
  echo "  ✗ Dashboard not running. Start it:"
  echo "    go run ${ROOT_DIR}/cmd/dashboard/main.go"
  exit 1
fi
echo "  ✓ Dashboard running"

echo "[ 4/6 ] Cleaning up any leftovers from previous runs..."
kubectl delete agentrequests --all -n "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 || true
kubectl delete -f "${DEMO_DIR}/k8s/payment-api.yaml" --namespace "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 || true
echo "  ✓ Clean slate"

echo "[ 5/6 ] Deploying payment-api (3 replicas) to cluster..."
kubectl apply -f "${DEMO_DIR}/k8s/payment-api.yaml" --namespace "${NAMESPACE}" > /dev/null
echo "  Waiting for pods to be ready..."
kubectl rollout status deployment/payment-api -n "${NAMESPACE}" --timeout=90s
READY=$(kubectl get endpoints payment-api -n "${NAMESPACE}" -o jsonpath='{.subsets[0].addresses}' 2>/dev/null || echo "")
if [[ -n "$READY" ]]; then
  echo "  ✓ payment-api ready — active endpoints detected (live traffic signal)"
else
  echo "  ⚠ payment-api deployed but endpoints not yet active — wait a few seconds"
fi

echo "[ 6/6 ] Applying live-traffic-guard SafetyPolicy..."
kubectl apply -f "${DEMO_DIR}/policies/live-traffic-guard.yaml" --namespace "${NAMESPACE}" > /dev/null
echo "  ✓ live-traffic-guard active"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Dashboard: ${DASHBOARD_URL}"
echo "  Agent will try to DELETE payment-api (thinks it's idle — stale data)."
echo "  AIP will block it. Open the dashboard and DENY the deletion request."
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
sleep 2

# ── Run agent ─────────────────────────────────────────────────────────────────
# Set AGENT=claude to use the Claude-powered agent (requires ANTHROPIC_API_KEY).
# Default is the deterministic Go agent.

if [[ "${AGENT:-go}" == "claude" ]]; then
  if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
    echo "  ✗ ANTHROPIC_API_KEY not set — required for Claude agent"
    exit 1
  fi
  echo "  Using Python Claude-powered agent (claude-sonnet-4-6)"
  pip install -q -r "${DEMO_DIR}/claude-agent/requirements.txt"
  python3 "${DEMO_DIR}/claude-agent/agent.py" \
    --gateway "${GATEWAY_URL}" \
    --dashboard "${DASHBOARD_URL}" \
    --namespace "${NAMESPACE}"
elif [[ "${AGENT:-go}" == "claude-go" ]]; then
  if [[ -z "${AWS_ACCESS_KEY_ID:-}" ]] && [[ -z "${AWS_PROFILE:-}" ]]; then
    echo "  ⚠ AWS_ACCESS_KEY_ID or AWS_PROFILE not set — relying on default AWS credential chain"
  fi
  echo "  Using Golang Claude-powered agent (Amazon Bedrock)"
  (cd "${DEMO_DIR}/claude-agent-go" && go run main.go tools.go \
    --gateway "${GATEWAY_URL}" \
    --dashboard "${DASHBOARD_URL}" \
    --namespace "${NAMESPACE}")
else
  go run "${DEMO_DIR}/agent/main.go" \
    --gateway "${GATEWAY_URL}" \
    --dashboard "${DASHBOARD_URL}" \
    --namespace "${NAMESPACE}"
fi

# ── Cleanup prompt ────────────────────────────────────────────────────────────

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Demo complete. Re-run ./demo/scaledown/run.sh to replay."
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
