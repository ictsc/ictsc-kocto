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
	"fmt"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	clonev1 "kubevirt.io/api/clone/v1beta1"
	virtv1 "kubevirt.io/api/core/v1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func failTeam(c *labv1alpha1.ProblemChallenge, team *labv1alpha1.ProblemChallengeTeamStatus, reason, message string) {
	teamCondition(c, team, "Failed", true, reason, message)
	teamCondition(c, team, "Ready", false, reason, message)
}

// 初回はCloneの完了と復元PVCを確認して起動する。失敗済みチームは次のReconcileでも復旧しない。
func (r *ProblemChallengeReconciler) reconcileTeam(ctx context.Context, c *labv1alpha1.ProblemChallenge, team *labv1alpha1.ProblemChallengeTeamStatus, snapshot *snapshotv1.VirtualMachineSnapshot) error {
	if apiMeta.IsStatusConditionTrue(team.Conditions, "Failed") {
		return nil
	}
	cloneName, vmName, pvcName := challengeTeamNames(c, team.ID)
	clone := &clonev1.VirtualMachineClone{}
	err := r.reader().Get(ctx, types.NamespacedName{Name: cloneName, Namespace: c.Namespace}, clone)
	if apierrors.IsNotFound(err) {
		if team.CloneRef != nil && team.VMRef == nil {
			failTeam(c, team, "CloneMissing", "The recorded clone is missing")
			return nil
		}
		if team.CloneRef == nil {
			// 新規Cloneが同名のVM/PVCを上書きしないよう、作成前に占有を確認する。
			for _, object := range []client.Object{&virtv1.VirtualMachine{}, &corev1.PersistentVolumeClaim{}} {
				name := vmName
				if _, ok := object.(*corev1.PersistentVolumeClaim); ok {
					name = pvcName
				}
				getErr := r.reader().Get(ctx, types.NamespacedName{Name: name, Namespace: c.Namespace}, object)
				if getErr == nil {
					failTeam(c, team, "ResourceConflict", "Team VM or PVC name is occupied")
					return nil
				}
				if !apierrors.IsNotFound(getErr) {
					return getErr
				}
			}
			clone, err = challengeClone(c, team.ID)
			if err != nil {
				return err
			}
			if err := r.Create(ctx, clone); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	if clone.UID != "" {
		desired, err := challengeClone(c, team.ID)
		if err != nil {
			return err
		}
		if !ownedByChallenge(clone, c) || !equality.Semantic.DeepEqual(clone.Spec, desired.Spec) || (team.CloneRef != nil && team.CloneRef.UID != clone.UID) || !clone.DeletionTimestamp.IsZero() {
			failTeam(c, team, "ResourceConflict", "Clone identity or configuration changed")
			return nil
		}
		team.CloneRef = resourceRef(clone)
		if clone.Status.Phase == clonev1.Failed {
			failTeam(c, team, "CloneFailed", "KubeVirt could not restore the team VM")
			return nil
		}
		if clone.Status.Phase != clonev1.Succeeded {
			teamCondition(c, team, "Ready", false, "ClonePending", "Waiting for the team clone")
			return nil
		}
	}
	// CloneのSucceededだけでは内部Restoreの処理が終わっていない場合がある。
	restores := &snapshotv1.VirtualMachineRestoreList{}
	if err := r.reader().List(ctx, restores, client.InNamespace(c.Namespace)); err != nil {
		return err
	}
	for _, restore := range restores.Items {
		if team.CloneRef == nil || !hasOwner(&restore, "VirtualMachineClone", team.CloneRef.UID) {
			continue
		}
		if restore.Spec.Target.Name != vmName || restore.Spec.VirtualMachineSnapshotName != snapshot.Name {
			failTeam(c, team, "ResourceConflict", "Restore source or target changed")
			return nil
		}
		if restore.Status != nil && snapshotConditionFailed(restore.Status.Conditions) {
			failTeam(c, team, "RestoreFailed", "Team disk restoration failed")
			return nil
		}
		if restore.Status == nil || restore.Status.Complete == nil || !*restore.Status.Complete {
			teamCondition(c, team, "Ready", false, "RestorePending", "Waiting for the disk restore")
			return nil
		}
	}
	vm := &virtv1.VirtualMachine{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: vmName, Namespace: c.Namespace}, vm); err != nil {
		if apierrors.IsNotFound(err) {
			failTeam(c, team, "VMMissing", "The restored team VM is missing; do not recreate it")
			return nil
		}
		return err
	}
	if !ownedByChallenge(vm, c) || (team.VMRef != nil && vm.UID != team.VMRef.UID) || !vm.DeletionTimestamp.IsZero() || vm.Labels[challengeUIDLabel] != string(c.UID) || vm.Annotations[challengeSnapshotAnnotation] != string(snapshot.UID) || vm.Spec.Template == nil || vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != virtv1.RunStrategyManual || vm.Spec.Running != nil || len(vm.Spec.DataVolumeTemplates) != 0 {
		failTeam(c, team, "ResourceConflict", "Team VM identity or run policy changed")
		return nil
	}
	if !equality.Semantic.DeepEqual(vm.Spec.Template.Spec.NodeSelector, c.Spec.Placement.NodeSelector) || vm.Spec.Template.ObjectMeta.Labels[challengeUIDLabel] != string(c.UID) || vm.Spec.Template.ObjectMeta.Labels[problemUIDLabel] != "" {
		failTeam(c, team, "ResourceConflict", "Team placement or management labels changed")
		return nil
	}
	volumes := vm.Spec.Template.Spec.Volumes
	if len(volumes) != 1 || volumes[0].Name != "root" || volumes[0].PersistentVolumeClaim == nil || volumes[0].PersistentVolumeClaim.ClaimName != pvcName {
		failTeam(c, team, "ResourceConflict", "Team VM does not use its dedicated root PVC")
		return nil
	}
	team.VMRef = resourceRef(vm)
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: pvcName, Namespace: c.Namespace}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			failTeam(c, team, "DiskMissing", "The restored team PVC is missing")
			return nil
		}
		return err
	}
	if err := r.adoptTeamPVC(ctx, c, team, pvc); err != nil {
		if failure, ok := err.(*challengeResourceError); ok {
			failTeam(c, team, failure.reason, failure.message)
			return nil
		}
		return err
	}
	team.PVCRef = resourceRef(pvc)
	// VMやPVCの参照を先に保存し、初回起動後の消失を新規展開として扱わない。
	if err := r.saveChallenge(ctx, c); err != nil {
		return err
	}
	if pvc.Status.Phase != corev1.ClaimBound || !pvc.DeletionTimestamp.IsZero() || vm.Status.RestoreInProgress != nil {
		teamCondition(c, team, "Ready", false, "DiskPending", "Waiting for a bound PVC and completed restore")
		return nil
	}
	return r.startAndObserveTeam(ctx, c, team, vm)
}

