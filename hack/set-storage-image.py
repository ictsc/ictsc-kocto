#!/usr/bin/env python3
"""Pin the development CSI image to the source commit in GitOps output."""

import re
import sys

if len(sys.argv) != 2:
    raise SystemExit("Usage: set-storage-image.py <image>")
if not re.fullmatch(r"ghcr\.io/ictsc/ictsc-kocto:storage-[0-9a-f]{40}", sys.argv[1]):
    raise SystemExit("Storage image must be pinned to a source commit")

manifest = sys.stdin.read()
manifest, count = re.subn(
    r"(?m)^(\s*image: )registry\.k8s\.io/sig-storage/hostpathplugin:v1\.18\.0$",
    lambda match: match.group(1) + sys.argv[1],
    manifest,
)
if count != 1:
    raise SystemExit(f"Expected one hostpath image, found {count}")
sys.stdout.write(manifest)
