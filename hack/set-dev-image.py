#!/usr/bin/env python3

import re
import sys


if len(sys.argv) not in (2, 3, 4, 5):
    raise SystemExit("usage: set-dev-image.py ghcr.io/ictsc/ictsc-kocto:<tag> [image-pull-secret] [bundle-builder-image] [bundle-builder-pull-secret]")

image = sys.argv[1]
if not re.fullmatch(r"ghcr\.io/ictsc/ictsc-kocto:[A-Za-z0-9_][A-Za-z0-9_.-]*", image):
    raise SystemExit("DEV_IMAGE must be a tagged image under ghcr.io/ictsc/ictsc-kocto")

pull_secret = sys.argv[2] if len(sys.argv) >= 3 else ""
if pull_secret and not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", pull_secret):
    raise SystemExit("DEV_IMAGE_PULL_SECRET must be a Kubernetes Secret name")

builder_image = sys.argv[3] if len(sys.argv) >= 4 else ""
builder_secret = sys.argv[4] if len(sys.argv) >= 5 else ""
if builder_image:
    tag = image.rsplit(":", 1)[1]
    if not (builder_image == f"ghcr.io/ictsc/ictsc-kocto-bundle-builder:{tag}" or
            re.fullmatch(r"ghcr\.io/ictsc/ictsc-kocto-bundle-builder@sha256:[0-9a-f]{64}", builder_image)):
        raise SystemExit("BUNDLE_IMAGE must use the controller tag or a builder digest")
if builder_secret and (not builder_image or not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", builder_secret)):
    raise SystemExit("builder pull Secret requires a builder image and a valid Secret name")

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

pattern = re.compile(r"^(?P<indent>[ ]*)serviceAccountName: ictsc-kocto-controller-manager$", re.MULTILINE)
manifest, replacements = pattern.subn(
    lambda match: (
        (
            f"{match.group('indent')}imagePullSecrets:\n"
            f"{match.group('indent')}- name: {pull_secret}\n"
        )
        if pull_secret
        else ""
    )
    + f"{match.group('indent')}serviceAccountName: ictsc-kocto-controller-manager",
    manifest,
)
if replacements != 1:
    raise SystemExit(f"expected one controller manager service account, found {replacements}")

if builder_image:
    # Anchor on the unique health argument in the manager container, rather than
    # adding a floating builder tag to the base Kustomize configuration.
    pattern = re.compile(r"^(?P<indent>[ ]*)- --health-probe-bind-address=:8081$", re.MULTILINE)
    manifest, replacements = pattern.subn(
        lambda match: match.group(0) + f"\n{match.group('indent')}- --bundle-builder-image={builder_image}"
        + (f"\n{match.group('indent')}- --bundle-builder-pull-secret={builder_secret}" if builder_secret else ""),
        manifest,
    )
    if replacements != 1:
        raise SystemExit(f"expected one manager health argument, found {replacements}")

sys.stdout.write(manifest)
