/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bundle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestStageEnvFiles(t *testing.T) {
	for _, topology := range []string{"lab.yml", "nested/lab.yml"} {
		t.Run(topology, func(t *testing.T) {
			reference := "shared/node.env"
			if strings.Contains(topology, "/") {
				reference = "../" + reference
			}
			data := fmt.Sprintf(`name: env-files
topology:
  defaults:
    env-files: [%[1]s]
  kinds:
    linux:
      env-files: [%[1]s]
  groups:
    selected:
      env-files: [%[1]s]
  nodes:
    n1:
      kind: linux
      group: selected
      image: alpine:3.20
      env-files: [%[1]s]
      labels: {env-files: arbitrary-label}
`, reference)
			source, destination := stageFixture(t, data)
			if err := os.WriteFile(filepath.Join(source, topology), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			const contents = "REVIEW_VALUE=offline\n"
			if err := os.WriteFile(filepath.Join(source, "shared/node.env"), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Stage(source, topology, destination); err != nil {
				t.Fatal(err)
			}
			staged, err := os.ReadFile(filepath.Join(destination, "topology.clab.yml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(staged), "/opt/kocto/lab/shared/node.env") != 4 {
				t.Fatalf("env-files lost their original base directory: %s", staged)
			}
			if !strings.Contains(string(staged), "env-files: arbitrary-label") {
				t.Fatalf("label was changed: %s", staged)
			}
			copied, err := os.ReadFile(filepath.Join(destination, "shared/node.env"))
			if err != nil || string(copied) != contents {
				t.Fatalf("env file contents changed: %q, %v", copied, err)
			}
		})
	}
}

func TestStageRejectsInvalidEnvFiles(t *testing.T) {
	for _, test := range []struct{ name, value, message string }{
		{"scalar", "configs/node.cfg", "env-files must be a list"},
		{"mapping", "{file: configs/node.cfg}", "env-files must be a list"},
		{"non-string", "[configs/node.cfg, 7]", "entries must be strings"},
		{"missing", "[absent.env]", "env-files: file reference"},
		{"directory", "[configs]", "regular file"},
		{"empty path", "['']", "regular file"},
		{"absolute", "[/etc/os-release]", "local relative"},
		{"outside", "[../../outside.env]", "leaves source.directory"},
		{"remote", "[https://example.invalid/node.env]", "local relative"},
		{"variable", "['${ENV_FILE}']", "local relative"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, destination := stageFixture(t, "name: invalid-env\ntopology:\n  nodes:\n    n1:\n      kind: linux\n      image: alpine:3.20\n      env-files: "+test.value+"\n")
			if err := Stage(source, "nested/lab.yml", destination); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("expected %q, got %v", test.message, err)
			}
		})
	}
}

func TestStageRejectsOfflineIncompatibleDefinitions(t *testing.T) {
	for _, scope := range []string{"defaults", "kinds", "groups", "nodes"} {
		for _, test := range []struct {
			name, key, message string
			value              any
		}{
			{"always", "image-pull-policy", "Always is incompatible", "Always"},
			{"normalized always", "image-pull-policy", "Always is incompatible", " always "},
			{"unknown policy", "image-pull-policy", "unsupported image-pull-policy", "Sometimes"},
			{"typed policy", "image-pull-policy", "must be a string", true},
			{"podman", "runtime", "only the docker runtime", "podman"},
			{"unknown runtime", "runtime", "only the docker runtime", "Docker"},
			{"typed runtime", "runtime", "only the docker runtime", 7},
			{"digest", "image", "digest image references", "alpine@sha256:" + strings.Repeat("a", 64)},
		} {
			t.Run(scope+"/"+test.name, func(t *testing.T) {
				node := map[string]any{"kind": "linux", "group": "selected", "image": "alpine:3.20", "image-pull-policy": "Never", "runtime": "docker"}
				definition := map[string]any{test.key: test.value}
				topology := map[string]any{"nodes": map[string]any{"n1": node}}
				switch scope {
				case "defaults":
					topology[scope] = definition
				case "nodes":
					node[test.key] = test.value
				default:
					name := "linux"
					if scope == "groups" {
						name = "selected"
					}
					topology[scope] = map[string]any{name: definition}
				}
				data, err := yaml.Marshal(map[string]any{"name": "offline-settings", "topology": topology})
				if err != nil {
					t.Fatal(err)
				}
				source, destination := stageFixture(t, string(data))
				if err := Stage(source, "nested/lab.yml", destination); err == nil || !strings.Contains(err.Error(), test.message) {
					t.Fatalf("expected %q, got %v", test.message, err)
				}
			})
		}
	}
}

func TestStageAcceptsOfflinePullPolicies(t *testing.T) {
	for _, policy := range []string{"", "Never", "IfNotPresent", " ifnotpresent "} {
		t.Run(policy, func(t *testing.T) {
			source, destination := stageFixture(t, fmt.Sprintf("name: offline\ntopology:\n  defaults: {image-pull-policy: %q, runtime: docker}\n  nodes:\n    n1: {kind: linux, image: alpine:3.20}\n", policy))
			if err := Stage(source, "nested/lab.yml", destination); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStageMixedImageFixture(t *testing.T) {
	source, err := filepath.EvalSymlinks("../../test/fixtures/offline-lab-multi")
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "stage")
	if err := Stage(source, "topologies/three-nodes.yml", destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "topology.clab.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"alpine:3.20", "busybox:1.37", "/opt/kocto/lab/configs:/kocto:ro"} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("mixed-image fixture lost %q: %s", expected, data)
		}
	}
	if strings.Count(string(data), "/opt/kocto/lab/env/") != 4 {
		t.Fatalf("mixed-image env-files were not all rebased: %s", data)
	}
}
