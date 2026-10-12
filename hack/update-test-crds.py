#!/usr/bin/env python3
"""Regenerate pinned external CRDs from upstream definitions, without installing operators."""

from pathlib import Path
import subprocess
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
SOURCES = {
    "kubevirt_crds.go": "https://raw.githubusercontent.com/kubevirt/kubevirt/v1.9.0/pkg/virt-operator/resource/generate/components/crds.go",
    "validations.go": "https://raw.githubusercontent.com/kubevirt/kubevirt/v1.9.0/pkg/virt-operator/resource/generate/components/validations_generated.go",
    "cdi_crds.go": "https://raw.githubusercontent.com/kubevirt/containerized-data-importer/v1.64.0/pkg/operator/resources/crds_generated.go",
}

MAIN = r'''
package main
import (
 "os"
 "path/filepath"
 "sigs.k8s.io/yaml"
)
func main() {
 dest := "test/fixtures/crds"
 if err := os.MkdirAll(dest,0755); err != nil { panic(err) }
 for _, kind := range []string{"virtualmachines","virtualmachineinstances","virtualmachinesnapshots","virtualmachinesnapshotcontents","virtualmachinerestores","virtualmachineclones"} {
  crd, err := NewVirtualMachineCrd()
  switch kind {
  case "virtualmachineinstances": crd, err = NewVirtualMachineInstanceCrd()
  case "virtualmachinesnapshots": crd, err = NewVirtualMachineSnapshotCrd()
  case "virtualmachinesnapshotcontents": crd, err = NewVirtualMachineSnapshotContentCrd()
  case "virtualmachinerestores": crd, err = NewVirtualMachineRestoreCrd()
  case "virtualmachineclones": crd, err = NewVirtualMachineCloneCrd()
  }
  if err != nil { panic(err) }
  data, err := yaml.Marshal(crd); if err != nil { panic(err) }
  prefix := []byte("# Generated from kubevirt/kubevirt v1.9.0 official CRD definitions\n")
  if err := os.WriteFile(filepath.Join(dest,kind+".yaml"),append(prefix,data...),0644); err != nil { panic(err) }
 }
 data, ok := CDICRDs["datavolume"]
 if !ok { panic("DataVolume CRD key missing") }
 if err := os.WriteFile(filepath.Join(dest,"datavolumes.yaml"),[]byte("# Source: kubevirt/containerized-data-importer v1.64.0 pkg/operator/resources/crds_generated.go\n"+data),0644); err != nil { panic(err) }
}
'''

def main():
    with tempfile.TemporaryDirectory(prefix="kocto-external-crds-") as directory:
        directory = Path(directory)
        for name, url in SOURCES.items():
            with urllib.request.urlopen(url, timeout=60) as response:
                source = response.read().decode()
            source = source.replace("package components", "package main").replace("package resources", "package main")
            # Only the pointer helper's import/name changes; CRD definitions and schemas remain upstream.
            source = source.replace('"kubevirt.io/kubevirt/pkg/pointer"', '"k8s.io/utils/ptr"').replace("pointer.P(", "ptr.To(")
            (directory / name).write_text(source)
        (directory / "main.go").write_text(MAIN)
        subprocess.run(["go", "run", *map(str, sorted(directory.glob("*.go")))], cwd=ROOT, check=True)
        for kind in ("volumesnapshots", "volumesnapshotcontents", "volumesnapshotclasses"):
            url = f"https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/v8.6.0/client/config/crd/snapshot.storage.k8s.io_{kind}.yaml"
            with urllib.request.urlopen(url, timeout=60) as response:
                data = response.read()
            (ROOT / "test/fixtures/crds" / f"{kind}.yaml").write_bytes(b"# Source: kubernetes-csi/external-snapshotter v8.6.0\n" + data)


if __name__ == "__main__":
    main()
