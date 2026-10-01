#!/usr/bin/env bash

set -euo pipefail

: "${DEV_CONTEXT:?Set DEV_CONTEXT to the kubeconfig context to use.}"
: "${ARGOCD_REPO_SSH_KEY:?Set ARGOCD_REPO_SSH_KEY to the read-only GitHub deploy key path.}"

if [[ ! -r "$ARGOCD_REPO_SSH_KEY" ]]; then
	echo "Cannot read Argo CD GitHub deploy key: $ARGOCD_REPO_SSH_KEY" >&2
	exit 1
fi

kubectl_bin="${KUBECTL:-kubectl}"
"$kubectl_bin" --context="$DEV_CONTEXT" create secret generic ictsc-kocto-repo \
	--namespace=argocd \
	--from-literal=type=git \
	--from-literal=url=git@github.com:ictsc/ictsc-kocto.git \
	--from-file=sshPrivateKey="$ARGOCD_REPO_SSH_KEY" \
	--dry-run=client -o yaml \
	| "$kubectl_bin" --context="$DEV_CONTEXT" apply -f -
"$kubectl_bin" --context="$DEV_CONTEXT" label secret ictsc-kocto-repo \
	--namespace=argocd argocd.argoproj.io/secret-type=repository --overwrite
