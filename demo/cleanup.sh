#!/bin/bash

# Ensure we're in the project root
cd "$(dirname "$0")/.."

echo "=== AIP Demo Cleanup: Purging all objects ==="

NAMESPACE=${1:-"default"}

echo "Cleaning up objects in namespace: $NAMESPACE"

# 1. Delete AgentRequests
echo "Deleting AgentRequests..."
kubectl delete agentrequests --all -n "$NAMESPACE" --ignore-not-found --timeout=30s

# 2. Delete AuditRecords
echo "Deleting AuditRecords..."
kubectl delete auditrecords --all -n "$NAMESPACE" --ignore-not-found --timeout=30s

# 3. Delete SafetyPolicies
echo "Deleting SafetyPolicies..."
kubectl delete safetypolicies --all -n "$NAMESPACE" --ignore-not-found

# 4. Delete Leases (Locks)
echo "Deleting AIP Leases..."
kubectl get leases -n "$NAMESPACE" -o name 2>/dev/null | grep "aip-lock-" | xargs kubectl delete -n "$NAMESPACE" 2>/dev/null || true

# 5. Delete demo deployments
echo "Deleting demo deployments..."
kubectl delete deployment payment-api -n "$NAMESPACE" --ignore-not-found
kubectl delete service payment-api -n "$NAMESPACE" --ignore-not-found

echo "✅ Cleanup complete."
