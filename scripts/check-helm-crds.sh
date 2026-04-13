#!/usr/bin/env bash
# check-helm-crds.sh — verify that every CRD in charts/aip-k8s/templates/crds/
# matches the canonical CRD in config/crd/bases/ with the three Helm lifecycle
# annotations injected by sync-helm-crds.sh.
#
# Detects two classes of drift:
#   1. Content drift  — canonical CRD changed without re-running sync-helm-crds.sh
#   2. Annotation drift — Helm lifecycle annotations modified/removed in the chart
#   3. Extra CRDs — chart contains a CRD not present in config/crd/bases/
#
# Exits 1 if any drift is detected; prints a diff for each drifted file.
# Run with `make helm-crds-check`.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASES="$REPO_ROOT/config/crd/bases"
CHART="$REPO_ROOT/charts/aip-k8s/templates/crds"

# awk program that mirrors sync-helm-crds.sh exactly.
# Generating expected output here (rather than stripping annotations from dst)
# means both content drift AND annotation drift are caught.
AWK_INJECT='/controller-gen\.kubebuilder\.io\/version:/ {
    print
    print "    helm.sh/resource-policy: keep"
    print "    \"helm.sh/hook\": pre-install,pre-upgrade"
    print "    \"helm.sh/hook-weight\": \"-5\""
    next
}
{ print }'

fail=0

# --- Forward check: every canonical CRD must exist and match in the chart ---
for src in "$BASES"/*.yaml; do
    name="$(basename "$src")"
    dst="$CHART/$name"

    if [ ! -f "$dst" ]; then
        echo "MISSING: $name not found in $CHART"
        echo "  Run 'make sync-helm-crds' to fix."
        fail=1
        continue
    fi

    expected="$(awk "$AWK_INJECT" "$src")"
    if ! diff -u <(echo "$expected") "$dst" > /dev/null 2>&1; then
        echo "DRIFT: $name"
        diff -u <(echo "$expected") "$dst" || true
        echo ""
        fail=1
    fi
done

# --- Reverse check: every chart CRD must have a canonical counterpart ---
for dst in "$CHART"/*.yaml; do
    name="$(basename "$dst")"
    src="$BASES/$name"

    if [ ! -f "$src" ]; then
        echo "EXTRA: $name found in $CHART but missing in $BASES"
        echo "  Remove it from the chart or add a matching CRD to $BASES."
        fail=1
    fi
done

if [ "$fail" -ne 0 ]; then
    echo "Helm chart CRDs are out of sync with config/crd/bases/."
    echo "Run 'make sync-helm-crds' (after 'make manifests') to fix."
    exit 1
fi

echo "All Helm chart CRDs are in sync with config/crd/bases/."
