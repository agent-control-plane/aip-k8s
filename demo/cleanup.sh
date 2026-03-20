#!/bin/bash

# Ensure we're in the project root
cd "$(dirname "$0")/.."

echo "=== AIP Demo Cleanup: Purging all objects ==="

NAMESPACE=${1:-"default"}

echo "Cleaning up objects in namespace: $NAMESPACE"

# 1. Delete AgentRequests
echo "Deleting AgentRequests..."
kubectl delete agentrequests --all -n "$NAMESPACE" --timeout=30s

# 2. Delete AuditRecords
echo "Deleting AuditRecords..."
kubectl delete auditrecords --all -n "$NAMESPACE" --timeout=30s

# 3. Delete SafetyPolicies
echo "Deleting SafetyPolicies..."
kubectl delete safetypolicies --all -n "$NAMESPACE"

# 4. Delete Leases (Locks)
echo "Deleting AIP Leases..."
kubectl get leases -n "$NAMESPACE" -o name | grep "aip-lock-" | xargs kubectl delete -n "$NAMESPACE" 2>/dev/null

echo "✅ Cleanup complete."
