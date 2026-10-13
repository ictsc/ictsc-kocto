//go:build ignore

/*
Copyright 2026.
SPDX-License-Identifier: Apache-2.0
*/

package hostpath

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-driver-host-path/pkg/state"
)

func TestKoctoReflinkFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	// cpの失敗を再現する。reflink非対応時に通常コピーで成功しないことを検証する。
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "cp"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := reflinkDirectory(source, target); err == nil {
		t.Fatal("unsupported reflink succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".kocto-reflink-") {
			t.Fatal("partial snapshot leaked")
		}
	}
	if empty, err := hostPathIsEmpty(target); err != nil || !empty {
		t.Fatalf("destination modified: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reflinkDirectory(source, target); err == nil {
		t.Fatal("nonempty destination overwritten")
	}
	if data, err := os.ReadFile(filepath.Join(target, "keep")); err != nil || string(data) != "original" {
		t.Fatalf("existing data lost: %v", err)
	}
}

func TestKoctoReflinkRejectsLegacyArchives(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "legacy.snap")
	if err := os.WriteFile(archive, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reflinkDirectory(archive, filepath.Join(dir, "restore")); err == nil {
		t.Fatal("legacy archive accepted")
	}
}

func TestKoctoReflinkRestartRemovesOnlyPartialCopies(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".kocto-reflink-interrupted", "volume", "saved.snap"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "disk.img"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewHostPathDriver(Config{StateDir: dir, DriverName: "hostpath.csi.k8s.io", NodeID: "test", Endpoint: "unix://" + filepath.Join(dir, "csi.sock")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kocto-reflink-interrupted")); !os.IsNotExist(err) {
		t.Fatalf("partial copy remained: %v", err)
	}
	for _, name := range []string{"volume", "saved.snap"} {
		if _, err := os.Stat(filepath.Join(dir, name, "disk.img")); err != nil {
			t.Fatal(err)
		}
	}
}

// CIのファイルシステムがCoW対応とは限らないため、実機のXFS上で明示して実行する。
func TestKoctoReflinkSnapshotRoundTrip(t *testing.T) {
	root := os.Getenv("KOCTO_REFLINK_TEST_DIR")
	if root == "" {
		t.Skip("set KOCTO_REFLINK_TEST_DIR to a CoW filesystem for the integration test")
	}
	dir, err := os.MkdirTemp(root, "kocto-reflink-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	hp, err := NewHostPathDriver(Config{StateDir: dir, DriverName: "hostpath.csi.k8s.io", NodeID: "test", MaxVolumeSize: 1 << 30, Endpoint: "unix://" + filepath.Join(dir, "csi.sock")})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("source-data"), 100000)
	if err := os.WriteFile(filepath.Join(source, "disk.img"), data, 0600); err != nil {
		t.Fatal(err)
	}
	volume := state.Volume{VolID: "source", VolName: "source", VolPath: source, VolSize: 1 << 20, VolAccessType: state.MountAccess}
	if err := hp.state.UpdateVolume(volume); err != nil {
		t.Fatal(err)
	}
	snap, err := hp.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "saved", SourceVolumeId: "source"})
	if err != nil {
		t.Fatal(err)
	}
	// CSIの再起動後も同じsnapshotを復元できることを確認する。
	hp, err = NewHostPathDriver(hp.config)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		target := filepath.Join(dir, name)
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		if err := hp.loadFromSnapshot(1<<20, snap.Snapshot.SnapshotId, target, state.MountAccess); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(target, "disk.img"))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("restore differs: %v", err)
		}
		// 容量表示ではなくFIEMAPのSHAREDフラグで物理ブロックの共有を検証する。
		output, err := exec.Command("xfs_io", "-r", "-c", "fiemap -v", filepath.Join(target, "disk.img")).CombinedOutput()
		if err != nil {
			t.Fatalf("fiemap failed: %v: %s", err, output)
		}
		shared := false
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			flags, err := strconv.ParseUint(fields[len(fields)-1], 0, 32)
			if err == nil && flags&0x2000 != 0 {
				shared = true
			}
		}
		if !shared {
			t.Fatalf("clone contains no shared physical extents: %s", output)
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "a", "disk.img"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("team-a"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := hp.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: snap.Snapshot.SnapshotId}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hp.getSnapshotPath(snap.Snapshot.SnapshotId)); !os.IsNotExist(err) {
		t.Fatalf("snapshot remained: %v", err)
	}
	for _, path := range []string{filepath.Join(source, "disk.img"), filepath.Join(dir, "b", "disk.img")} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("source or sibling changed: %v", err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "a", "disk.img"))
	if err != nil || len(got) < 6 || !bytes.Equal(got[:6], []byte("team-a")) {
		t.Fatalf("clone lost after snapshot deletion: %v", err)
	}
}
