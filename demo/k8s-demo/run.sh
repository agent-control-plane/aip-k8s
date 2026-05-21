#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
GATEWAY_PORT="${GATEWAY_PORT:-18080}"
DASHBOARD_PORT="${DASHBOARD_PORT:-18082}"
K8S_MCP_PORT="${K8S_MCP_PORT:-8090}"
CTL_PROBE_PORT="${CTL_PROBE_PORT:-8081}"
JWT_KEY_PATH="${JWT_KEY_PATH:-$ROOT_DIR/bin/demo-jwt.key}"
NAMESPACE="${NAMESPACE:-default}"
K8S_MCP_VERSION="${K8S_MCP_VERSION:-v0.0.62}"
HELM_NAMESPACE="${HELM_NAMESPACE:-aip-k8s-system}"
HELM_RELEASE="${HELM_RELEASE:-aip-k8s}"

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
  [[ -n "${CTL_PID:-}" ]]  && kill "$CTL_PID"  2>/dev/null || true
  [[ -n "${GW_PID:-}" ]]   && kill "$GW_PID"   2>/dev/null || true
  [[ -n "${DASH_PID:-}" ]] && kill "$DASH_PID" 2>/dev/null || true
  [[ -n "${MCP_PID:-}" ]]  && kill "$MCP_PID"  2>/dev/null || true
  warn "Restoring in-cluster deployments to 1 replica..."
  kubectl scale deployment "${HELM_RELEASE}-controller" "${HELM_RELEASE}-gateway" \
    -n "$HELM_NAMESPACE" --replicas=1 2>/dev/null || true
  warn "Removing demo manifests..."
  kubectl delete -f "$SCRIPT_DIR/manifests/" --ignore-not-found 2>/dev/null || true
  warn "Removing kubectl-block hook..."
  SETTINGS_FILE="$ROOT_DIR/.claude/settings.json"
  if [[ -f "$SETTINGS_FILE" ]]; then
    python3 - "$SETTINGS_FILE" << 'PYEOF'
import json, sys
path = sys.argv[1]
with open(path) as f:
    s = json.load(f)
if "hooks" in s and "PreToolUse" in s["hooks"]:
    s["hooks"]["PreToolUse"] = [
        h for h in s["hooks"]["PreToolUse"]
        if not any("block-kubectl" in str(hook.get("command","")) for hook in h.get("hooks",[]))
    ]
with open(path, "w") as f:
    json.dump(s, f, indent=2)
PYEOF
  fi
  rm -f "$ROOT_DIR/.claude/hooks/block-kubectl.sh"
}
trap cleanup EXIT

# ── Prerequisites ─────────────────────────────────────────────────────────────
banner "Checking prerequisites"
for cmd in kubectl go curl openssl; do
  command -v "$cmd" &>/dev/null || { echo "Required: $cmd"; exit 1; }
done

# CRDs must exist — installed by Helm. The in-cluster controller and gateway
# will be scaled to 0; this script runs them locally from the current source.
if ! kubectl get crd agentrequests.governance.aip.io &>/dev/null; then
  echo "AIP CRDs not found. Install AIP first:"
  echo "  helm install aip-k8s oci://ghcr.io/agent-control-plane/aip-k8s/charts/aip-k8s \\"
  echo "    --namespace aip-k8s-system --create-namespace"
  exit 1
fi
info "All prerequisites present"

# ── Refresh CRDs from local chart to ensure schema matches the binaries ───────
# The installed Helm chart may be an older version missing trustRequirements
# or AgentGraduationPolicy. Apply the local CRDs to bring the cluster up to date.
banner "Refreshing CRDs from local chart"
kubectl apply -f "$ROOT_DIR/charts/aip-k8s/crds/" --server-side
info "CRDs up to date"

# ── Scale down in-cluster controller and gateway to avoid split-brain ─────────
banner "Scaling down in-cluster controller and gateway"
lsof -ti tcp:"$CTL_PROBE_PORT" | xargs kill -9 2>/dev/null || true
kubectl scale deployment "${HELM_RELEASE}-controller" "${HELM_RELEASE}-gateway" \
  -n "$HELM_NAMESPACE" --replicas=0 2>/dev/null || true
