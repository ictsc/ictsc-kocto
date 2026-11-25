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

package controller

import (
	"context"
	"slices"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	csiv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	virtv1 "kubevirt.io/api/core/v1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// リソースの不整合はAPIの一時エラーと分ける。別UIDへの差し替えやディスク喪失で自動再作成しない。
type challengeResourceError struct{ reason, message string }

func (e *challengeResourceError) Error() string { return e.message }
func resourceFailure(reason, message string) error {
	return &challengeResourceError{reason: reason, message: message}
}

func resourceRef(object client.Object) *labv1alpha1.TeamResourceReference {
	return &labv1alpha1.TeamResourceReference{Name: object.GetName(), UID: object.GetUID()}
}
func rememberRef(ref **labv1alpha1.TeamResourceReference, object client.Object) error {
	if *ref != nil && ((*ref).Name != object.GetName() || (*ref).UID != object.GetUID()) {
		return resourceFailure("ResourceConflict", "Recorded resource identity changed")
	}
	*ref = resourceRef(object)
	return nil
}

// 停止確認後に1つだけSnapshotを作り、rootディスクを含む保存が完了するまで待つ。
func (r *ProblemChallengeReconciler) ensureSnapshot(ctx context.Context, c *labv1alpha1.ProblemChallenge, p *labv1alpha1.Problem) (*snapshotv1.VirtualMachineSnapshot, bool, ctrl.Result, error) {
	if c.Status.SourceProblem == nil || c.Status.SourceProblem.DiskPVCUID == "" || p.Status.DiskRef == nil ||
		p.Status.DiskRef.PVCUID == "" || p.Status.DiskRef.PVCUID != c.Status.SourceProblem.DiskPVCUID {
		result, e := r.failSnapshot(ctx, c, "ResourceConflict", "Author disk PVC identity is not recorded consistently")
		return nil, false, result, e
	}
	sourcePVC := &corev1.PersistentVolumeClaim{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: p.Status.DiskRef.DiskName, Namespace: p.Namespace}, sourcePVC); err != nil {
		if apierrors.IsNotFound(err) {
			result, e := r.failSnapshot(ctx, c, "SourceDiskMissing", "The locked author root PVC is missing")
			return nil, false, result, e
		}
		return nil, false, ctrl.Result{}, err
	}
	if sourcePVC.UID != c.Status.SourceProblem.DiskPVCUID || !ownedByDataVolume(sourcePVC, p.Status.DiskRef.UID) || sourcePVC.Status.Phase != corev1.ClaimBound || !sourcePVC.DeletionTimestamp.IsZero() {
		result, e := r.failSnapshot(ctx, c, "ResourceConflict", "The locked author root PVC identity changed")
		return nil, false, result, e
	}
	if c.Status.SourceProblem.BundlePVCUID != "" {
		bundlePVC := &corev1.PersistentVolumeClaim{}
		if p.Status.BundleRef == nil || p.Status.BundleRef.PVCUID != c.Status.SourceProblem.BundlePVCUID || p.Status.BundleRef.SourceHash != c.Status.SourceProblem.BundleSourceHash || !p.Status.BundleRef.Ready {
			result, e := r.failSnapshot(ctx, c, "ResourceConflict", "Author bundle identity changed")
			return nil, false, result, e
		}
		if err := r.reader().Get(ctx, types.NamespacedName{Name: problemBundleName(p), Namespace: p.Namespace}, bundlePVC); err != nil {
			if apierrors.IsNotFound(err) {
				result, e := r.failSnapshot(ctx, c, "SourceDiskMissing", "The locked author bundle PVC is missing")
				return nil, false, result, e
			}
			return nil, false, ctrl.Result{}, err
		}
		if bundlePVC.UID != c.Status.SourceProblem.BundlePVCUID || !ownedByProblem(bundlePVC, p) || bundlePVC.Annotations[bundleSourceAnnotation] != p.Status.BundleRef.SourceHash || bundlePVC.Status.Phase != corev1.ClaimBound || !bundlePVC.DeletionTimestamp.IsZero() {
			result, e := r.failSnapshot(ctx, c, "ResourceConflict", "The locked author bundle PVC identity changed")
			return nil, false, result, e
		}
	}
	s := &snapshotv1.VirtualMachineSnapshot{}
	err := r.reader().Get(ctx, types.NamespacedName{Name: challengeSnapshotName(c), Namespace: c.Namespace}, s)
	if apierrors.IsNotFound(err) {
		if c.Status.SnapshotRef != nil {
			result, e := r.failSnapshot(ctx, c, "SnapshotMissing", "The recorded snapshot is missing; do not resnapshot")
			return nil, false, result, e
		}
		if p.Status.VMRef == nil {
			result, e := r.failSnapshot(ctx, c, "SourceVMMissing", "The author VM reference is missing")
			return nil, false, result, e
		}
		// Quiescedの表示に加え、VMのHaltedとVMI/Podの消失を直接確認する。
		vm := &virtv1.VirtualMachine{}
		if err := r.reader().Get(ctx, types.NamespacedName{Name: p.Status.VMRef.VMName, Namespace: p.Namespace}, vm); err != nil {
			if apierrors.IsNotFound(err) {
				result, e := r.failSnapshot(ctx, c, "SourceVMMissing", "The locked author VM is missing")
				return nil, false, result, e
			}
			return nil, false, ctrl.Result{}, err
		}
		if !ownedByProblem(vm, p) || vm.UID != p.Status.VMRef.UID {
			result, e := r.failSnapshot(ctx, c, "ResourceConflict", "Author VM identity changed")
			return nil, false, result, e
		}
		stopped, err := (&ProblemReconciler{Client: r.Client, APIReader: r.reader()}).problemStopped(ctx, p, vm)
		if err != nil {
			return nil, false, ctrl.Result{}, err
		}
		quiesced := apiMeta.FindStatusCondition(p.Status.Conditions, "Quiesced")
		if vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != virtv1.RunStrategyHalted || !stopped || quiesced == nil || quiesced.Status != metav1.ConditionTrue || quiesced.ObservedGeneration != p.Generation {
			reason := "AuthorStopping"
			if condition := apiMeta.FindStatusCondition(p.Status.Conditions, "VMReady"); condition != nil && condition.Reason == "ShutdownTimeout" {
				reason = "ShutdownTimeout"
			}
			challengeCondition(c, "SnapshotReady", false, reason, "Waiting for the locked author VM to stop")
			result, e := r.waitChallenge(ctx, c, reason, "Waiting for the locked author VM to stop", true)
			return nil, false, result, e
		}
		s = challengeSnapshot(c, vm.Name)
		if err := r.Create(ctx, s); err != nil {
			return nil, false, ctrl.Result{}, err
		}
	} else if err != nil {
		return nil, false, ctrl.Result{}, err
	}
	vmName, _ := problemResourceNames(&labv1alpha1.Problem{ObjectMeta: metav1.ObjectMeta{UID: c.Status.SourceProblem.UID}})
	if !ownedByChallenge(s, c) || s.Spec.Source.Kind != "VirtualMachine" || s.Spec.Source.APIGroup == nil || *s.Spec.Source.APIGroup != "kubevirt.io" || s.Spec.Source.Name != vmName || (c.Status.SnapshotRef != nil && (s.UID != c.Status.SnapshotRef.UID || s.Name != c.Status.SnapshotRef.Name)) {
		result, e := r.failSnapshot(ctx, c, "ResourceConflict", "Snapshot does not belong to this Challenge or source")
		return nil, false, result, e
	}
	if c.Status.SnapshotRef == nil {
		c.Status.SnapshotRef = &labv1alpha1.ChallengeSnapshotReference{Name: s.Name, UID: s.UID}
	}
	if !s.DeletionTimestamp.IsZero() {
		result, e := r.failSnapshot(ctx, c, "SnapshotDeleting", "The snapshot is being deleted")
		return nil, false, result, e
	}
	if s.Status != nil && s.Status.Phase == snapshotv1.Failed {
		challengeCondition(c, "SnapshotReady", false, "SnapshotFailed", "Snapshot creation failed")
		result, e := r.failSnapshot(ctx, c, "SnapshotFailed", "Snapshot creation failed; keep the author lock")
		return nil, false, result, e
	}
	content, volume, err := r.recordSnapshotChildren(ctx, c, s, false)
	if err != nil {
		if failure, ok := err.(*challengeResourceError); ok {
			result, e := r.failSnapshot(ctx, c, failure.reason, failure.message)
			return nil, false, result, e
		}
		return nil, false, ctrl.Result{}, err
	}
	ready := s.Status != nil && s.Status.ReadyToUse != nil && *s.Status.ReadyToUse && s.Status.Phase == snapshotv1.Succeeded
	if ready {
		// VM構成だけの保存を成功にしない。rootの除外や追加ディスクは今回の範囲外。
		if s.Status.SnapshotVolumes == nil || !slices.Contains(s.Status.SnapshotVolumes.IncludedVolumes, "root") || slices.Contains(s.Status.SnapshotVolumes.ExcludedVolumes, "root") {
			result, e := r.failSnapshot(ctx, c, "RootDiskExcluded", "The snapshot did not include the root disk")
			return nil, false, result, e
		}
		if p.Status.VMRef == nil || s.Status.SourceUID == nil || *s.Status.SourceUID != p.Status.VMRef.UID {
			result, e := r.failSnapshot(ctx, c, "ResourceConflict", "Snapshot source VM identity changed")
			return nil, false, result, e
		}
		if c.Status.SourceProblem.BundlePVCUID != "" && (!slices.Contains(s.Status.SnapshotVolumes.IncludedVolumes, "bundle") || slices.Contains(s.Status.SnapshotVolumes.ExcludedVolumes, "bundle")) {
			result, e := r.failSnapshot(ctx, c, "BundleDiskExcluded", "The snapshot did not include the bundle disk")
			return nil, false, result, e
		}
	}
	ready = ready && content != nil && content.Status != nil && content.Status.ReadyToUse != nil && *content.Status.ReadyToUse && volume != nil && volume.Status != nil && volume.Status.ReadyToUse != nil && *volume.Status.ReadyToUse && c.Status.SnapshotRef.VolumeSnapshotContentRef != nil
	if c.Status.SourceProblem.BundlePVCUID != "" {
		ref := c.Status.SnapshotRef.BundleVolumeSnapshotRef
		ready = ready && ref != nil && c.Status.SnapshotRef.BundleVolumeSnapshotContentRef != nil
		if ref != nil {
			bundle := &csiv1.VolumeSnapshot{}
			if err := r.reader().Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: c.Namespace}, bundle); err != nil {
				return nil, false, ctrl.Result{}, err
			}
			ready = ready && bundle.UID == ref.UID && bundle.Status != nil && bundle.Status.ReadyToUse != nil && *bundle.Status.ReadyToUse
		}
	}
	if !ready {
		challengeCondition(c, "SnapshotReady", false, "SnapshotPending", "Waiting for required disk snapshots")
		result, e := r.waitChallenge(ctx, c, "SnapshotPending", "Waiting for required disk snapshots", false)
		return nil, false, result, e
	}
	challengeCondition(c, "SnapshotReady", true, "SnapshotSaved", "The source VM and required disks were saved")
	if err := r.saveChallenge(ctx, c); err != nil {
		return nil, false, ctrl.Result{}, err
	}
	return s, true, ctrl.Result{}, nil
}

