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

// Package bundle prepares an offline lab for the kocto-clab-image bundle script.
package bundle

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var repositoryPathPattern = regexp.MustCompile(`^/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Validate deliberately supports only public GitHub HTTPS repositories initially.
// No credentials, redirects, submodules, LFS, templates or Ansible are executed.
func Validate(source labv1alpha1.ProblemSourceSpec) error {
	u, err := url.Parse(source.Repository)
	if err != nil || len(source.Repository) > 2048 || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !repositoryPathPattern.MatchString(u.Path) {
		return fmt.Errorf("source.repository must be a public https://github.com/owner/repository URL without credentials or parameters")
	}
	if !commitPattern.MatchString(source.CommitHash) {
		return fmt.Errorf("source.commitHash must be a complete lowercase 40-character commit SHA")
	}
	if !relativePath(source.Directory, true) || !relativePath(source.TopologyFile, false) {
		return fmt.Errorf("source paths must be clean relative paths without parent traversal")
	}
	if source.AnsiblePlaybook != nil {
		return fmt.Errorf("source.ansiblePlaybook is not supported by the offline bundle builder")
	}
	return nil
}

func relativePath(value string, empty bool) bool {
	if value == "" {
		return empty
	}
	if len(value) > 1024 || strings.ContainsAny(value, "\\\x00\n\r") || path.IsAbs(value) || path.Clean(value) != value || value == ".." || strings.HasPrefix(value, "../") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == ".git" {
			return false
		}
	}
	return true
}

// SourceHash fixes the initial input independently of mutable builder settings.
func SourceHash(source labv1alpha1.ProblemSourceSpec) string {
	data, _ := json.Marshal(source)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