info "In-cluster deployments scaled to 0"

# ── Build gateway, dashboard, and controller binaries ────────────────────────
banner "Building gateway, dashboard, and controller"
mkdir -p "$ROOT_DIR/bin"
go build -o "$ROOT_DIR/bin/gateway"   "$ROOT_DIR/cmd/gateway"
go build -o "$ROOT_DIR/bin/dashboard" "$ROOT_DIR/cmd/dashboard"
go build -o "$ROOT_DIR/bin/manager"   "$ROOT_DIR/cmd/main.go"
info "Build complete"

# ── Download containers/kubernetes-mcp-server ─────────────────────────────────
banner "Downloading containers/kubernetes-mcp-server $K8S_MCP_VERSION"
K8S_MCP_BIN="$ROOT_DIR/bin/kubernetes-mcp-server"
if [[ ! -f "$K8S_MCP_BIN" ]]; then
  OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
  ARCH="$(uname -m)"
  [[ "$ARCH" == "x86_64" ]] && ARCH="amd64"
  [[ "$ARCH" == "aarch64" ]] && ARCH="arm64"
  ASSET="kubernetes-mcp-server-${OS}-${ARCH}"
  URL="https://github.com/containers/kubernetes-mcp-server/releases/download/${K8S_MCP_VERSION}/${ASSET}"
  info "Downloading $ASSET..."
  curl -fsSL -o "$K8S_MCP_BIN" "$URL"
  chmod +x "$K8S_MCP_BIN"
  info "Downloaded to $K8S_MCP_BIN"
else
  info "Using cached $K8S_MCP_BIN"
fi

# ── Install Claude Code hook to block direct kubectl/k8s CLI access ───────────
# Installed AFTER manifests are applied so setup kubectl calls are not blocked.
# Forces Claude to route all Kubernetes operations through AIP MCP tools.
banner "Installing Claude Code kubectl-block hook"
HOOK_DIR="$ROOT_DIR/.claude/hooks"
mkdir -p "$HOOK_DIR"

cat > "$HOOK_DIR/block-kubectl.sh" << 'HOOKEOF'
#!/usr/bin/env bash
# Blocks direct kubectl/helm/k9s usage — forces routing through AIP MCP tools.
# Claude Code passes tool input as JSON on stdin.
INPUT=$(cat)
COMMAND=$(echo "$INPUT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('tool_input',{}).get('command',''))" 2>/dev/null || true)
for blocked in kubectl helm k9s kubeadm; do
  if echo "$COMMAND" | grep -qw "$blocked"; then
    python3 -c "
import json
print(json.dumps({
  'hookSpecificOutput': {
    'hookEventName': 'PreToolUse',
    'permissionDecision': 'deny',
    'permissionDecisionReason': (
      'Direct Kubernetes CLI access is blocked in this demo. '
      'Use the AIP MCP tools (k8s/resources_scale, k8s/resources_list, etc.) '
      'which route through the AIP governance layer.'
    )
  }
}))
"
    exit 0
  fi
done
exit 0
HOOKEOF
chmod +x "$HOOK_DIR/block-kubectl.sh"

# Merge hook into project-level .claude/settings.json
SETTINGS_FILE="$ROOT_DIR/.claude/settings.json"
if [[ ! -f "$SETTINGS_FILE" ]]; then
  echo '{}' > "$SETTINGS_FILE"
fi
python3 - "$SETTINGS_FILE" "$HOOK_DIR/block-kubectl.sh" << 'PYEOF'
import json, sys
settings_path, hook_path = sys.argv[1], sys.argv[2]
with open(settings_path) as f:
    settings = json.load(f)
settings.setdefault("hooks", {}).setdefault("PreToolUse", [])
# Remove any existing kubectl-block entry before re-adding
settings["hooks"]["PreToolUse"] = [
    h for h in settings["hooks"]["PreToolUse"]
    if not any("block-kubectl" in str(hook.get("command","")) for hook in h.get("hooks",[]))
]
settings["hooks"]["PreToolUse"].append({
    "matcher": "Bash",
    "hooks": [{"type": "command", "command": hook_path, "timeout": 5}]
})
with open(settings_path, "w") as f:
    json.dump(settings, f, indent=2)
