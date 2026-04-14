#!/usr/bin/env bash
# check-helm-crds.sh — verify that every CRD in charts/aip-k8s/crds/
# matches the canonical CRD in config/crd/bases/.
#
# Detects two classes of drift:
#   1. Content drift  — canonical CRD changed without re-running sync-helm-crds.sh
#   2. Extra CRDs — chart contains a CRD not present in config/crd/bases/
#
# Exits 1 if any drift is detected; prints a diff for each drifted file.
# Run with `make helm-crds-check`.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASES="$REPO_ROOT/config/crd/bases"
CHART="$REPO_ROOT/charts/aip-k8s/crds"

fail=0

# Fail fast if the source tree is missing — nullglob would silently produce zero
# iterations, causing the script to exit 0 with "all in sync" on a broken checkout.
if [ ! -d "$BASES" ]; then
    echo "ERROR: $BASES does not exist — run 'make manifests' first." >&2
    exit 1
fi
if [ ! -d "$CHART" ]; then
    echo "ERROR: $CHART does not exist — run 'make sync-helm-crds' first." >&2
    exit 1
fi

# nullglob prevents literal '*.yaml' from being iterated when a directory is empty,
# which would produce false "MISSING: *.yaml" CI failures.
shopt -s nullglob

bases_files=("$BASES"/*.yaml)
if [ "${#bases_files[@]}" -eq 0 ]; then
    echo "ERROR: no *.yaml files found in $BASES — run 'make manifests' first." >&2
    exit 1
fi

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

    if ! diff -u "$src" "$dst" > /dev/null 2>&1; then
        echo "DRIFT: $name"
        diff -u "$src" "$dst" || true
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

shopt -u nullglob

if [ "$fail" -ne 0 ]; then
    echo "Helm chart CRDs are out of sync with config/crd/bases/."
    echo "Run 'make sync-helm-crds' (after 'make manifests') to fix."
    exit 1
fi

echo "All Helm chart CRDs are in sync with config/crd/bases/."
