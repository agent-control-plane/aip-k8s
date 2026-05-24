#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "=== AIP Demo Cleanup: Purging all objects ==="

NAMESPACE=${1:-"default"}
echo "Namespace: $NAMESPACE"

echo "Deleting AgentRequests..."
kubectl delete agentrequests --all -n "$NAMESPACE" --ignore-not-found --timeout=30s

echo "Deleting AuditRecords..."
kubectl delete auditrecords --all -n "$NAMESPACE" --ignore-not-found --timeout=30s

echo "Deleting SafetyPolicies..."
kubectl delete safetypolicies --all -n "$NAMESPACE" --ignore-not-found

echo "Deleting demo GovernedResources (cluster-scoped)..."
kubectl delete governedresource kiro-prod-deployments scaledown-prod-deployments opslock-prod-deployments demo-deployments --ignore-not-found

echo "Deleting AgentGraduationPolicy..."
kubectl delete agentgraduationpolicy default -n "$NAMESPACE" --ignore-not-found

echo "Deleting AgentTrustProfiles..."
kubectl delete agenttrustprofiles --all -n "$NAMESPACE" --ignore-not-found --timeout=30s

echo "Deleting DiagnosticAccuracySummaries..."
kubectl delete diagnosticaccuracysummaries --all -n "$NAMESPACE" --ignore-not-found --timeout=30s

echo "Deleting AIP Leases..."
kubectl get leases -n "$NAMESPACE" -o name 2>/dev/null | grep "aip-lock-" | xargs kubectl delete -n "$NAMESPACE" 2>/dev/null || true

echo "Deleting demo deployments..."
kubectl delete deployment payment-api -n "$NAMESPACE" --ignore-not-found
kubectl delete service payment-api -n "$NAMESPACE" --ignore-not-found

echo "✅ Cleanup complete."