// 削除前に各階層のUIDを保存する。途中で親が消えてもCSI側の削除完了まで追跡できるようにする。
func (r *ProblemChallengeReconciler) recordSnapshotChildren(ctx context.Context, c *labv1alpha1.ProblemChallenge, s *snapshotv1.VirtualMachineSnapshot, deleting bool) (*snapshotv1.VirtualMachineSnapshotContent, *csiv1.VolumeSnapshot, error) {
	ref := c.Status.SnapshotRef
	if s.Status == nil || s.Status.VirtualMachineSnapshotContentName == nil {
		return nil, nil, nil
	}
	content := &snapshotv1.VirtualMachineSnapshotContent{}
	key := types.NamespacedName{Name: *s.Status.VirtualMachineSnapshotContentName, Namespace: c.Namespace}
	if err := r.reader().Get(ctx, key, content); err != nil {
		if apierrors.IsNotFound(err) && (ref.ContentRef == nil || deleting) {
			return nil, nil, nil
		}
		if apierrors.IsNotFound(err) {
			return nil, nil, resourceFailure("SnapshotContentMissing", "Recorded snapshot content is missing")
		}
		return nil, nil, err
	}
	if content.Name != challengeSnapshotContentName(s.UID) || content.Spec.VirtualMachineSnapshotName == nil || *content.Spec.VirtualMachineSnapshotName != s.Name {
		return nil, nil, resourceFailure("ResourceConflict", "Snapshot content ownership does not match")
	}
	if err := rememberRef(&ref.ContentRef, content); err != nil {
		return nil, nil, err
	}
	problem := &labv1alpha1.Problem{ObjectMeta: metav1.ObjectMeta{UID: c.Status.SourceProblem.UID}}
	_, sourceDisk := problemResourceNames(problem)
	expected := map[string]struct {
		name       string
		uid        types.UID
		volumeRef  **labv1alpha1.TeamResourceReference
		storageRef **labv1alpha1.TeamResourceReference
	}{"root": {sourceDisk, c.Status.SourceProblem.DiskPVCUID, &ref.VolumeSnapshotRef, &ref.VolumeSnapshotContentRef}}
	if c.Status.SourceProblem.BundlePVCUID != "" {
		expected["bundle"] = struct {
			name       string
			uid        types.UID
			volumeRef  **labv1alpha1.TeamResourceReference
			storageRef **labv1alpha1.TeamResourceReference
		}{problemBundleName(problem), c.Status.SourceProblem.BundlePVCUID, &ref.BundleVolumeSnapshotRef, &ref.BundleVolumeSnapshotContentRef}
	}
	sourceVM := content.Spec.Source.VirtualMachine
	if sourceVM == nil || !hasOwner(sourceVM, "Problem", c.Status.SourceProblem.UID) || sourceVM.Spec.Template == nil || len(sourceVM.Spec.Template.Spec.Volumes) != len(expected) {
		return nil, nil, resourceFailure("ResourceConflict", "Snapshot content has an unexpected source VM")
	}
	seen := map[string]bool{}
	for _, volume := range sourceVM.Spec.Template.Spec.Volumes {
		want, ok := expected[volume.Name]
		if !ok || seen[volume.Name] || volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != want.name {
			return nil, nil, resourceFailure("ResourceConflict", "Snapshot source VM volumes do not match")
		}
		// KubeVirt must be able to prepare a Filesystem PVC before presenting
		// the virtual bundle disk read-only to the guest.
		if volume.Name == "bundle" && volume.PersistentVolumeClaim.ReadOnly {
			return nil, nil, resourceFailure("ResourceConflict", "Snapshot source bundle PVC mount is read-only")
		}
		seen[volume.Name] = true
	}
	if _, ok := expected["bundle"]; ok {
		if len(sourceVM.Spec.Template.Spec.Domain.Devices.Disks) != 2 {
			return nil, nil, resourceFailure("ResourceConflict", "Snapshot source has unexpected disks")
		}
		readOnly := false
		for _, disk := range sourceVM.Spec.Template.Spec.Domain.Devices.Disks {
			if disk.Name == "bundle" && disk.Disk != nil && disk.Disk.ReadOnly {
				readOnly = true
			}
		}
		if !readOnly {
			return nil, nil, resourceFailure("ResourceConflict", "Snapshot source bundle disk is not read-only")
		}
	}
	if !deleting && len(content.Spec.VolumeBackups) != len(expected) {
		return nil, nil, resourceFailure("RootDiskExcluded", "Snapshot content does not contain all required disks")
	}
	seen = map[string]bool{}
	var rootVolume *csiv1.VolumeSnapshot
	for _, backup := range content.Spec.VolumeBackups {
		want, ok := expected[backup.VolumeName]
		if !ok || seen[backup.VolumeName] || backup.PersistentVolumeClaim.Name != want.name || backup.PersistentVolumeClaim.UID != want.uid {
			return nil, nil, resourceFailure("ResourceConflict", "Snapshot backup origin does not match the source PVC")
		}
		seen[backup.VolumeName] = true
		if backup.VolumeSnapshotName == nil {
			if !deleting && *want.volumeRef != nil {
				return nil, nil, resourceFailure("ResourceConflict", "Recorded volume snapshot was removed from the snapshot content")
			}
			continue
		}
		volume := &csiv1.VolumeSnapshot{}
		if err := r.reader().Get(ctx, types.NamespacedName{Name: *backup.VolumeSnapshotName, Namespace: c.Namespace}, volume); err != nil {
			if apierrors.IsNotFound(err) && (*want.volumeRef == nil || deleting) {
				continue
			}
			if apierrors.IsNotFound(err) {
				return nil, nil, resourceFailure("VolumeSnapshotMissing", "Recorded volume snapshot is missing")
			}
			return nil, nil, err
		}
		if !hasOwner(volume, "VirtualMachineSnapshotContent", content.UID) || volume.Spec.Source.PersistentVolumeClaimName == nil || *volume.Spec.Source.PersistentVolumeClaimName != want.name {
			return nil, nil, resourceFailure("ResourceConflict", "CSI snapshot does not reference the source disk")
		}
		if err := rememberRef(want.volumeRef, volume); err != nil {
			return nil, nil, err
		}
		if backup.VolumeName == "root" {
			rootVolume = volume
		}
		if volume.Status == nil || volume.Status.BoundVolumeSnapshotContentName == nil {
			if !deleting && *want.storageRef != nil {
				return nil, nil, resourceFailure("ResourceConflict", "Recorded CSI snapshot binding is missing")
			}
			continue
		}
		storage := &csiv1.VolumeSnapshotContent{}
		if err := r.reader().Get(ctx, types.NamespacedName{Name: *volume.Status.BoundVolumeSnapshotContentName}, storage); err != nil {
			if apierrors.IsNotFound(err) && (*want.storageRef == nil || deleting) {
				continue
			}
			if apierrors.IsNotFound(err) {
				return nil, nil, resourceFailure("VolumeSnapshotContentMissing", "Recorded CSI snapshot content is missing")
			}
			return nil, nil, err
		}
		source := storage.Spec.VolumeSnapshotRef
		if source.UID != volume.UID || source.Name != volume.Name || source.Namespace != volume.Namespace || storage.Spec.DeletionPolicy != csiv1.VolumeSnapshotContentDelete {
			return nil, nil, resourceFailure("ResourceConflict", "CSI snapshot binding or deletion policy does not match")
		}
		if err := rememberRef(want.storageRef, storage); err != nil {
			return nil, nil, err
		}
	}
	return content, rootVolume, nil
}

