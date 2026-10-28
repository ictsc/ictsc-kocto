#!/usr/bin/env bash
# Exercise the actual amd64 image; no cluster, socket mount, or registry push.
set -euo pipefail
: "${BUILDER_IMAGE:?set BUILDER_IMAGE to a locally loaded builder image}"
: "${BUNDLE_TEST_REPOSITORY:?set a public GitHub source repository}"
: "${BUNDLE_TEST_COMMIT:?set the exact published commit to test}"
source_directory=${BUNDLE_TEST_DIRECTORY:-test/fixtures/offline-lab}
topology=${BUNDLE_TEST_TOPOLOGY:-topologies/two-nodes.yml}
expected_nodes=${BUNDLE_TEST_EXPECTED_NODES:-2}
expected_images=${BUNDLE_TEST_EXPECTED_IMAGES:-1}
repo_root=$(cd "$(dirname "$0")/.." && pwd)
run_directory=$(mktemp -d)
run_id=$(basename "$run_directory")
disk_volume="kocto-bundle-disk-${run_id}"
work_volume="kocto-bundle-work-${run_id}"
cleanup() {
    docker volume rm "$disk_volume" "$work_volume" >/dev/null 2>&1 || true
    rmdir "$run_directory"
}
trap cleanup EXIT
docker volume create "$disk_volume" >/dev/null
docker volume create "$work_volume" >/dev/null
# Simulate kubelet/CSI fsGroup ownership on empty storage. Only this initialization
# uses root and CHOWN; the builder and filesystem inspection both run as UID 107.
docker run --rm --platform linux/amd64 --network none --user 0:0 --read-only \
    --cap-drop ALL --cap-add CHOWN --security-opt no-new-privileges \
    -v "$disk_volume:/disk" -v "$work_volume:/tmp" \
    --entrypoint chown "$BUILDER_IMAGE" 107:107 /disk /tmp
docker run --rm --platform linux/amd64 --user 107:107 --read-only \
    --cap-drop ALL --security-opt no-new-privileges --memory 2g --cpus 2 \
    -v "$disk_volume:/disk" -v "$work_volume:/tmp" -v "$work_volume:/var/tmp" "$BUILDER_IMAGE" \
    "--repository=$BUNDLE_TEST_REPOSITORY" "--commit=$BUNDLE_TEST_COMMIT" \
    "--directory=$source_directory" "--topology=$topology"
docker run --rm --platform linux/amd64 --user 107:107 --read-only --network none \
    --cap-drop ALL --security-opt no-new-privileges --memory 2g --cpus 2 \
    -e "BUNDLE_TEST_EXPECTED_NODES=$expected_nodes" -e "BUNDLE_TEST_EXPECTED_IMAGES=$expected_images" \
    -v "$disk_volume:/disk:ro" -v "$work_volume:/tmp" \
    --mount "type=bind,source=$repo_root/hack/inspect-bundle.sh,target=/inspect-bundle.sh,readonly" \
    --entrypoint bash "$BUILDER_IMAGE" /inspect-bundle.sh
