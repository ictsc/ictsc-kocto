//go:build ignore

/*
Copyright 2026.
SPDX-License-Identifier: Apache-2.0
*/

package hostpath

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StateDirを専有する単一CSIプロセスの起動時に、異常終了で残った未公開のコピーだけを片付ける。
func removePartialReflinks(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".kocto-reflink-") {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// 検証用Filesystemボリュームをreflinkで複製する。通常コピーへは切り替えない。
// 元VMはKubeVirt側で停止させる。稼働中に更新される複数ファイルの整合性は保証しない。
func reflinkDirectory(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return status.Error(codes.FailedPrecondition, "Filesystem snapshot must be a reflink directory; recreate legacy tar snapshots")
	}
	if info, err := os.Lstat(destination); err == nil {
		if !info.IsDir() {
			return status.Error(codes.AlreadyExists, "Reflink destination is not an empty directory")
		}
		empty, err := hostPathIsEmpty(destination)
		if err != nil {
			return err
		}
		if !empty {
			return status.Error(codes.AlreadyExists, "Do not overwrite an existing volume or snapshot")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// 作成途中のSnapshotを公開しない。同じファイルシステム上の一時ディレクトリからrenameする。
	temporary, err := os.MkdirTemp(filepath.Dir(destination), ".kocto-reflink-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	output, err := exec.Command("cp", "--archive", "--reflink=always", "--", source+"/.", temporary+"/").CombinedOutput()
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "Reflink copy failed; a CoW filesystem on the same device is required: %v: %s", err, output)
	}
	// reflinkで作ったinodeと共有ブロックの参照を永続化してからCSIへ成功を返す。
	if output, err := exec.Command("sync", "-f", temporary).CombinedOutput(); err != nil {
		return fmt.Errorf("could not sync reflink data: %w: %s", err, output)
	}
	// Goのos.Renameは既存ディレクトリを置換できない。復元先として確保された空のものだけ外す。
	if info, err := os.Lstat(destination); err == nil {
		if !info.IsDir() {
			return status.Error(codes.AlreadyExists, "Reflink destination changed")
		}
		if err := os.Remove(destination); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
