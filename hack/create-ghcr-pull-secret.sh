#!/usr/bin/env bash

set -euo pipefail

: "${DEV_CONTEXT:?Set DEV_CONTEXT to the kubeconfig context to use.}"
: "${DEV_IMAGE_PULL_SECRET:?Set DEV_IMAGE_PULL_SECRET to the Secret name to create.}"
: "${GHCR_TOKEN:?Set GHCR_TOKEN to a token with read:packages.}"

kubectl_bin="${KUBECTL:-kubectl}"
ghcr_username="${GHCR_USERNAME:-$(gh api user --jq .login)}"
docker_config="$(mktemp -d)"
trap 'rm -rf "$docker_config"' EXIT
chmod 700 "$docker_config"

printf '%s' "$GHCR_TOKEN" | docker --config "$docker_config" login ghcr.io --username "$ghcr_username" --password-stdin >/dev/null
"$kubectl_bin" --context="$DEV_CONTEXT" create namespace ictsc-kocto-system --dry-run=client -o yaml \
	| "$kubectl_bin" --context="$DEV_CONTEXT" apply -f -
"$kubectl_bin" --context="$DEV_CONTEXT" create secret generic "$DEV_IMAGE_PULL_SECRET" \
	--namespace=ictsc-kocto-system \
	--type=kubernetes.io/dockerconfigjson \
	--from-file=.dockerconfigjson="$docker_config/config.json" \
	--dry-run=client -o yaml \
	| "$kubectl_bin" --context="$DEV_CONTEXT" apply -f -
