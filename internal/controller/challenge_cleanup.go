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
	"time"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	csiv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clonev1 "kubevirt.io/api/clone/v1beta1"
	virtv1 "kubevirt.io/api/core/v1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Clone/Restoreを先に止め、VM、ディスク、Snapshotの順で片付ける。全部消えるまで作問側のロックを保持する。
func (r *ProblemChallengeReconciler) deleteChallenge(ctx context.Context, c *labv1alpha1.ProblemChallenge) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(c, challengeCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	challengeCondition(c, "Deleting", true, "CleanupPending", "Removing team resources before releasing the author lock")
	challengeCondition(c, "Ready", false, "Deleting", "The Challenge is being deleted")
	for _, spec := range c.Spec.Teams {
		found := false
		for _, team := range c.Status.Teams {
			if team.ID == spec.ID {
				found = true
			}
		}
		if !found {
			c.Status.Teams = append(c.Status.Teams, labv1alpha1.ProblemChallengeTeamStatus{ID: spec.ID})
		}
	}
	if err := r.captureDeletingSnapshot(ctx, c); err != nil {
		return r.cleanupResult(ctx, c, false, err)
	}
	pending, err := r.deleteCloneProducers(ctx, c)
	if pending || err != nil {
		return r.cleanupResult(ctx, c, pending, err)
	}
	pending, err = r.deleteTeamVMs(ctx, c)
	if pending || err != nil {
		return r.cleanupResult(ctx, c, pending, err)
	}
	pending, err = r.deleteTeamPVCs(ctx, c)
	if pending || err != nil {
		return r.cleanupResult(ctx, c, pending, err)
	}
	pending, err = r.deleteSavedSnapshot(ctx, c)
	if pending || err != nil {
		return r.cleanupResult(ctx, c, pending, err)
	}
	// Problemを読み直し、自分が持つロックだけを解除する。再作成された同名Problemや他Challengeには触れない。
	if source := c.Status.SourceProblem; source != nil {
		p := &labv1alpha1.Problem{}
		err := r.reader().Get(ctx, types.NamespacedName{Name: source.Name, Namespace: c.Namespace}, p)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil && p.UID == source.UID && p.Status.ActiveChallenge != nil && p.Status.ActiveChallenge.UID == c.UID {
			p.Status.ActiveChallenge = nil
			if err := r.Status().Update(ctx, p); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(c), c); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	controllerutil.RemoveFinalizer(c, challengeCleanupFinalizer)
	return ctrl.Result{}, r.Update(ctx, c)
}

func (r *ProblemChallengeReconciler) cleanupResult(ctx context.Context, c *labv1alpha1.ProblemChallenge, pending bool, err error) (ctrl.Result, error) {
	if failure, ok := err.(*challengeResourceError); ok {
		challengeCondition(c, "Deleting", true, failure.reason, failure.message)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.saveChallenge(ctx, c)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	result := ctrl.Result{}
	if pending {
		result.RequeueAfter = 5 * time.Second
	}
	return result, r.saveChallenge(ctx, c)
}

// 親Snapshotを削除する前に、すでに作られた子のUIDを保存する。status保存前のクラッシュにも対応する。
func (r *ProblemChallengeReconciler) captureDeletingSnapshot(ctx context.Context, c *labv1alpha1.ProblemChallenge) error {
	s := &snapshotv1.VirtualMachineSnapshot{}
	err := r.reader().Get(ctx, types.NamespacedName{Name: challengeSnapshotName(c), Namespace: c.Namespace}, s)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedByChallenge(s, c) || (c.Status.SnapshotRef != nil && c.Status.SnapshotRef.UID != s.UID) {
		return resourceFailure("ResourceConflict", "Do not delete another snapshot")
	}
	if c.Status.SnapshotRef == nil {
		c.Status.SnapshotRef = &labv1alpha1.ChallengeSnapshotReference{Name: s.Name, UID: s.UID}
	}
	if c.Status.SourceProblem != nil {
		_, _, err = r.recordSnapshotChildren(ctx, c, s)
		// rootが除外された保存も片付ける。存在しない子は、削除がすでに進んだものとして扱う。
		if failure, ok := err.(*challengeResourceError); ok && (failure.reason == "RootDiskExcluded" || failure.reason == "SnapshotContentMissing" || failure.reason == "VolumeSnapshotMissing" || failure.reason == "VolumeSnapshotContentMissing") {
			err = nil
		}
		if err != nil {
			return err
		}
	}
	return r.saveChallenge(ctx, c)
}

// 生成処理が残る間は、復元先VM/PVCを削除しない。Cloneと内部Restoreの消失を確認してから次へ進む。
func (r *ProblemChallengeReconciler) deleteCloneProducers(ctx context.Context, c *labv1alpha1.ProblemChallenge) (bool, error) {
	pending := false
	for index := range c.Status.Teams {
		team := &c.Status.Teams[index]
		name, _, _ := challengeTeamNames(c, team.ID)
		clone := &clonev1.VirtualMachineClone{}
		err := r.reader().Get(ctx, types.NamespacedName{Name: name, Namespace: c.Namespace}, clone)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !ownedByChallenge(clone, c) || (team.CloneRef != nil && team.CloneRef.UID != clone.UID) {
			return false, resourceFailure("ResourceConflict", "Do not delete another clone")
		}
		team.CloneRef = resourceRef(clone)
		if err := r.saveChallenge(ctx, c); err != nil {
			return false, err
		}
		if clone.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, clone, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &clone.UID}); err != nil {
				return false, client.IgnoreNotFound(err)
			}
		}
		pending = true
	}
	restores := &snapshotv1.VirtualMachineRestoreList{}
	if err := r.reader().List(ctx, restores, client.InNamespace(c.Namespace)); err != nil {
		return false, err
	}
	for index := range restores.Items {
		restore := &restores.Items[index]
		for _, team := range c.Status.Teams {
			_, vmName, _ := challengeTeamNames(c, team.ID)
			if restore.Spec.Target.Name != vmName {
				continue
			}
			if team.CloneRef == nil || !hasOwner(restore, "VirtualMachineClone", team.CloneRef.UID) || restore.Spec.VirtualMachineSnapshotName != challengeSnapshotName(c) {
				return false, resourceFailure("ResourceConflict", "An unrelated restore targets a team VM")
			}
			if restore.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, restore, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &restore.UID}); err != nil {
					return false, client.IgnoreNotFound(err)
				}
			}
			pending = true
		}
	}
	return pending, nil
}

