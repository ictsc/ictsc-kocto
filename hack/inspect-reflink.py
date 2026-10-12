#!/usr/bin/env python3
"""Inspect shared physical extents of a dev-cluster Challenge without writing data."""

import argparse
import json
import re
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--context", required=True)
parser.add_argument("namespace")
parser.add_argument("challenge")
args = parser.parse_args()


def kubectl(*arguments):
    return subprocess.check_output(
        ["kubectl", "--context=" + args.context, *arguments], text=True
    )


def get(kind, name, namespace=None):
    scope = ["-n", namespace] if namespace else []
    return json.loads(kubectl(*scope, "get", kind, name, "-o", "json"))


def pvc_file(reference):
    pvc = get("pvc", reference["name"], args.namespace)
    if reference.get("uid") and pvc["metadata"]["uid"] != reference["uid"]:
        raise SystemExit("PVC UID changed")
    pv = get("pv", pvc["spec"]["volumeName"])
    if pv["spec"]["csi"]["driver"] != "hostpath.csi.k8s.io":
        raise SystemExit("This inspection is only for the development hostpath driver")
    return data_file(pv["spec"]["csi"]["volumeHandle"])


def data_file(handle, snapshot=False):
    if not re.fullmatch(r"[0-9a-f-]{36}", handle):
        raise SystemExit("Unexpected hostpath handle")
    return "/csi-data-dir/" + handle + (".snap" if snapshot else "") + "/disk.img"


challenge = get("problemchallenge", args.challenge, args.namespace)
source = challenge["status"]["sourceProblem"]
problem = get("problem", source["name"], args.namespace)
if problem["metadata"]["uid"] != source["uid"]:
    raise SystemExit("Source Problem UID changed")
files = {"author": pvc_file({"name": problem["status"]["diskRef"]["diskName"]})}
ref = challenge["status"]["snapshotRef"]["volumeSnapshotContentRef"]
content = get("volumesnapshotcontent", ref["name"])
if content["metadata"]["uid"] != ref["uid"]:
    raise SystemExit("SnapshotContent UID changed")
files["snapshot"] = data_file(content["status"]["snapshotHandle"], snapshot=True)
for team in challenge["status"]["teams"]:
    files["team:" + team["id"]] = pvc_file(team["pvcRef"])

# xfs_io reports physical ranges in 512-byte units. The union counts shared
# data blocks only once; it excludes filesystem metadata and other volumes.
pattern = re.compile(
    r"^\s*\d+:\s+\[\d+\.\.\d+\]:\s+(\d+)\.\.(\d+)\s+(\d+)\s+(0x[0-9a-fA-F]+)\s*$"
)
all_extents = []
result = {"files": {}}
for label, path in files.items():
    output = kubectl(
        "-n", "kocto-storage", "exec", "csi-hostpathplugin-0", "-c", "hostpath",
        "--", "xfs_io", "-r", "-c", "fiemap -v", path,
    )
    extents = []
    shared = 0
    for line in output.splitlines():
        match = pattern.match(line)
        if not match:
            continue
        start, end, length = map(int, match.group(1, 2, 3))
        flags = int(match[4], 16)
        if flags & (0x2 | 0x4):
            raise SystemExit("Extent is unknown or delayed; sync the guest disks first")
        extents.append((start, end + 1))
        if flags & 0x2000:
            shared += length * 512
    if not extents:
        raise SystemExit("No physical extents parsed: " + output)
    allocated = sum(end - start for start, end in extents) * 512
    result["files"][label] = {"allocated_bytes": allocated, "shared_bytes": shared}
    all_extents.extend(extents)

merged = []
for start, end in sorted(all_extents):
    if merged and start <= merged[-1][1]:
        merged[-1][1] = max(merged[-1][1], end)
    else:
        merged.append([start, end])
result["unique_data_bytes"] = sum(end - start for start, end in merged) * 512
result["sum_allocated_bytes"] = sum(f["allocated_bytes"] for f in result["files"].values())
print(json.dumps(result, indent=2))
