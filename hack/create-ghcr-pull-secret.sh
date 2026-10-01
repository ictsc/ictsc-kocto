#!/usr/bin/env bash

set -euo pipefail

: "${DEV_CONTEXT:?Set DEV_CONTEXT to the kubeconfig context to use.}"
: "${DEV_IMAGE_PULL_SECRET:?Set DEV_IMAGE_PULL_SECRET to the Secret name to create.}"
: "${GHCR_TOKEN:?Set GHCR_TOKEN to a token with read:packages.}"

kubectl_bin="${KUBECTL:-kubectl}"
ghcr_username="${GHCR_USERNAME:-$(gh api user --jq .login)}"
export GHCR_USERNAME="$ghcr_username"
docker_config="$(mktemp -d)"
trap 'rm -rf "$docker_config"' EXIT
chmod 700 "$docker_config"

python3 - "$docker_config/config.json" <<'PY'
import base64
import json
import os
import sys

auth = base64.b64encode(
    f"{os.environ['GHCR_USERNAME']}:{os.environ['GHCR_TOKEN']}".encode()
).decode()
with open(sys.argv[1], "w", encoding="utf-8") as config:
    json.dump({"auths": {"ghcr.io": {"auth": auth}}}, config)
PY
chmod 600 "$docker_config/config.json"
"$kubectl_bin" --context="$DEV_CONTEXT" create namespace ictsc-kocto-system --dry-run=client -o yaml \
	| "$kubectl_bin" --context="$DEV_CONTEXT" apply -f -
"$kubectl_bin" --context="$DEV_CONTEXT" create secret generic "$DEV_IMAGE_PULL_SECRET" \
	--namespace=ictsc-kocto-system \
	--type=kubernetes.io/dockerconfigjson \
	--from-file=.dockerconfigjson="$docker_config/config.json" \
	--dry-run=client -o yaml \
	| "$kubectl_bin" --context="$DEV_CONTEXT" apply -f -