// status保存前に作られたVM/PVCも固定名で見つけ、由来を確認して参照を保存してから削除する。
func (r *ProblemChallengeReconciler) deleteTeamVMs(ctx context.Context, c *labv1alpha1.ProblemChallenge) (bool, error) {
	pending := false
	for index := range c.Status.Teams {
		team := &c.Status.Teams[index]
		_, name, pvcName := challengeTeamNames(c, team.ID)
		vm := &virtv1.VirtualMachine{}
		err := r.reader().Get(ctx, types.NamespacedName{Name: name, Namespace: c.Namespace}, vm)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		if err == nil {
			if !ownedByChallenge(vm, c) || (team.VMRef != nil && team.VMRef.UID != vm.UID) {
				return false, resourceFailure("ResourceConflict", "Do not delete another team VM")
			}
			team.VMRef = resourceRef(vm)
		}
		pvc := &corev1.PersistentVolumeClaim{}
		pvcErr := r.reader().Get(ctx, types.NamespacedName{Name: pvcName, Namespace: c.Namespace}, pvc)
		if pvcErr == nil {
			if !ownedByChallenge(pvc, c) {
				if err := r.adoptTeamPVC(ctx, c, team, pvc); err != nil {
					return false, err
				}
			}
			if team.PVCRef != nil && team.PVCRef.UID != pvc.UID {
				return false, resourceFailure("ResourceConflict", "Recorded PVC identity changed during cleanup")
			}
			team.PVCRef = resourceRef(pvc)
		} else if !apierrors.IsNotFound(pvcErr) {
			return false, pvcErr
		}
		if err := r.saveChallenge(ctx, c); err != nil {
			return false, err
		}
		if err == nil {
			if vm.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, vm, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &vm.UID}); err != nil {
					return false, client.IgnoreNotFound(err)
				}
			}
			pending = true
		}
		vmi := &virtv1.VirtualMachineInstance{}
		instanceErr := r.reader().Get(ctx, types.NamespacedName{Name: name, Namespace: c.Namespace}, vmi)
		if instanceErr == nil {
			if team.VMRef == nil || !hasOwner(vmi, "VirtualMachine", team.VMRef.UID) {
				return false, resourceFailure("ResourceConflict", "An unrelated VMI occupies the team name")
			}
			pending = true
		} else if !apierrors.IsNotFound(instanceErr) {
			return false, instanceErr
		}
	}
	pods := &corev1.PodList{}
	if err := r.reader().List(ctx, pods, client.InNamespace(c.Namespace), client.MatchingLabels{challengeUIDLabel: string(c.UID), virtv1.AppLabel: "virt-launcher"}); err != nil {
		return false, err
	}
	return pending || len(pods.Items) != 0, nil
}

