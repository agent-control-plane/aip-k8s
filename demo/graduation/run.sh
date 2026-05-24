#!/usr/bin/env bash
set -euo pipefail

GATEWAY_PORT="${GATEWAY_PORT:-8080}"
CTL_PROBE_PORT="${CTL_PROBE_PORT:-8081}"
NAMESPACE="${NAMESPACE:-default}"
DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "${DEMO_DIR}/../.." && pwd)"
HELM_NAMESPACE="${HELM_NAMESPACE:-aip-k8s-system}"
HELM_RELEASE="${HELM_RELEASE:-aip-k8s}"

GATEWAY_URL="http://localhost:${GATEWAY_PORT}"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

banner() { echo -e "\n${BOLD}${CYAN}=== $* ===${RESET}\n"; }
info()   { echo -e "${GREEN}▶${RESET} $*"; }
warn()   { echo -e "${YELLOW}!${RESET} $*"; }

cleanup() {
  warn "Cleaning up demo processes..."
  [[ -n "${CTL_PID:-}" ]] && kill "$CTL_PID" 2>/dev/null || true
  [[ -n "${GW_PID:-}" ]]  && kill "$GW_PID"  2>/dev/null || true
  warn "Restoring in-cluster deployments to 1 replica..."
  kubectl scale deployment "${HELM_RELEASE}-controller" "${HELM_RELEASE}-gateway" "${HELM_RELEASE}-dashboard" \
    -n "$HELM_NAMESPACE" --replicas=1 2>/dev/null || true
}
trap cleanup EXIT

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  AIP Demo: Trust Graduation Ladder"
echo "  Observer → Advisor → Supervised → Trusted → Autonomous"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# ── Prerequisites ─────────────────────────────────────────────────────────────
banner "Checking prerequisites"
for cmd in kubectl go curl; do
  command -v "$cmd" &>/dev/null || { echo "  ✗ Required: $cmd"; exit 1; }
done
if ! kubectl cluster-info > /dev/null 2>&1; then
  echo "  ✗ No Kubernetes cluster found."
  echo "    Create one: kind create cluster --name aip-demo"
  exit 1
fi
info "Prerequisites ok"

# ── CRDs ──────────────────────────────────────────────────────────────────────
banner "Installing AIP CRDs"
kubectl apply -f "$ROOT_DIR/charts/aip-k8s/crds/" --server-side > /dev/null
info "CRDs up to date"

# ── Scale down in-cluster deployments + kill leftover local processes ─────────
banner "Clearing port conflicts"
lsof -ti tcp:"$CTL_PROBE_PORT" | xargs kill -9 2>/dev/null || true
lsof -ti tcp:"$GATEWAY_PORT"   | xargs kill -9 2>/dev/null || true
kubectl scale deployment "${HELM_RELEASE}-controller" "${HELM_RELEASE}-gateway" "${HELM_RELEASE}-dashboard" \
  -n "$HELM_NAMESPACE" --replicas=0 2>/dev/null || true
info "In-cluster deployments scaled to 0"

# ── Apply demo resources and clear previous state (BEFORE controller starts) ──
# Cleanup must happen before the controller starts. If the controller is running
# during cleanup, it can race to recreate the trust profile from stale DAS or
# AuditRecord data before those are deleted, leaving the agent at the wrong level.
banner "Applying demo resources"

if kubectl get agentgraduationpolicy default > /dev/null 2>&1; then
  warn "AgentGraduationPolicy 'default' already exists — overwriting with demo thresholds."
fi
kubectl apply -f "${DEMO_DIR}/k8s/policy.yaml" > /dev/null
kubectl apply -f "${DEMO_DIR}/k8s/resource.yaml" > /dev/null
info "AgentGraduationPolicy (default) applied"
info "GovernedResource (demo-deployments) applied"

# Delete in dependency order: accuracy data first so the controller cannot
# reconstruct a stale trust level before the profile itself is deleted.
# Leases must also be deleted: if a previous run was killed with in-flight
# requests, the OpsLock leases are not released and will cause lock contention
# on subsequent runs that reuse the same target URIs.
kubectl delete auditrecords --all -n "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 || true
kubectl delete diagnosticaccuracysummaries --all -n "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 || true
kubectl delete agenttrustprofiles --all -n "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 || true
kubectl delete agentrequests --all -n "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 || true
kubectl delete leases -l governance.aip.io/managed-by=aip-controller -n "${NAMESPACE}" --ignore-not-found > /dev/null 2>&1 || true
info "Previous demo state cleared"

# ── Build binaries ────────────────────────────────────────────────────────────
banner "Building gateway and controller"
mkdir -p "$ROOT_DIR/bin"
go build -o "$ROOT_DIR/bin/gateway" "$ROOT_DIR/cmd/gateway"
go build -o "$ROOT_DIR/bin/manager" "$ROOT_DIR/cmd/main.go"
info "Build complete"

# ── Start AIP Controller ──────────────────────────────────────────────────────
banner "Starting AIP Controller (health probe: $CTL_PROBE_PORT)"
"$ROOT_DIR/bin/manager" \
  --health-probe-bind-address ":$CTL_PROBE_PORT" \
  --metrics-bind-address "0" \
  --jwt-key-namespace "$HELM_NAMESPACE" \
  &
CTL_PID=$!
info "Waiting for controller health probe..."
for i in $(seq 1 30); do
  if curl -sf "http://localhost:$CTL_PROBE_PORT/healthz" > /dev/null; then
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "Controller failed to start"
    exit 1
  fi
  sleep 1
done
info "AIP Controller running (PID $CTL_PID)"

# ── Start AIP Gateway (open mode — no auth required) ─────────────────────────
banner "Starting AIP Gateway (port $GATEWAY_PORT)"
"$ROOT_DIR/bin/gateway" \
  --addr ":$GATEWAY_PORT" \
  --wait-timeout 120s \
  &
GW_PID=$!
info "Waiting for gateway health probe..."
for i in $(seq 1 30); do
  if curl -sf "${GATEWAY_URL}/healthz" > /dev/null; then
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "Gateway failed to start"
    exit 1
  fi
  sleep 1
done
info "AIP Gateway running at ${GATEWAY_URL} (PID $GW_PID)"

# ── Run the agent ─────────────────────────────────────────────────────────────
banner "Running graduation demo agent"

go run "${DEMO_DIR}/agent/main.go" \
  --gateway="${GATEWAY_URL}" \
  --namespace="${NAMESPACE}"

# ── Post-run summary ──────────────────────────────────────────────────────────
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Post-run audit (last 15 records):"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
kubectl get auditrecords -n "${NAMESPACE}" \
  --sort-by=.spec.timestamp 2>/dev/null | tail -15 || true

echo ""
echo "  To clean up all demo resources:"
echo "    kubectl delete agentrequests,auditrecords -n ${NAMESPACE} -l aip.io/agentIdentity=graduation-demo-agent"
echo "    kubectl delete governedresource demo-deployments"
echo "    kubectl delete agentgraduationpolicy default"
