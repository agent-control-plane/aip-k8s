#!/usr/bin/env bash
# sync-helm-crds.sh — copy every CRD from config/crd/bases/ into
# charts/aip-k8s/crds/.
#
# Run after `make manifests` any time the Go types change.
# Counterpart: scripts/check-helm-crds.sh validates the result.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASES="$REPO_ROOT/config/crd/bases"
CHART="$REPO_ROOT/charts/aip-k8s/crds"

if [ ! -d "$BASES" ]; then
    echo "ERROR: $BASES does not exist — run 'make manifests' first." >&2
    exit 1
fi

mkdir -p "$CHART"

# nullglob prevents literal '*.yaml' from being iterated when a directory is empty.
shopt -s nullglob

# Remove CRDs that no longer exist in config/crd/bases/.
for dst in "$CHART"/*.yaml; do
    name="$(basename "$dst")"
    if [ ! -f "$BASES/$name" ]; then
        rm "$dst"
        echo "removed stale $name"
    fi
done

for src in "$BASES"/*.yaml; do
    name="$(basename "$src")"
    dst="$CHART/$name"

    cp "$src" "$dst"
    echo "synced $name"
done

shopt -u nullglob
echo "All Helm chart CRDs synced from config/crd/bases/."
