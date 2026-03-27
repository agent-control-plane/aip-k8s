#!/bin/bash

# Ensure we're in the project root
cd "$(dirname "$0")/../.."

GATEWAY_URL=${GATEWAY_URL:-"http://localhost:8080"}

echo "=== AIP Kiro Scenario: Autonomous Production Guardrails ==="

# 1. Check if gateway is running
if ! curl -sf "$GATEWAY_URL/healthz" > /dev/null; then
  echo "❌ Error: Demo Gateway is not running at $GATEWAY_URL"
  echo "Please start it with: go run cmd/gateway/main.go"
  exit 1
fi

# 2. Apply the SafetyPolicy
echo "Applying SafetyPolicy: RequireApproval for production targets..."
kubectl apply -f demo/kiro/policies/prod-require-approval.yaml

# 3. Run the Kiro agent
echo "Running Kiro coding agent..."
go run demo/kiro/agent/main.go

echo
echo "=== Post-Scenario Audit ==="
echo "AuditRecords emitted for this namespace:"
kubectl get auditrecords -n default --sort-by=.metadata.creationTimestamp | tail -n 5

echo
echo "To clean up: kubectl delete safetypolicy prod-require-approval"
