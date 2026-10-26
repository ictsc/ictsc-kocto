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
	"os"
	"path/filepath"
	"strings"
	"testing"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	"go.yaml.in/yaml/v3"
)

func validSource() labv1alpha1.ProblemSourceSpec {
	return labv1alpha1.ProblemSourceSpec{Repository: "https://github.com/ictsc/labs.git", CommitHash: strings.Repeat("a", 40), TopologyFile: "topology.clab.yml"}
}

func TestValidate(t *testing.T) {
	if err := Validate(validSource()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*labv1alpha1.ProblemSourceSpec)
	}{
		{"credentials", func(s *labv1alpha1.ProblemSourceSpec) { s.Repository = "https://user:password@github.com/ictsc/labs" }},
		{"query", func(s *labv1alpha1.ProblemSourceSpec) { s.Repository += "?token=secret" }},
		{"nonpublic host", func(s *labv1alpha1.ProblemSourceSpec) { s.Repository = "https://127.0.0.1/ictsc/labs" }},
		{"ssh", func(s *labv1alpha1.ProblemSourceSpec) { s.Repository = "git@github.com:ictsc/labs" }},
		{"encoded path", func(s *labv1alpha1.ProblemSourceSpec) { s.Repository = "https://github.com/ictsc/%2e%2e" }},
		{"branch", func(s *labv1alpha1.ProblemSourceSpec) { s.CommitHash = "main" }},
		{"short commit", func(s *labv1alpha1.ProblemSourceSpec) { s.CommitHash = "abcdef0" }},
		{"directory traversal", func(s *labv1alpha1.ProblemSourceSpec) { s.Directory = "a/../b" }},
		{"absolute topology", func(s *labv1alpha1.ProblemSourceSpec) { s.TopologyFile = "/etc/passwd" }},
		{"parent topology", func(s *labv1alpha1.ProblemSourceSpec) { s.TopologyFile = "../lab.yml" }},
		{"newline", func(s *labv1alpha1.ProblemSourceSpec) { s.TopologyFile = "lab\n.yml" }},
		{"metadata", func(s *labv1alpha1.ProblemSourceSpec) { s.Directory = ".git" }},
		{"ansible", func(s *labv1alpha1.ProblemSourceSpec) { s.AnsiblePlaybook = new("playbook.yml") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := validSource()
			test.change(&s)
			if Validate(s) == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
	a, b := validSource(), validSource()
	b.Directory = "different"
	if SourceHash(a) == SourceHash(b) {
		t.Fatal("source identity did not change")
	}
}

func stageFixture(t *testing.T, topology string) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "stage")
	for _, dir := range []string{"nested/configs", "shared", ".git"} {
		if err := os.MkdirAll(filepath.Join(source, dir), 0750); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{"nested/lab.yml": topology, "nested/configs/node.cfg": "hostname test", "shared/message.txt": "hello", ".git/config": "must not ship"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return source, destination
}

func TestStageNestedTopology(t *testing.T) {
	source, destination := stageFixture(t, `name: test
topology:
  nodes:
    n1:
      kind: linux
      image: alpine:3.20
      labels: {license: arbitrary-label, binds: another-label}
      startup-config: configs/node.cfg
      binds: ["../shared:/shared:ro"]
`)

	if err := os.WriteFile(filepath.Join(source, "helper.sh"), []byte("#!/bin/sh\ntrue\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Stage(source, "nested/lab.yml", destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "topology.clab.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	labels := parsed["topology"].(map[string]any)["nodes"].(map[string]any)["n1"].(map[string]any)["labels"].(map[string]any)
	if labels["license"] != "arbitrary-label" || labels["binds"] != "another-label" {
		t.Fatal("labels were treated as filesystem paths")
	}
	for _, expected := range []string{"/opt/kocto/lab/nested/configs/node.cfg", "/opt/kocto/lab/shared:/shared:ro"} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("lost path %s: %s", expected, data)
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "shared/message.txt")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "helper.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Fatal("executable file lost its mode")
	}
	if _, err := os.Stat(filepath.Join(destination, ".git")); !os.IsNotExist(err) {
		t.Fatal("Git metadata copied")
	}
}

func TestStageRejectsUnsafeFilesAndReferences(t *testing.T) {
	for _, test := range []struct {
		name, extra string
		setup       func(string) error
	}{
		{name: "LFS", setup: func(root string) error {
			return os.WriteFile(filepath.Join(root, "lfs-pointer"), []byte("version https://git-lfs.github.com/spec/v1\noid sha256:abc"), 0600)
		}},
		{name: "symlink", setup: func(root string) error { return os.Symlink("/etc/passwd", filepath.Join(root, "escape")) }},
		{name: "traversing bind", extra: "    binds: [\"../../secret:/secret\"]\n"},
		{name: "host bind", extra: "    binds: [\"/etc:/etc\"]\n"},
		{name: "remote config", extra: "    startup-config: https://example.com/config\n"},
		{name: "missing config", extra: "    startup-config: absent\n"},
		{name: "ambiguous canonical", setup: func(root string) error {
			return os.WriteFile(filepath.Join(root, "topology.clab.yml"), []byte("other"), 0600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, destination := stageFixture(t, "name: test\ntopology:\n nodes:\n  n1:\n    kind: linux\n    image: alpine:3.20\n"+test.extra)
			if test.setup != nil {
				if err := test.setup(source); err != nil {
					t.Fatal(err)
				}
			}
			if Stage(source, "nested/lab.yml", destination) == nil {
				t.Fatal("unsafe input accepted")
			}
		})
	}
}

func TestStagePreservesYAMLStrings(t *testing.T) {
	source, destination := stageFixture(t, `name: strings
topology:
  nodes:
    on:
      kind: linux
      image: alpine:3.20
      env: {MODE: yes, DISABLED: off}
`)
	if err := Stage(source, "nested/lab.yml", destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "topology.clab.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := yaml.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	node := result["topology"].(map[string]any)["nodes"].(map[string]any)["on"].(map[string]any)
	env := node["env"].(map[string]any)
	if env["MODE"] != "yes" || env["DISABLED"] != "off" {
		t.Fatalf("YAML strings changed: %v", env)
	}
}

func TestStageSizeLimits(t *testing.T) {
	for _, test := range []struct {
		name, file string
		size       int64
	}{
		{"lab data", "oversized", MaxLabBytes + 1},
		{"topology", "nested/lab.yml", MaxTopologyBytes + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, destination := stageFixture(t, "name: unused")
			file, err := os.Create(filepath.Join(source, test.file))
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(test.size); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			err = Stage(source, "nested/lab.yml", destination)
			if err == nil || !strings.Contains(err.Error(), "limit") {
				t.Fatalf("size limit was not enforced: %v", err)
			}
		})
	}
}
