#!/usr/bin/env bash

set -euo pipefail

if [[ $# -eq 0 ]]; then
	echo "Provide a command to run with the selected Kubernetes context." >&2
	exit 2
fi

: "${DEV_CONTEXT:?Set DEV_CONTEXT to the kubeconfig context to use.}"

kubectl_bin="${KUBECTL:-kubectl}"
kubeconfig="$(mktemp)"
trap 'rm -f "$kubeconfig"' EXIT
chmod 600 "$kubeconfig"

"$kubectl_bin" config view --raw --flatten --minify --context="$DEV_CONTEXT" > "$kubeconfig"
KUBECONFIG="$kubeconfig" "$@"
