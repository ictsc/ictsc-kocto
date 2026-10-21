#!/usr/bin/env bash
# Gate for the Dependabot auto-merge workflow.
#
# Usage: hack/dependabot-automerge-gate.sh <pull-request-diff-file> [head-branch]
#
# The caller must verify the Dependabot author and successful CI for the PR's
# current head before invoking this script. Security-only groups bypass the
# version restrictions below. These branch names come from the security-updates
# groups in .github/dependabot.yml; labels and PR titles are not trusted.
#
# Exit 0 when the update may be merged automatically, exit 1 when a human
# should review it. For ordinary updates, the decision only looks at go.mod
# and .github/workflows/ hunks and refuses coordinated changes:
#
#   * k8s.io/* and sigs.k8s.io/* move together with the Makefile tool pins
#     (controller-tools, envtest's Kubernetes version), so they are reviewed.
#   * major version updates are reviewed, for Go modules and for actions.
set -uo pipefail

case "${2:-}" in
  dependabot/go_modules/security-updates-*|dependabot/github_actions/security-updates-*)
    echo "Security update: ${2}"
    exit 0
    ;;
esac

awk '
function major(v,   p) { sub(/^v/, "", v); split(v, p, "."); return p[1] + 0 }

/^\+\+\+ b\// { file = substr($0, 7); next }
/^[-+][-+][-+]/ { next }

file == "go.mod" && /^[-+]/ {
  sign = substr($0, 1, 1)
  rest = substr($0, 2); sub(/^[ \t]+/, "", rest)
  n = split(rest, f, /[ \t]+/)
  if (n >= 2 && f[2] ~ /^v?[0-9]+\.[0-9]/) {
    if (sign == "-") { oldm[f[1]] = f[2] } else { newm[f[1]] = f[2] }
  }
  next
}

file ~ /^\.github\/workflows\// && /^[-+][ \t]+uses:/ {
  sign = substr($0, 1, 1)
  rest = substr($0, 2); sub(/^[ \t]+/, "", rest); sub(/^uses:[ \t]*/, "", rest)
  n = split(rest, f, "@")
  if (n >= 2) { if (sign == "-") { olda[f[1]] = f[2] } else { newa[f[1]] = f[2] } }
  next
}

END {
  bad = 0
  for (m in newm) {
    if (m ~ /^(k8s\.io|sigs\.k8s\.io)\//) { print "kubernetes dependency: " m; bad = 1; continue }
    if (m in oldm && major(newm[m]) > major(oldm[m])) {
      print "major update: " m " " oldm[m] " -> " newm[m]; bad = 1
    }
  }
  for (a in newa) {
    if (a in olda && major(newa[a]) > major(olda[a])) {
      print "major update: " a " " olda[a] " -> " newa[a]; bad = 1
    }
  }
  exit bad ? 1 : 0
}
' "$1"
