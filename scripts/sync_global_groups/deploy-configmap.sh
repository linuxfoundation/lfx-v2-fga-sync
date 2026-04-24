#!/usr/bin/env bash
# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT

# deploy-configmap.sh wraps scripts/sync_global_groups/main.go as a Kubernetes
# ConfigMap and applies it to the cluster. The ConfigMap is intended for use
# with the accompanying cronjob.yaml.
#
# Usage: ./deploy-configmap.sh [namespace]
#   namespace  Target Kubernetes namespace (default: lfx)

set -euo pipefail

NAMESPACE="${1:-lfx}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_FILE="${SCRIPT_DIR}/main.go"
CONFIGMAP_NAME="sync-global-groups-ad-hoc"

# Support KUBECTL_CONTEXT env var to select a kubeconfig context.
KUBECTL_ARGS=()
if [[ -n "${KUBECTL_CONTEXT:-}" ]]; then
  KUBECTL_ARGS+=(--context "${KUBECTL_CONTEXT}")
fi

echo "Deploying ConfigMap '${CONFIGMAP_NAME}' to namespace '${NAMESPACE}'..."

kubectl "${KUBECTL_ARGS[@]}" create configmap "${CONFIGMAP_NAME}" \
  --from-file=main.go="${SCRIPT_FILE}" \
  --namespace="${NAMESPACE}" \
  --dry-run=client -o yaml \
  | kubectl "${KUBECTL_ARGS[@]}" apply -f -

echo "Done."