// 復元元のCSI snapshotと固定PVC名を照合してから所有参照を追加する。
// 元の作問PVCや他チームのPVCは採用しない。GCだけに頼らず、UIDを記録して削除時にも確認する。
func (r *ProblemChallengeReconciler) adoptTeamPVC(ctx context.Context, c *labv1alpha1.ProblemChallenge, team *labv1alpha1.ProblemChallengeTeamStatus, pvc *corev1.PersistentVolumeClaim) error {
	_, vmName, pvcName := challengeTeamNames(c, team.ID)
	snapshotName, sourceUID, err := r.rootSnapshot(ctx, c)
	if err != nil {
		return err
	}
	if pvc.Name != pvcName || pvc.UID == sourceUID || (team.PVCRef != nil && team.PVCRef.UID != pvc.UID) {
		return resourceFailure("ResourceConflict", "Team PVC identity does not match")
	}
	matches := pvc.Spec.DataSource != nil && pvc.Spec.DataSource.APIGroup != nil && *pvc.Spec.DataSource.APIGroup == "snapshot.storage.k8s.io" && pvc.Spec.DataSource.Kind == "VolumeSnapshot" && pvc.Spec.DataSource.Name == snapshotName
	if pvc.Spec.DataSourceRef != nil {
		source := pvc.Spec.DataSourceRef
		matches = matches || (source.APIGroup != nil && *source.APIGroup == "snapshot.storage.k8s.io" && source.Kind == "VolumeSnapshot" && source.Name == snapshotName && (source.Namespace == nil || *source.Namespace == c.Namespace))
	}
	if !matches {
		return resourceFailure("ResourceConflict", "Team PVC was not restored from this root snapshot")
	}
	for _, other := range c.Status.Teams {
		if other.ID != team.ID && other.PVCRef != nil && other.PVCRef.UID == pvc.UID {
			return resourceFailure("ResourceConflict", "Teams cannot share a PVC")
		}
	}
	for _, owner := range pvc.OwnerReferences {
		if owner.Kind == "ProblemChallenge" && owner.UID == c.UID {
			continue
		}
		if owner.Kind == "VirtualMachine" && team.VMRef != nil && owner.UID == team.VMRef.UID && owner.Name == vmName {
			continue
		}
		return resourceFailure("ResourceConflict", "Team PVC has an unrelated owner")
	}
	before := pvc.DeepCopy()
	if err := controllerutil.SetOwnerReference(c, pvc, r.Scheme); err != nil {
		return err
	}
	if pvc.Labels == nil {
		pvc.Labels = map[string]string{}
	}
	for key, value := range challengeLabels(c, team.ID) {
		pvc.Labels[key] = value
	}
	if equality.Semantic.DeepEqual(before.ObjectMeta, pvc.ObjectMeta) {
		return nil
	}
	return r.Update(ctx, pvc)
}