print(f"Hook installed in {settings_path}")
PYEOF
info "kubectl-block hook installed — Claude will be forced to use AIP MCP tools"

# ── Generate JWT signing key ──────────────────────────────────────────────────
if [[ ! -f "$JWT_KEY_PATH" ]]; then
  banner "Generating JWT signing key"
  openssl genpkey -algorithm ed25519 -out "$JWT_KEY_PATH"
  info "Key written to $JWT_KEY_PATH"
fi

# ── Reset agent trust profile so demo always starts from Observer ─────────────
banner "Resetting agent trust profiles"
kubectl delete agenttrustprofiles --all -n "$NAMESPACE" --ignore-not-found 2>/dev/null || true
info "Agent trust profiles cleared (demo starts as Observer)"

# ── Apply AIP manifests ───────────────────────────────────────────────────────
banner "Applying AIP demo manifests"
kubectl apply -f "$SCRIPT_DIR/manifests/payment-api.yaml"
kubectl apply -f "$SCRIPT_DIR/manifests/governed-resource.yaml"
kubectl apply -f "$SCRIPT_DIR/manifests/safety-policy.yaml"
kubectl apply -f "$SCRIPT_DIR/manifests/graduation-policy.yaml"
info "Waiting for payment-api to be ready..."
kubectl rollout status deployment/payment-api -n "$NAMESPACE" --timeout=60s

# ── Start containers/kubernetes-mcp-server ────────────────────────────────────
# Runs internally — Claude never sees this URL, only the AIP gateway endpoint.
banner "Starting kubernetes-mcp-server (port $K8S_MCP_PORT)"
# Kill any leftover process from a previous run on this port.
lsof -ti tcp:"$K8S_MCP_PORT"   | xargs kill -9 2>/dev/null || true
lsof -ti tcp:"$GATEWAY_PORT"   | xargs kill -9 2>/dev/null || true
lsof -ti tcp:"$DASHBOARD_PORT" | xargs kill -9 2>/dev/null || true
"$K8S_MCP_BIN" --port "$K8S_MCP_PORT" &
MCP_PID=$!
sleep 2
# Health check: verify the process is still alive and serving MCP.
kill -0 "$MCP_PID" 2>/dev/null || { echo "kubernetes-mcp-server failed to start"; exit 1; }
info "kubernetes-mcp-server running (PID $MCP_PID)"

# ── Start AIP Controller ──────────────────────────────────────────────────────
banner "Starting AIP Controller (health probe: $CTL_PROBE_PORT)"
"$ROOT_DIR/bin/manager" \
  --health-probe-bind-address ":$CTL_PROBE_PORT" \
  --metrics-bind-address "0" \
  --jwt-key-namespace "$HELM_NAMESPACE" \
  &
CTL_PID=$!
sleep 3
curl -sf "http://localhost:$CTL_PROBE_PORT/healthz" >/dev/null || { echo "controller failed to start"; exit 1; }
info "AIP Controller running (PID $CTL_PID)"

# ── Start AIP Gateway ─────────────────────────────────────────────────────────
banner "Starting AIP Gateway (port $GATEWAY_PORT)"
MCP_REGISTRY=$(printf '[{"name":"k8s","url":"http://localhost:%s/mcp","status":"available","tools":[{"name":"resources_scale","read_only":false},{"name":"resources_delete","read_only":false},{"name":"resources_get","read_only":true},{"name":"resources_list","read_only":true},{"name":"pods_list","read_only":true},{"name":"pods_log","read_only":true},{"name":"namespaces_list","read_only":true},{"name":"events_list","read_only":true}]}]' "$K8S_MCP_PORT")

MCP_REGISTRY="$MCP_REGISTRY" \
  "$ROOT_DIR/bin/gateway" \
  --jwt-key-path "$JWT_KEY_PATH" \
  --addr ":$GATEWAY_PORT" \
  --wait-timeout 120s \
  &
