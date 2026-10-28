#!/usr/bin/env bash
# Runs inside the builder image without mounting the raw filesystem.
set -euo pipefail
test "$(id -u)" = 107
test ! -e /var/run/docker.sock
test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
grep -Eq '^CapEff:[[:space:]]+0+$' /proc/self/status
grep -Eq '^NoNewPrivs:[[:space:]]+1$' /proc/self/status
for tool in git skopeo yq jq mkfs.ext4 qemu-img e2fsck debugfs dumpe2fs; do command -v "$tool"; done
test "$(stat -c %u:%g /disk/disk.img)" = 107:107
test "$(stat -c %a /disk/disk.img)" = 640
qemu-img info --output=json /disk/disk.img | tee /tmp/raw-info.json
jq -e '.format == "raw" and .["virtual-size"] > 0 and .["virtual-size"] <= 6442450944' /tmp/raw-info.json
e2fsck -fn /disk/disk.img
dumpe2fs -h /disk/disk.img 2>/dev/null | tee /tmp/ext4-info.txt
grep -Eq '^Filesystem volume name:[[:space:]]+KOCTO-BUNDLE$' /tmp/ext4-info.txt
grep -Eq '^Filesystem features:.*extent' /tmp/ext4-info.txt
mkdir -p /tmp/bundle-inspection
# Do not restore ownership of filesystem root/lost+found (UID 0) as UID 107.
# Inspect only the contract's directories and regular metadata files.
for directory in lab images; do
    debugfs -R "rdump /$directory /tmp/bundle-inspection" /disk/disk.img
done
for metadata in manifest.sha256 bundle.id; do
    debugfs -R "dump /$metadata /tmp/bundle-inspection/$metadata" /disk/disk.img
done
cd /tmp/bundle-inspection
test -f lab/topology.clab.yml
test -d images
sha256sum --check manifest.sha256
test "$(sha256sum manifest.sha256 | cut -d' ' -f1)" = "$(cat bundle.id)"
# shellcheck disable=SC2016 # $nodes is a jq variable, not a shell variable.
yq -e --argjson nodes "${BUNDLE_TEST_EXPECTED_NODES:-2}" \
    '(.topology.nodes | length) == $nodes' lab/topology.clab.yml
yq -r '[.topology.nodes[].image] | unique | .[]' lab/topology.clab.yml > /tmp/bundle-image-references
mapfile -t references < /tmp/bundle-image-references
yq -r '[.topology.defaults, (.topology.kinds // {} | .[]),
    (.topology.groups // {} | .[]), .topology.nodes[]] |
    [.[] | .["env-files"] // [] | .[]] | unique | .[]' lab/topology.clab.yml > /tmp/bundle-env-files
mapfile -t env_files < /tmp/bundle-env-files
for env_file in "${env_files[@]}"; do
    case "$env_file" in
        /opt/kocto/lab/*) test -f "lab/${env_file#/opt/kocto/lab/}" ;;
        *) echo "Unrebased env-file: $env_file" >&2; exit 1 ;;
    esac
done
archives=(images/*.tar)
test "${#archives[@]}" -eq "${BUNDLE_TEST_EXPECTED_IMAGES:-1}"
test "${#references[@]}" -eq "${BUNDLE_TEST_EXPECTED_IMAGES:-1}"
tags=/tmp/bundle-image-tags
: > "$tags"
for archive in images/*.tar; do
    directory=$(mktemp -d /tmp/image-inspection.XXXXXX)
    tar -xf "$archive" -C "$directory" manifest.json
    config=$(jq -r '.[0].Config' "$directory/manifest.json")
    jq -r '.[].RepoTags[]' "$directory/manifest.json" >> "$tags"
    tar -xf "$archive" -C "$directory" "$config"
    jq -e '.architecture == "amd64" and .os == "linux"' "$directory/$config"
done
for reference in "${references[@]}"; do
    # Docker archives normalize short names such as alpine:3.20.
    grep -Fxq -- "$reference" "$tags" || grep -Fxq -- "docker.io/library/$reference" "$tags" \
        || grep -Fxq -- "docker.io/$reference" "$tags"
done
free_blocks=$(awk '/^Free blocks:/ {print $3}' /tmp/ext4-info.txt)
all_blocks=$(awk '/^Block count:/ {print $3}' /tmp/ext4-info.txt)
test "$free_blocks" -gt 0
printf 'Validated raw ext4 bundle: id=%s owner=107:107 free_blocks=%s total_blocks=%s\n' "$(cat bundle.id)" "$free_blocks" "$all_blocks"
