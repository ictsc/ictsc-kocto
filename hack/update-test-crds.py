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
 for _, kind := range []string{"virtualmachines","virtualmachineinstances"} {
  crd, err := NewVirtualMachineCrd()
  if kind == "virtualmachineinstances" { crd, err = NewVirtualMachineInstanceCrd() }
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


if __name__ == "__main__":
    main()
