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
	content, volume, err := r.recordSnapshotChildren(ctx, c, s)
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
	}
	ready = ready && content != nil && content.Status != nil && content.Status.ReadyToUse != nil && *content.Status.ReadyToUse && volume != nil && volume.Status != nil && volume.Status.ReadyToUse != nil && *volume.Status.ReadyToUse && c.Status.SnapshotRef.VolumeSnapshotContentRef != nil
	if !ready {
		challengeCondition(c, "SnapshotReady", false, "SnapshotPending", "Waiting for the root disk snapshot")
		result, e := r.waitChallenge(ctx, c, "SnapshotPending", "Waiting for the root disk snapshot", false)
		return nil, false, result, e
	}
	challengeCondition(c, "SnapshotReady", true, "SnapshotSaved", "The source VM and root disk were saved")
	if err := r.saveChallenge(ctx, c); err != nil {
		return nil, false, ctrl.Result{}, err
	}
	return s, true, ctrl.Result{}, nil
}

// 削除前に各階層のUIDを保存する。途中で親が消えてもCSI側の削除完了まで追跡できるようにする。
func (r *ProblemChallengeReconciler) recordSnapshotChildren(ctx context.Context, c *labv1alpha1.ProblemChallenge, s *snapshotv1.VirtualMachineSnapshot) (*snapshotv1.VirtualMachineSnapshotContent, *csiv1.VolumeSnapshot, error) {
	ref := c.Status.SnapshotRef
	if s.Status == nil || s.Status.VirtualMachineSnapshotContentName == nil {
		return nil, nil, nil
	}
	content := &snapshotv1.VirtualMachineSnapshotContent{}
	key := types.NamespacedName{Name: *s.Status.VirtualMachineSnapshotContentName, Namespace: c.Namespace}
	if err := r.reader().Get(ctx, key, content); err != nil {
		if apierrors.IsNotFound(err) && ref.ContentRef == nil {
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
	_, sourceDisk := problemResourceNames(&labv1alpha1.Problem{ObjectMeta: metav1.ObjectMeta{UID: c.Status.SourceProblem.UID}})
	sourceVM := content.Spec.Source.VirtualMachine
	if sourceVM == nil || !hasOwner(sourceVM, "Problem", c.Status.SourceProblem.UID) || sourceVM.Spec.Template == nil || len(sourceVM.Spec.Template.Spec.Volumes) != 1 || sourceVM.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim == nil || sourceVM.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != sourceDisk {
		return nil, nil, resourceFailure("ResourceConflict", "Snapshot content has an unexpected source VM")
	}
	if len(content.Spec.VolumeBackups) != 1 || content.Spec.VolumeBackups[0].VolumeName != "root" || content.Spec.VolumeBackups[0].PersistentVolumeClaim.Name != sourceDisk {
		return nil, nil, resourceFailure("RootDiskExcluded", "Snapshot content does not contain only the author root disk")
	}
	backup := content.Spec.VolumeBackups[0]
	if backup.VolumeSnapshotName == nil {
		return content, nil, nil
	}
	volume := &csiv1.VolumeSnapshot{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: *backup.VolumeSnapshotName, Namespace: c.Namespace}, volume); err != nil {
		if apierrors.IsNotFound(err) && ref.VolumeSnapshotRef == nil {
			return content, nil, nil
		}
		if apierrors.IsNotFound(err) {
			return nil, nil, resourceFailure("VolumeSnapshotMissing", "Recorded root snapshot is missing")
		}
		return nil, nil, err
	}
	if !hasOwner(volume, "VirtualMachineSnapshotContent", content.UID) || volume.Spec.Source.PersistentVolumeClaimName == nil || *volume.Spec.Source.PersistentVolumeClaimName != sourceDisk {
		return nil, nil, resourceFailure("ResourceConflict", "CSI snapshot does not reference the author root disk")
	}
	if err := rememberRef(&ref.VolumeSnapshotRef, volume); err != nil {
		return nil, nil, err
	}
	if volume.Status == nil || volume.Status.BoundVolumeSnapshotContentName == nil {
		return content, volume, nil
	}
	storage := &csiv1.VolumeSnapshotContent{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: *volume.Status.BoundVolumeSnapshotContentName}, storage); err != nil {
		if apierrors.IsNotFound(err) && ref.VolumeSnapshotContentRef == nil {
			return content, volume, nil
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
	if err := rememberRef(&ref.VolumeSnapshotContentRef, storage); err != nil {
		return nil, nil, err
	}
	return content, volume, nil
}

// 復元PVCの由来とストレージ設定を照合するため、Snapshotに保存された元PVCを返す。
// 稼働中のmanager設定や現在のStorageProfileで保存時の構成を上書きしない。
func (r *ProblemChallengeReconciler) rootSnapshot(ctx context.Context, c *labv1alpha1.ProblemChallenge) (string, *snapshotv1.PersistentVolumeClaim, error) {
	ref := c.Status.SnapshotRef
	if ref == nil || ref.ContentRef == nil || ref.VolumeSnapshotRef == nil {
		return "", nil, resourceFailure("SnapshotNotReady", "Root snapshot references are not recorded")
	}
	content := &snapshotv1.VirtualMachineSnapshotContent{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: ref.ContentRef.Name, Namespace: c.Namespace}, content); err != nil {
		return "", nil, err
	}
	if content.UID != ref.ContentRef.UID || content.Name != challengeSnapshotContentName(ref.UID) || len(content.Spec.VolumeBackups) != 1 || content.Spec.VolumeBackups[0].VolumeName != "root" {
		return "", nil, resourceFailure("ResourceConflict", "Root snapshot content identity changed")
	}
	backup := content.Spec.VolumeBackups[0]
	if backup.VolumeSnapshotName == nil || *backup.VolumeSnapshotName != ref.VolumeSnapshotRef.Name || backup.PersistentVolumeClaim.UID == "" {
		return "", nil, resourceFailure("ResourceConflict", "Root snapshot origin is missing")
	}
	return *backup.VolumeSnapshotName, &backup.PersistentVolumeClaim, nil
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
