# Ensure we're in the project root
cd "$(dirname "$0")/../.."

GATEWAY_URL=${GATEWAY_URL:-"http://localhost:8080"}

echo "=== AIP OpsLock Demo: Distributed Concurrency Control ==="

# 1. Check if gateway is running
if ! curl -sf "$GATEWAY_URL/healthz" > /dev/null; then
  echo "❌ Error: Demo Gateway is not running at $GATEWAY_URL"
  echo "Please start it with: go run demo/gateway/main.go"
  exit 1
fi

TARGET="k8s://prod/default/deployment/payment-api"

echo "Scenario: Two independent agents (agent-a and agent-b) attempt to scale the same deployment simultaneously."
echo "Target: $TARGET"
echo

# 2. Launch agents simultaneously
echo "Launching agents..."
go run demo/opslock/agent/main.go --agent-id agent-a --target "$TARGET" &
PID_A=$!
go run demo/opslock/agent/main.go --agent-id agent-b --target "$TARGET" &
PID_B=$!

# 3. Wait for both to finish
wait $PID_A
STATUS_A=$?
wait $PID_B
STATUS_B=$?

echo
echo "=== Summary ==="
if [ $STATUS_A -eq 0 ] && [ $STATUS_B -ne 0 ]; then
  echo "✅ agent-a succeeded (acquired OpsLock)"
  echo "🚫 agent-b was denied (contention detected)"
elif [ $STATUS_B -eq 0 ] && [ $STATUS_A -ne 0 ]; then
  echo "✅ agent-b succeeded (acquired OpsLock)"
  echo "🚫 agent-a was denied (contention detected)"
else
  echo "Outcome: Mixed (Check agent logs above for details)"
fi

echo
echo "To watch live: kubectl get agentrequests -w -n default"
