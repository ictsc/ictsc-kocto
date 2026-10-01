#!/usr/bin/env python3

import re
import sys


if len(sys.argv) not in (2, 3):
    raise SystemExit("usage: set-dev-image.py ghcr.io/ictsc/ictsc-kocto:<tag> [image-pull-secret]")

image = sys.argv[1]
if not re.fullmatch(r"ghcr\.io/ictsc/ictsc-kocto:[A-Za-z0-9_][A-Za-z0-9_.-]*", image):
    raise SystemExit("DEV_IMAGE must be a tagged image under ghcr.io/ictsc/ictsc-kocto")

pull_secret = sys.argv[2] if len(sys.argv) == 3 else ""
if pull_secret and not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", pull_secret):
    raise SystemExit("DEV_IMAGE_PULL_SECRET must be a Kubernetes Secret name")

manifest = sys.stdin.read()
pattern = re.compile(r"^(?P<indent>[ ]*)image: controller:latest$", re.MULTILINE)
manifest, replacements = pattern.subn(
    lambda match: (
        f"{match.group('indent')}image: {image}\n"
        f"{match.group('indent')}imagePullPolicy: Always"
    ),
    manifest,
)
if replacements != 1:
    raise SystemExit(f"expected one controller image in rendered manifests, found {replacements}")

if pull_secret:
    pattern = re.compile(r"^(?P<indent>[ ]*)serviceAccountName: [a-z0-9]([-a-z0-9]*[a-z0-9])?$", re.MULTILINE)
    manifest, replacements = pattern.subn(
        lambda match: (
            f"{match.group('indent')}imagePullSecrets:\n"
            f"{match.group('indent')}- name: {pull_secret}\n"
            f"{match.group('indent')}serviceAccountName: controller-manager"
        ),
        manifest,
    )
    if replacements != 1:
        raise SystemExit(f"expected one manager service account in rendered manifests, found {replacements}")

sys.stdout.write(manifest)