// 復元PVCの由来とストレージ設定を照合するため、Snapshotに保存された元PVCを返す。
// 稼働中のmanager設定や現在のStorageProfileで保存時の構成を上書きしない。
func (r *ProblemChallengeReconciler) diskSnapshot(ctx context.Context, c *labv1alpha1.ProblemChallenge, disk string) (string, *snapshotv1.PersistentVolumeClaim, error) {
	ref := c.Status.SnapshotRef
	if ref == nil || ref.ContentRef == nil {
		return "", nil, resourceFailure("SnapshotNotReady", "Snapshot references are not recorded")
	}
	volumeRef := ref.VolumeSnapshotRef
	if disk == "bundle" {
		volumeRef = ref.BundleVolumeSnapshotRef
	}
	if volumeRef == nil {
		return "", nil, resourceFailure("SnapshotNotReady", "Disk snapshot reference is not recorded")
	}
	content := &snapshotv1.VirtualMachineSnapshotContent{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: ref.ContentRef.Name, Namespace: c.Namespace}, content); err != nil {
		return "", nil, err
	}
	if content.UID != ref.ContentRef.UID || content.Name != challengeSnapshotContentName(ref.UID) {
		return "", nil, resourceFailure("ResourceConflict", "Snapshot content identity changed")
	}
	var found *snapshotv1.VolumeBackup
	for i := range content.Spec.VolumeBackups {
		if content.Spec.VolumeBackups[i].VolumeName == disk {
			if found != nil {
				return "", nil, resourceFailure("ResourceConflict", "Duplicate snapshot backup")
			}
			found = &content.Spec.VolumeBackups[i]
		}
	}
	if c.Status.SourceProblem == nil {
		return "", nil, resourceFailure("ResourceConflict", "Source Problem reference is missing")
	}
	sourceUID := c.Status.SourceProblem.DiskPVCUID
	if disk == "bundle" {
		sourceUID = c.Status.SourceProblem.BundlePVCUID
	}
	if found == nil || found.VolumeSnapshotName == nil || *found.VolumeSnapshotName != volumeRef.Name || sourceUID == "" || found.PersistentVolumeClaim.UID != sourceUID {
		return "", nil, resourceFailure("ResourceConflict", "Disk snapshot origin is missing")
	}
	return *found.VolumeSnapshotName, &found.PersistentVolumeClaim, nil
}

func snapshotConditionFailed(conditions []snapshotv1.Condition) bool {
	for _, condition := range conditions {
		if condition.Type == snapshotv1.ConditionFailure && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *ProblemChallengeReconciler) failSnapshot(ctx context.Context, c *labv1alpha1.ProblemChallenge, reason, message string) (ctrl.Result, error) {
	challengeCondition(c, "SnapshotReady", false, reason, message)
	return r.failChallenge(ctx, c, reason, message)
}