GW_PID=$!
sleep 2
curl -sf "http://localhost:$GATEWAY_PORT/healthz" >/dev/null || { echo "gateway failed to start"; exit 1; }
info "AIP Gateway running (PID $GW_PID)"

# ── Start AIP Dashboard ───────────────────────────────────────────────────────
banner "Starting AIP Dashboard (port $DASHBOARD_PORT)"
"$ROOT_DIR/bin/dashboard" \
  --port "$DASHBOARD_PORT" \
  --gateway-url "http://localhost:$GATEWAY_PORT" \
  &
DASH_PID=$!
sleep 1
kill -0 "$DASH_PID" 2>/dev/null || { echo "dashboard failed to start"; exit 1; }
info "AIP Dashboard running at http://localhost:$DASHBOARD_PORT (PID $DASH_PID)"

# ── Print Claude Code config ──────────────────────────────────────────────────
banner "Claude Code configuration"
cat <<EOF
Register the AIP gateway as your only Kubernetes MCP server:

  claude mcp add-json aip '{"url":"http://localhost:$GATEWAY_PORT/mcp"}'

  # Or add to your project's .mcp.json:
  {
    "mcpServers": {
      "aip": { "url": "http://localhost:$GATEWAY_PORT/mcp" }
    }
  }

Add to CLAUDE.md (system prompt):
  You have access to Kubernetes tools via AIP. Read-only tools work immediately.
  For write tools (scale, delete), AIP enforces governance:
  - If a call returns {"status":"pending_approval","requestId":"..."}, call
    aip/await_approval with that requestId and wait for human approval.
  - On approval you receive a JWT — re-call the original tool with
    _aip_authorization set to that JWT value.
  - On denial, report the reason to the user and stop.

Dashboard (approve/deny requests):
  http://localhost:$DASHBOARD_PORT
EOF

# ── Demo narrative ────────────────────────────────────────────────────────────
banner "Demo ready — three scenarios"
echo -e "
${BOLD}Scenario 1 — Scale beyond policy cap (auto-denied, no human needed):${RESET}
  Ask Claude: \"Scale the payment-api deployment to 10 replicas\"
  Expected:
    1. Claude calls k8s/resources_scale {scale:10}
    2. AIP evaluates SafetyPolicy before touching K8s API
    3. Rule 'cap-replica-count' fires (scale > 7) → Denied instantly
    4. Claude reads the SafetyPolicy, explains: 'Policy caps production at 7 replicas'
  vs OPA/Gatekeeper: fires at K8s admission — AFTER the agent forms the API call.
  AIP intercepts agent intent, before any API call is made.

${BOLD}Scenario 2 — Scale within limit (requires human approval + JWT):${RESET}
  Ask Claude: \"Scale the payment-api deployment to 5 replicas\"
  Expected:
    1. Claude calls k8s/resources_scale {scale:5} — passes cap check
    2. Rule 'require-approval-for-scale' fires → pending_approval returned
    3. Claude calls aip/await_approval and blocks
    4. You approve in the AIP dashboard → JWT minted (agent identity + action + expiry)
    5. Claude re-calls with _aip_authorization=<jwt> → AIP validates → K8s scaled ✓
  No sandbox, no OPA: a human-issued capability token proves the approval.

${BOLD}Scenario 3 — Delete (requires human approval):${RESET}
  Ask Claude: \"Delete the payment-api deployment\"
  Expected:
    1. Claude calls k8s/resources_delete
    2. Rule 'require-approval-for-delete' fires → pending_approval returned
    3. Claude calls aip/await_approval and blocks
    4. You approve or deny in the AIP dashboard — delete only proceeds with JWT
  Demonstrates that destructive operations get the same governance treatment as mutations.

${BOLD}Current deployment state:${RESET}"
kubectl get deployment payment-api -n "$NAMESPACE" 2>/dev/null || true

echo -e "\n${YELLOW}Press Ctrl+C to stop the demo and clean up${RESET}"
wait
