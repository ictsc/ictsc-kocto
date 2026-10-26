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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	"go.yaml.in/yaml/v3"
)

const MaxDiskBytes int64 = 6 * 1024 * 1024 * 1024
const MaxLabBytes int64 = 1024 * 1024 * 1024
const MaxLabFiles = 10000
const MaxTopologyBytes int64 = 1024 * 1024

var imagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)

func run(ctx context.Context, dir, command string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, os.Stdout, os.Stderr
	// The builder has no service account token, registry credentials, or host mounts.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	return cmd.Run()
}

// Build delegates the bundle format and image collection to a pinned upstream
// script. Only the input adapter and KubeVirt storage conversion live here.
func Build(ctx context.Context, source labv1alpha1.ProblemSourceSpec, diskDir, script string) error {
	if err := Validate(source); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "kocto-bundle-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	work, err = filepath.EvalSymlinks(work)
	if err != nil {
		return err
	}
	repo := filepath.Join(work, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		return err
	}
	gitArgs := []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "http.followRedirects=false", "-c", "http.lowSpeedLimit=1024", "-c", "http.lowSpeedTime=60", "-c", "credential.helper=", "-c", "core.hooksPath=/dev/null"}
	git := func(args ...string) error {
		return run(ctx, repo, "git", append(append([]string{}, gitArgs...), args...)...)
	}
	if err := git("init", "--template="); err != nil {
		return fmt.Errorf("initialize Git: %w", err)
	}
	if err := git("fetch", "--depth=1", "--no-tags", "--", source.Repository, source.CommitHash); err != nil {
		return fmt.Errorf("fetch commit: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "FETCH_HEAD^{commit}")
	actual, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(actual)) != source.CommitHash {
		return fmt.Errorf("fetched commit does not match source.commitHash")
	}
	if err := git("-c", "advice.detachedHead=false", "checkout", "--detach", source.CommitHash); err != nil {
		return fmt.Errorf("checkout commit: %w", err)
	}
	index, err := exec.CommandContext(ctx, "git", "-C", repo, "ls-files", "--stage").Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(index), "\n") {
		if strings.HasPrefix(line, "160000 ") {
			return fmt.Errorf("git submodules are not supported")
		}
	}
	lab := filepath.Join(work, "lab")
	if err := Stage(filepath.Join(repo, filepath.FromSlash(source.Directory)), source.TopologyFile, lab); err != nil {
		return err
	}
	qcow := filepath.Join(work, "bundle.qcow2")
	if err := run(ctx, work, "bash", script, lab, qcow); err != nil {
		return fmt.Errorf("upstream bundle build: %w", err)
	}
	info, err := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", "-f", "qcow2", qcow).Output()
	if err != nil {
		return fmt.Errorf("inspect bundle: %w", err)
	}
	var metadata struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(info, &metadata); err != nil {
		return err
	}
	if metadata.VirtualSize <= 0 || metadata.VirtualSize > MaxDiskBytes {
		return fmt.Errorf("bundle virtual size exceeds the 6Gi limit")
	}
	// Never publish a partial image. Kubernetes marks the Job complete only after
	// this process has flushed the raw image. Retries replace only unpublished data.
	partial := filepath.Join(diskDir, "disk.img.part")
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := run(ctx, work, "qemu-img", "convert", "-f", "qcow2", "-O", "raw", qcow, partial); err != nil {
		return err
	}
	f, err := os.OpenFile(partial, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(partial, 0640); err != nil {
		return err
	}
	if err := os.Rename(partial, filepath.Join(diskDir, "disk.img")); err != nil {
		return err
	}
	directory, err := os.Open(diskDir)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// Stage keeps all files in source.directory. The canonical topology is generated
// at lab/topology.clab.yml; known local file references are rebased to their
// original location so nested topology files keep their bind/startup paths.
func Stage(directory, topology, destination string) error {
	root, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	// Reject symlinks in directory itself as well as its contents.
	real, err := filepath.EvalSymlinks(root)
	if err != nil || real != root {
		return fmt.Errorf("source.directory must exist without symlinks")
	}
	if !relativePath(topology, false) {
		return fmt.Errorf("invalid topology path")
	}
	var totalBytes int64
	var files int
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return fmt.Errorf("git metadata is not supported in lab files")
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("lab contains a symlink or special file: %s", rel)
		}
		input, err := os.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = input.Close() }()
		info, err := input.Stat()
		if err != nil {
			return err
		}
		files++
		if files > MaxLabFiles || info.Size() > MaxLabBytes-totalBytes {
			return fmt.Errorf("lab exceeds the 1Gi or 10000-file limit")
		}
		totalBytes += info.Size()
		reader := bufio.NewReader(input)
		marker, _ := reader.Peek(len("version https://git-lfs.github.com/spec/v1"))
		if string(marker) == "version https://git-lfs.github.com/spec/v1" {
			return fmt.Errorf("git LFS pointers are not supported: %s", rel)
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644|info.Mode().Perm()&0111)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, reader)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}); err != nil {
		return err
	}
	topologyPath := filepath.Join(destination, filepath.FromSlash(topology))
	info, err := os.Stat(topologyPath)
	if err != nil {
		return fmt.Errorf("read topology: %w", err)
	}
	if info.Size() > MaxTopologyBytes {
		return fmt.Errorf("topology exceeds the 1Mi limit")
	}
	data, err := os.ReadFile(topologyPath)
	if err != nil {
		return fmt.Errorf("read topology: %w", err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse topology: %w", err)
	}
	t, ok := document["topology"].(map[string]any)
	if !ok {
		return fmt.Errorf("topology must contain a topology mapping")
	}
	nodes, ok := t["nodes"].(map[string]any)
	if !ok || len(nodes) == 0 {
		return fmt.Errorf("topology must contain nodes")
	}

	for name, value := range nodes {
		node, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid node %s", name)
		}
		if _, ok := node["image"].(string); !ok {
			return fmt.Errorf("node %s must specify an explicit registry image", name)
		}
	}
	// Only node definition fields are paths. Labels, environment variables and
	// arbitrary user data may contain the same keys and must remain unchanged.
	definitions := []map[string]any{}
	if value, exists := t["defaults"]; exists {
		defaults, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("topology.defaults must be a mapping")
		}
		definitions = append(definitions, defaults)
	}
	for _, section := range []string{"nodes", "kinds", "groups"} {
		value, exists := t[section]
		if !exists {
			continue
		}
		items, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("topology.%s must be a mapping", section)
		}
		for name, value := range items {
			definition, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid %s definition %s", section, name)
			}
			definitions = append(definitions, definition)
		}
	}
	archives := map[string]string{}
	for _, definition := range definitions {
		value, exists := definition["image"]
		if !exists {
			continue
		}
		image, ok := value.(string)
		if !ok || !imagePattern.MatchString(image) || strings.Contains(image, "://") {
			return fmt.Errorf("invalid registry image %v", value)
		}
		archive := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(image)
		if previous, exists := archives[archive]; exists && previous != image {
			return fmt.Errorf("image archive name collision between %s and %s", previous, image)
		}
		archives[archive] = image
	}
	base := filepath.Dir(filepath.FromSlash(topology))
	rebase := func(value string) (string, error) {
		if filepath.IsAbs(value) || strings.ContainsAny(value, "$\x00\n\r\\") || strings.Contains(value, "://") || strings.Contains(value, "{{") {
			return "", fmt.Errorf("only local relative file references are supported: %q", value)
		}
		rel := filepath.Clean(filepath.Join(base, value))
		if rel == ".." || strings.HasPrefix(rel, "../") {
			return "", fmt.Errorf("file reference leaves source.directory: %q", value)
		}
		if _, err := os.Stat(filepath.Join(destination, rel)); err != nil {
			return "", fmt.Errorf("file reference %q: %w", value, err)
		}
		return "/opt/kocto/lab/" + filepath.ToSlash(rel), nil
	}

	for _, definition := range definitions {
		for _, key := range []string{"startup-config", "license"} {
			child, exists := definition[key]
			if !exists {
				continue
			}
			value, ok := child.(string)
			if !ok {
				return fmt.Errorf("%s must be a string", key)
			}
			if key == "startup-config" && strings.Contains(value, "\n") {
				continue
			}
			value, err := rebase(value)
			if err != nil {
				return err
			}
			definition[key] = value
		}
		if child, exists := definition["binds"]; exists {
			binds, ok := child.([]any)
			if !ok {
				return fmt.Errorf("binds must be a list")
			}
			for index, bind := range binds {
				value, ok := bind.(string)
				if !ok {
					return fmt.Errorf("bind must be a string")
				}
				parts := strings.Split(value, ":")
				if len(parts) < 2 || len(parts) > 3 || !strings.HasPrefix(parts[1], "/") {
					return fmt.Errorf("unsupported bind %q", value)
				}
				source, err := rebase(parts[0])
				if err != nil {
					return err
				}
				parts[0] = source
				binds[index] = strings.Join(parts, ":")
			}
		}
	}
	canonical := filepath.Join(destination, "topology.clab.yml")
	if topology != "topology.clab.yml" {
		if _, err := os.Stat(canonical); err == nil {
			return fmt.Errorf("source contains a different topology.clab.yml; canonical destination is ambiguous")
		}
	}
	data, err = yaml.Marshal(document)
	if err != nil {
		return err
	}
	return os.WriteFile(canonical, data, 0640)
}
