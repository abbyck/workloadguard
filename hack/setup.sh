#!/usr/bin/env bash
# Create the workloadguard kind cluster if it doesn't already exist.
set -euo pipefail

CLUSTER=workloadguard
HACK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "kind cluster '$CLUSTER' already exists"
else
  kind create cluster --config "$HACK_DIR/kind-config.yaml" --wait 120s
fi

kubectl --context "kind-$CLUSTER" get nodes -o wide
