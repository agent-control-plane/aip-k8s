#!/usr/bin/env bash
# check-helm-rbac.sh — verify that every resource in config/rbac/role.yaml
# (all API groups) is also present in the Helm chart controller ClusterRole.
#
# Detects RBAC drift: controller markers regenerated via 'make manifests' but
# the Helm chart ClusterRole was not manually updated to match.
#
# Exits 1 if any resource is missing from the Helm chart; lists every gap.
# Run with `make helm-rbac-check`.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUSTOMIZE_ROLE="$REPO_ROOT/config/rbac/role.yaml"
HELM_RBAC="$REPO_ROOT/charts/aip-k8s/templates/controller/rbac.yaml"

fail=0

if [ ! -f "$KUSTOMIZE_ROLE" ]; then
    echo "ERROR: $KUSTOMIZE_ROLE does not exist — run 'make manifests' first." >&2
    exit 1
fi

if [ ! -f "$HELM_RBAC" ]; then
    echo "ERROR: $HELM_RBAC does not exist." >&2
    exit 1
fi

# Parse config/rbac/role.yaml into (apiGroup, resource) pairs.
#
# Strategy: walk rules blocks line by line. When we see an apiGroups block,
# record the groups. When we then see a resources block, emit one
# "apiGroup|resource" line per (group, resource) combination.
# Skip resourceNames-scoped rules (they are subsets of already-checked rules).
#
# Written to be compatible with bash 3.x (macOS default).
pairs_file="$(mktemp)"
trap 'rm -f "$pairs_file"' EXIT

awk '
BEGIN {
    in_groups=0; in_resources=0; in_resource_names=0; in_verbs=0
    group_count=0; resource_count=0
}

# Detect start of apiGroups list
/^- apiGroups:/ {
    # Flush previous block
    if (group_count > 0 && resource_count > 0 && !has_resource_names) {
        for (g=0; g<group_count; g++) {
            for (r=0; r<resource_count; r++) {
                print groups[g] "|" resources[r]
            }
        }
    }
    delete groups; delete resources
    group_count=0; resource_count=0
    has_resource_names=0
    in_groups=1; in_resources=0; in_resource_names=0; in_verbs=0
    next
}

# Inside a rule block, detect section headers
/^  resources:/ && !in_groups    { in_resources=1; in_resource_names=0; in_verbs=0; next }
/^  resourceNames:/ && !in_groups { in_resource_names=1; has_resource_names=1; in_resources=0; in_verbs=0; next }
/^  verbs:/                       { in_verbs=1; in_resources=0; in_resource_names=0; next }

# Collect apiGroups entries (indented "  - value")
in_groups && /^  - / {
    val=$0; sub(/^  - /, "", val); gsub(/[ \t]+$/, "", val)
    # empty string means core group ("")
    if (val == "\"\"" || val == "") val=""
    groups[group_count++]=val
    next
}

# End of apiGroups when we see resources or resourceNames
in_groups && /^  (resources|resourceNames|verbs):/ {
    in_groups=0
    if (/resources:/)     { in_resources=1 }
    if (/resourceNames:/) { in_resource_names=1; has_resource_names=1 }
    if (/verbs:/)         { in_verbs=1 }
    next
}

# Collect resources entries
in_resources && /^  - / {
    val=$0; sub(/^  - /, "", val); gsub(/[ \t]+$/, "", val)
    resources[resource_count++]=val
    next
}
in_resources && !/^  - / && !/^$/ { in_resources=0 }

# Ignore verbs and other sections
in_verbs && /^  - / { next }
in_verbs && !/^  - / && !/^$/ { in_verbs=0 }
in_resource_names && /^  - / { next }
in_resource_names && !/^  - / && !/^$/ { in_resource_names=0 }

END {
    # Flush last block
    if (group_count > 0 && resource_count > 0 && !has_resource_names) {
        for (g=0; g<group_count; g++) {
            for (r=0; r<resource_count; r++) {
                print groups[g] "|" resources[r]
            }
        }
    }
}
' "$KUSTOMIZE_ROLE" > "$pairs_file"

if [ ! -s "$pairs_file" ]; then
    echo "ERROR: no resource rules found in $KUSTOMIZE_ROLE." >&2
    echo "  Is 'make manifests' up to date?" >&2
    exit 1
fi

# For each (apiGroup, resource) pair from the canonical role, verify it appears
# in the Helm ClusterRole. We check that:
#   1. The apiGroup appears under an "apiGroups:" block in the Helm file.
#   2. The resource name appears under a "resources:" block in the same file.
#
# Note: we do not require them to be in the same rule block — Helm chart may
# split rules differently than controller-gen. We only verify presence.

missing_file="$(mktemp)"
trap 'rm -f "$pairs_file" "$missing_file"' EXIT

while IFS='|' read -r apigroup resource; do
    # Determine what to look for in the Helm file for the apiGroup line.
    if [ "$apigroup" = "" ]; then
        group_pattern='^[[:space:]]*- ""$'
    else
        group_pattern="^[[:space:]]*- ${apigroup}$"
    fi

    if ! grep -qE "$group_pattern" "$HELM_RBAC"; then
        echo "${apigroup:-\"\"}/${resource}" >> "$missing_file"
        continue
    fi

    if ! grep -qE "^[[:space:]]*- ${resource}$" "$HELM_RBAC"; then
        echo "${apigroup:-\"\"}/${resource}" >> "$missing_file"
    fi
done < "$pairs_file"

if [ -s "$missing_file" ]; then
    echo "RBAC DRIFT: the following resources are in $KUSTOMIZE_ROLE"
    echo "  but missing from $HELM_RBAC:"
    while IFS= read -r r; do
        echo "    - $r"
    done < "$missing_file"
    echo ""
    echo "Update the ClusterRole in $HELM_RBAC to include the missing entries."
    echo "(Canonical source: $KUSTOMIZE_ROLE — regenerated by 'make manifests')"
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    exit 1
fi

echo "Helm chart controller RBAC is in sync with $KUSTOMIZE_ROLE."

# ── Gateway RBAC invariants ──────────────────────────────────────────────────
# The gateway ClusterRole is hand-maintained. Enforce known invariants:
# if a resource is listed, its /status subresource must also be listed
# (gateway patches status on all mutable resources).
HELM_GW_RBAC="$REPO_ROOT/charts/aip-k8s/templates/gateway/rbac.yaml"

if [ ! -f "$HELM_GW_RBAC" ]; then
    echo "ERROR: $HELM_GW_RBAC does not exist." >&2
    exit 1
fi

gw_fail=0
for base_resource in agentrequests diagnosticaccuracysummaries governedresources; do
    status_resource="${base_resource}/status"
    if grep -qE "^[[:space:]]*- ${base_resource}$" "$HELM_GW_RBAC"; then
        if ! grep -qE "^[[:space:]]*- ${status_resource}$" "$HELM_GW_RBAC"; then
            echo "GATEWAY RBAC DRIFT: '${base_resource}' is present in $HELM_GW_RBAC"
            echo "  but '${status_resource}' is missing."
            gw_fail=1
        fi
    fi
done

if [ "$gw_fail" -ne 0 ]; then
    exit 1
fi

echo "Gateway RBAC invariants satisfied."
