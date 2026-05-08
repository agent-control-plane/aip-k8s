#!/usr/bin/env bash
set -euo pipefail

GATEWAY_URL="${AIP_GATEWAY_URL:-http://localhost:8080}"
NAMESPACE="${NAMESPACE:-default}"
DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "${DEMO_DIR}/../.." && pwd)"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  AIP Demo: OpsLock — Distributed Concurrency Control"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "  Scenario: Two agents attempt to scale the same deployment"
echo "  simultaneously. AIP issues an OpsLock to exactly one agent"
echo "  and denies the other until the lock is released."
echo ""

# ── Preflight checks ──────────────────────────────────────────────────────────

echo "[ 1/5 ] Checking Kubernetes cluster..."
if ! kubectl cluster-info > /dev/null 2>&1; then
  echo "  ✗ No Kubernetes cluster found. Start KIND:"
  echo "    kind create cluster"
  exit 1
fi
echo "  ✓ Cluster ready"

echo "[ 2/5 ] Checking AIP CRDs..."
if ! kubectl get crd governedresources.governance.aip.io > /dev/null 2>&1; then
  echo "  ✗ AIP CRDs not installed."
  echo "    Install them: kubectl apply -k ${ROOT_DIR}/config/crd/bases"
  exit 1
fi
echo "  ✓ CRDs installed"

echo "[ 3/5 ] Checking AIP Gateway at ${GATEWAY_URL}..."
if ! curl -sf "${GATEWAY_URL}/healthz" > /dev/null; then
  echo "  ✗ Gateway not running. Start it:"
  echo "    go run ${ROOT_DIR}/cmd/gateway/main.go"
  echo ""
  echo "    Controller must also be running:"
  echo "    go run ${ROOT_DIR}/cmd/controller/main.go"
  exit 1
fi
echo "  ✓ Gateway running"

echo "[ 4/5 ] Cleaning up any leftovers from previous runs..."
kubectl delete agentrequests,auditrecords --all -n "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 || true
echo "  ✓ Clean slate"

echo "[ 5/5 ] Applying demo resources..."
kubectl apply -f "${DEMO_DIR}/k8s/resource.yaml" > /dev/null
echo "  ✓ GovernedResource (opslock-prod-deployments) applied"

TARGET="k8s://prod/default/deployment/payment-api"
echo ""
echo "  Target: ${TARGET}"
echo "  Launching agent-a and agent-b in parallel..."
echo ""

# ── Launch both agents simultaneously ────────────────────────────────────────

go run "${DEMO_DIR}/agent/main.go" \
  --agent-id=agent-a --target="${TARGET}" \
  --gateway="${GATEWAY_URL}" --namespace="${NAMESPACE}" &
PID_A=$!

go run "${DEMO_DIR}/agent/main.go" \
  --agent-id=agent-b --target="${TARGET}" \
  --gateway="${GATEWAY_URL}" --namespace="${NAMESPACE}" &
PID_B=$!

set +e
wait $PID_A; STATUS_A=$?
wait $PID_B; STATUS_B=$?
set -e

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Summary"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
if [ $STATUS_A -eq 0 ] && [ $STATUS_B -ne 0 ]; then
  echo "  ✅ agent-a acquired the OpsLock and completed successfully"
  echo "  🚫 agent-b was denied — contention detected by AIP"
elif [ $STATUS_B -eq 0 ] && [ $STATUS_A -ne 0 ]; then
  echo "  ✅ agent-b acquired the OpsLock and completed successfully"
  echo "  🚫 agent-a was denied — contention detected by AIP"
else
  echo "  Outcome: Mixed (check agent logs above for details)"
fi
echo ""
echo "  To watch live: kubectl get agentrequests -w -n ${NAMESPACE}"
echo ""
echo "  To clean up:"
echo "    kubectl delete agentrequests,auditrecords --all -n ${NAMESPACE}"
echo "    kubectl delete governedresource opslock-prod-deployments"