func (r *ProblemChallengeReconciler) deleteTeamPVCs(ctx context.Context, c *labv1alpha1.ProblemChallenge) (bool, error) {
	pending := false
	for _, team := range c.Status.Teams {
		_, _, name := challengeTeamNames(c, team.ID)
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.reader().Get(ctx, types.NamespacedName{Name: name, Namespace: c.Namespace}, pvc)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if team.PVCRef == nil || team.PVCRef.Name != pvc.Name || team.PVCRef.UID != pvc.UID || !ownedByChallenge(pvc, c) {
			return false, resourceFailure("ResourceConflict", "Cannot prove restored PVC ownership")
		}
		if pvc.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pvc, client.Preconditions{UID: &pvc.UID}); err != nil {
				return false, client.IgnoreNotFound(err)
			}
		}
		pending = true
	}
	return pending, nil
}

// 親から子へ順に削除を確認する。CSI snapshot contentのfinalizerが消えるまで、ロックを解除しない。
func (r *ProblemChallengeReconciler) deleteSavedSnapshot(ctx context.Context, c *labv1alpha1.ProblemChallenge) (bool, error) {
	ref := c.Status.SnapshotRef
	if ref == nil {
		return false, nil
	}
	s := &snapshotv1.VirtualMachineSnapshot{}
	err := r.reader().Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: c.Namespace}, s)
	if err == nil {
		if s.UID != ref.UID || !ownedByChallenge(s, c) || s.Spec.DeletionPolicy == nil || *s.Spec.DeletionPolicy != snapshotv1.VirtualMachineSnapshotContentDelete {
			return false, resourceFailure("ResourceConflict", "Snapshot identity or deletion policy changed")
		}
		if s.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, s, client.Preconditions{UID: &s.UID}); err != nil {
				return false, client.IgnoreNotFound(err)
			}
		}
		return true, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	type snapshotChild struct {
		ref       *labv1alpha1.TeamResourceReference
		object    client.Object
		ownerKind string
		ownerUID  types.UID
		namespace string
	}
	objects := []snapshotChild{
		{ref.ContentRef, &snapshotv1.VirtualMachineSnapshotContent{}, "VirtualMachineSnapshot", ref.UID, c.Namespace},
	}
	if ref.ContentRef != nil {
		objects = append(objects, snapshotChild{ref.VolumeSnapshotRef, &csiv1.VolumeSnapshot{}, "VirtualMachineSnapshotContent", ref.ContentRef.UID, c.Namespace})
	}
	for _, entry := range objects {
		if entry.ref == nil {
			continue
		}
		err := r.reader().Get(ctx, types.NamespacedName{Name: entry.ref.Name, Namespace: entry.namespace}, entry.object)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		ownerMatches := hasOwner(entry.object, entry.ownerKind, entry.ownerUID)
		if content, ok := entry.object.(*snapshotv1.VirtualMachineSnapshotContent); ok {
			ownerMatches = content.Name == challengeSnapshotContentName(ref.UID) && content.Spec.VirtualMachineSnapshotName != nil && *content.Spec.VirtualMachineSnapshotName == ref.Name
		}
		if entry.object.GetUID() != entry.ref.UID || !ownerMatches {
			return false, resourceFailure("ResourceConflict", "Snapshot child identity changed during cleanup")
		}
		uid := entry.object.GetUID()
		if entry.object.GetDeletionTimestamp().IsZero() {
			if err := r.Delete(ctx, entry.object, client.Preconditions{UID: &uid}); err != nil {
				return false, client.IgnoreNotFound(err)
			}
		}
		return true, nil
	}
	if storageRef := ref.VolumeSnapshotContentRef; storageRef != nil {
		storage := &csiv1.VolumeSnapshotContent{}
		err := r.reader().Get(ctx, types.NamespacedName{Name: storageRef.Name}, storage)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		binding := storage.Spec.VolumeSnapshotRef
		if ref.VolumeSnapshotRef == nil || storage.UID != storageRef.UID || binding.UID != ref.VolumeSnapshotRef.UID || binding.Name != ref.VolumeSnapshotRef.Name || binding.Namespace != c.Namespace || storage.Spec.DeletionPolicy != csiv1.VolumeSnapshotContentDelete {
			return false, resourceFailure("ResourceConflict", "Do not delete unrelated CSI snapshot content")
		}
		if storage.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, storage, client.Preconditions{UID: &storage.UID}); err != nil {
				return false, client.IgnoreNotFound(err)
			}
		}
		return true, nil
	}
	return false, nil
}