// 起動要求を送る前にVMへ記録する。送信と記録の間に落ちた場合は自動再送せず、運営による再試行に委ねる。
// startの受理とVMIの実在を分けて確認し、最初のVMIのUIDを記録して別の実行へ切り替わることを検出する。
func (r *ProblemChallengeReconciler) startAndObserveTeam(ctx context.Context, c *labv1alpha1.ProblemChallenge, team *labv1alpha1.ProblemChallengeTeamStatus, vm *virtv1.VirtualMachine) error {
	vmi := &virtv1.VirtualMachineInstance{}
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(vm), vmi)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if !hasOwner(vmi, "VirtualMachine", vm.UID) || vm.Annotations[challengeStartAttemptAnnotation] != string(c.UID) || (vm.Annotations[challengeVMIAnnotation] != "" && vm.Annotations[challengeVMIAnnotation] != string(vmi.UID)) {
			failTeam(c, team, "UnexpectedVMI", "Team VMI identity changed or was started outside the initial request")
			return nil
		}
		if vm.Annotations[challengeVMIAnnotation] == "" {
			vm.Annotations[challengeVMIAnnotation] = string(vmi.UID)
			if err := r.Update(ctx, vm); err != nil {
				return err
			}
		}
		teamCondition(c, team, "Started", true, "InitialStartObserved", "The initial VMI was observed")
		if vmi.Status.Phase == virtv1.Failed || vmi.Status.Phase == virtv1.Succeeded || !vmi.DeletionTimestamp.IsZero() {
			failTeam(c, team, "VMStopped", "The team VMI stopped; automatic recovery is disabled")
			return nil
		}
		if vmi.Status.NodeName != "" {
			node := &corev1.Node{}
			if err := r.reader().Get(ctx, types.NamespacedName{Name: vmi.Status.NodeName}, node); err != nil {
				if apierrors.IsNotFound(err) {
					failTeam(c, team, "NodeMissing", "The competition node is missing")
					return nil
				}
				return err
			}
			ready := false
			for _, condition := range node.Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			if !ready {
				failTeam(c, team, "NodeUnavailable", "The competition node is not Ready; automatic recovery is disabled")
				return nil
			}
		}
		ready := vmi.Status.Phase == virtv1.Running && vmi.Status.NodeName != "" && vm.Status.Ready && vm.Status.ObservedGeneration >= vm.Generation
		reason, message := "VMStarting", "Waiting for the initial VMI to become ready"
		if ready {
			reason, message = "VMRunning", "The team VM is ready"
		}
		teamCondition(c, team, "Ready", ready, reason, message)
		return nil
	}
	if vm.Annotations[challengeStartAttemptAnnotation] != "" {
		if vm.Annotations[challengeVMIAnnotation] != "" || apiMeta.IsStatusConditionTrue(team.Conditions, "Started") {
			failTeam(c, team, "VMStopped", "The initial VMI disappeared; do not start another VMI")
			return nil
		}
		for _, request := range vm.Status.StateChangeRequests {
			if request.Action == virtv1.StartRequest {
				teamCondition(c, team, "Ready", false, "VMStarting", "KubeVirt accepted the initial start request")
				return nil
			}
		}
		failTeam(c, team, "StartUnconfirmed", "The initial start outcome is unknown; do not automatically resend it")
		return nil
	}
	if r.StartVM == nil {
		return fmt.Errorf("KubeVirt start API is not configured")
	}
	if vm.Annotations == nil {
		vm.Annotations = map[string]string{}
	}
	vm.Annotations[challengeStartAttemptAnnotation] = string(c.UID)
	if err := r.Update(ctx, vm); err != nil {
		return err
	}
	teamCondition(c, team, "Ready", false, "VMStarting", "Submitting the initial start request")
	if err := r.StartVM(ctx, vm); err != nil {
		failTeam(c, team, "StartUnconfirmed", fmt.Sprintf("Initial start request did not return success: %v", err))
		return nil
	}
	// 成功応答の後も最新VMを使い、KubeVirtのstatus更新と競合した場合は次回にVMIを確認する。
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(vm), vm); err != nil {
		return err
	}
	if vm.Annotations == nil {
		vm.Annotations = map[string]string{}
	}
	vm.Annotations[challengeStartAcceptedAnnotation] = string(c.UID)
	return r.Update(ctx, vm)
}
