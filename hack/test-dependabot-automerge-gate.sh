#!/usr/bin/env bash
set -euo pipefail

gate="$(dirname "$0")/dependabot-automerge-gate.sh"
diff_file="$(mktemp)"
trap 'rm -f "$diff_file"' EXIT

check() {
  local expected="$1" branch="$2" actual=0
  bash "$gate" "$diff_file" "$branch" > /dev/null || actual=$?
  if [ "$actual" -ne "$expected" ]; then
    echo "Unexpected gate status for $branch: got $actual, expected $expected" >&2
    exit 1
  fi
}

cat > "$diff_file" <<'DIFF'
diff --git a/go.mod b/go.mod
--- a/go.mod
+++ b/go.mod
@@ -1 +1 @@
- k8s.io/api v0.37.0
+ k8s.io/api v0.37.1
DIFF
check 1 dependabot/go_modules/kubernetes-1234567890
check 1 dependabot/go_modules/not-security-updates-1234567890
check 1 feature/security-updates-1234567890
check 1 dependabot/go_modules/security-updates
check 1 ""
check 0 dependabot/go_modules/security-updates-1234567890

cat > "$diff_file" <<'DIFF'
diff --git a/go.mod b/go.mod
--- a/go.mod
+++ b/go.mod
@@ -1 +1 @@
- example.com/legacy v1.0.0
+ example.com/legacy v2.0.0+incompatible
DIFF
check 1 dependabot/go_modules/legacy-2.0.0
check 0 dependabot/go_modules/security-updates-1234567890

cat > "$diff_file" <<'DIFF'
diff --git a/.github/workflows/ci.yaml b/.github/workflows/ci.yaml
--- a/.github/workflows/ci.yaml
+++ b/.github/workflows/ci.yaml
@@ -1 +1 @@
-        uses: actions/checkout@v6
+        uses: actions/checkout@v7
DIFF
check 1 dependabot/github_actions/github-actions-1234567890
check 0 dependabot/github_actions/security-updates-1234567890

cat > "$diff_file" <<'DIFF'
diff --git a/go.mod b/go.mod
--- a/go.mod
+++ b/go.mod
@@ -1 +1 @@
- golang.org/x/net v0.57.0
+ golang.org/x/net v0.58.0
DIFF
check 0 dependabot/go_modules/go-modules-1234567890

cat > "$diff_file" <<'DIFF'
diff --git a/Dockerfile b/Dockerfile
--- a/Dockerfile
+++ b/Dockerfile
@@ -1 +1 @@
-FROM golang:1.26@sha256:old AS builder
+FROM golang:1.26@sha256:new AS builder
DIFF
check 0 dependabot/docker/golang-1.26

echo "Dependabot auto-merge policy checks passed"
