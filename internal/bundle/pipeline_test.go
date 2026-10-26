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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Use a real local Git repository. Only the remote transport and disk tools
// are substituted: this exercises checkout, input selection and publication
// without network access or a Docker daemon. It does not validate qcow2/ext4.
func TestBuildPublication(t *testing.T) {
	for _, test := range []struct {
		name, size, convertFail           string
		wrongCommit, cancelled, wantError bool
	}{
		{name: "complete"},
		{name: "wrong commit", wrongCommit: true, wantError: true},
		{name: "too large", size: "6442450945", wantError: true},
		{name: "conversion failure", convertFail: "1", wantError: true},
		{name: "cancelled", cancelled: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			repo, tools, disk := filepath.Join(root, "repo"), filepath.Join(root, "tools"), filepath.Join(root, "disk")
			for _, dir := range []string{repo, tools, disk} {
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) string {
				cmd := exec.Command(realGit, append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "core.hooksPath=/dev/null"}, args...)...)
				cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
				data, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("local git: %v: %s", err, data)
				}
				return strings.TrimSpace(string(data))
			}
			git("init", "--template=")
			if err := os.WriteFile(filepath.Join(repo, "custom.yml"), []byte("name: offline\ntopology:\n  nodes:\n    n1: {kind: linux, image: alpine:3.20}\n"), 0644); err != nil {
				t.Fatal(err)
			}
			git("add", "custom.yml")
			git("commit", "-m", "test")
			commit := git("rev-parse", "HEAD")
			t.Setenv("BUNDLE_REAL_GIT", realGit)
			t.Setenv("BUNDLE_TEST_REPO", repo)
			t.Setenv("BUNDLE_TEST_COMMIT", commit)
			t.Setenv("BUNDLE_TEST_SIZE", test.size)
			t.Setenv("BUNDLE_TEST_CONVERT_FAIL", test.convertFail)
			scripts := map[string]string{
				"git": `#!/bin/sh
case " $* " in
 *" fetch "*) exec "$BUNDLE_REAL_GIT" -c protocol.allow=always fetch --depth=1 --no-tags "$BUNDLE_TEST_REPO" "$BUNDLE_TEST_COMMIT" ;;
 *) exec "$BUNDLE_REAL_GIT" "$@" ;;
esac
`,
				"qemu-img": `#!/bin/sh
if [ "$1" = info ]; then
 printf '{"virtual-size":%s}\n' "${BUNDLE_TEST_SIZE:-4096}"
else
 for last; do :; done
 printf 'raw-test-output' > "$last"
 [ -z "$BUNDLE_TEST_CONVERT_FAIL" ]
fi
`,
				"build-bundle.sh": `#!/bin/sh
set -eu
test -f "$1/topology.clab.yml"
test -f "$1/custom.yml"
test ! -d "$1/.git"
printf qcow-test-output > "$2"
`,
			}
			for name, script := range scripts {
				if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			output := filepath.Join(disk, "disk.img")
			if err := os.WriteFile(output, []byte("existing-disk"), 0644); err != nil {
				t.Fatal(err)
			}
			source := validSource()
			source.CommitHash = commit
			source.TopologyFile = "custom.yml"
			if test.wrongCommit {
				source.CommitHash = strings.Repeat("a", 40)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancelled {
				cancel()
			}
			err = Build(ctx, source, disk, filepath.Join(tools, "build-bundle.sh"))
			if (err != nil) != test.wantError {
				t.Fatalf("Build error: %v, wanted failure %t", err, test.wantError)
			}
			content, readErr := os.ReadFile(output)
			if readErr != nil {
				t.Fatal(readErr)
			}
			expected := "raw-test-output"
			if test.wantError {
				expected = "existing-disk"
			}
			if string(content) != expected {
				t.Fatalf("published %q, expected %q", content, expected)
			}
		})
	}
}
