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
	"time"

	csiv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clonev1 "kubevirt.io/api/clone/v1beta1"
	virtv1 "kubevirt.io/api/core/v1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
)

// ProblemChallengeReconcilerはロック、保存、チーム展開、削除を進める。
// 競技用VMはHA対象にせず、初回起動後の障害や消失では自動復旧しない。
type ProblemChallengeReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// 初回の起動だけをKubeVirtのstart APIへ依頼する。テストではAPIの応答を差し替える。
	StartVM func(context.Context, *virtv1.VirtualMachine) error
}

// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problemchallenges,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problemchallenges/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problemchallenges/finalizers,verbs=update
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems,verbs=get;list;watch
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachines/start,verbs=update
// +kubebuilder:rbac:groups=snapshot.kubevirt.io,resources=virtualmachinesnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=snapshot.kubevirt.io,resources=virtualmachinesnapshotcontents;virtualmachinerestores,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=clone.kubevirt.io,resources=virtualmachineclones,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods;nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots;volumesnapshotcontents,verbs=get;list;watch;delete

func (r *ProblemChallengeReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// 非同期処理を1段階ずつ進める。statusと実リソースに状態を残し、controller再起動後も同じ保存結果を使う。
func (r *ProblemChallengeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c := &labv1alpha1.ProblemChallenge{}
	if err := r.reader().Get(ctx, req.NamespacedName, c); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !c.DeletionTimestamp.IsZero() {
		return r.deleteChallenge(ctx, c)
	}
	if !controllerutil.ContainsFinalizer(c, challengeCleanupFinalizer) {
		controllerutil.AddFinalizer(c, challengeCleanupFinalizer)
		if err := r.Update(ctx, c); err != nil {
			return ctrl.Result{}, err
		}
	}
	// 他のChallengeとの競合は固定する。後からロックが空いても、このChallengeでは出題しない。
	if condition := apiMeta.FindStatusCondition(c.Status.Conditions, "Accepted"); condition != nil && condition.Status == metav1.ConditionFalse && (condition.Reason == "ProblemLocked" || condition.Reason == "ProblemDeleting") {
		return ctrl.Result{}, nil
	}
	if apiMeta.IsStatusConditionTrue(c.Status.Conditions, "Failed") {
		return ctrl.Result{}, nil
	}
	problem, locked, result, err := r.acquireProblem(ctx, c)
	if err != nil || !locked {
		return result, err
	}
	for _, spec := range c.Spec.Teams {
		found := false
		for _, team := range c.Status.Teams {
			if team.ID == spec.ID {
				found = true
				break
			}
		}
		if !found {
			c.Status.Teams = append(c.Status.Teams, labv1alpha1.ProblemChallengeTeamStatus{ID: spec.ID})
		}
	}
	snapshot, ready, result, err := r.ensureSnapshot(ctx, c, problem)
	if err != nil || !ready {
		return result, err
	}
	allReady := true
	for index := range c.Status.Teams {
		if err := r.reconcileTeam(ctx, c, &c.Status.Teams[index], snapshot); err != nil {
			return ctrl.Result{}, err
		}
		allReady = allReady && apiMeta.IsStatusConditionTrue(c.Status.Teams[index].Conditions, "Ready")
	}
	reason, message := "TeamsPending", "Waiting for team VMs"
	if allReady {
		reason, message = "TeamsReady", "All team VMs are ready"
	}
	for _, team := range c.Status.Teams {
		if apiMeta.IsStatusConditionTrue(team.Conditions, "Failed") {
			reason, message = "TeamFailed", "A team VM failed; automatic recovery is disabled"
		}
	}
	challengeCondition(c, "Ready", allReady, reason, message)
	return ctrl.Result{}, r.saveChallenge(ctx, c)
}

func conditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func challengeCondition(c *labv1alpha1.ProblemChallenge, kind string, value bool, reason, message string) {
	apiMeta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: kind, Status: conditionStatus(value), Reason: reason, Message: message, ObservedGeneration: c.Generation})
}

func teamCondition(c *labv1alpha1.ProblemChallenge, team *labv1alpha1.ProblemChallengeTeamStatus, kind string, value bool, reason, message string) {
	apiMeta.SetStatusCondition(&team.Conditions, metav1.Condition{Type: kind, Status: conditionStatus(value), Reason: reason, Message: message, ObservedGeneration: c.Generation})
}

// 書き込み直前に読み直し、specや削除状態が変わったChallengeへ古い観測結果を書かない。
func (r *ProblemChallengeReconciler) saveChallenge(ctx context.Context, c *labv1alpha1.ProblemChallenge) error {
	current := &labv1alpha1.ProblemChallenge{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
		return err
	}
	if current.UID != c.UID || current.Generation != c.Generation || current.DeletionTimestamp.IsZero() != c.DeletionTimestamp.IsZero() {
		return apierrors.NewConflict(labv1alpha1.GroupVersion.WithResource("problemchallenges").GroupResource(), c.Name, fmt.Errorf("challenge changed during reconciliation"))
	}
	if equality.Semantic.DeepEqual(current.Status, c.Status) {
		return nil
	}
	current.Status = *c.Status.DeepCopy()
	if err := r.Status().Update(ctx, current); err != nil {
		return err
	}
	c.ObjectMeta = current.ObjectMeta
	return nil
}

func (r *ProblemChallengeReconciler) failChallenge(ctx context.Context, c *labv1alpha1.ProblemChallenge, reason, message string) (ctrl.Result, error) {
	challengeCondition(c, "Failed", true, reason, message)
	challengeCondition(c, "Ready", false, reason, message)
	return ctrl.Result{}, r.saveChallenge(ctx, c)
}

func (r *ProblemChallengeReconciler) waitChallenge(ctx context.Context, c *labv1alpha1.ProblemChallenge, reason, message string, poll bool) (ctrl.Result, error) {
	challengeCondition(c, "Ready", false, reason, message)
	result := ctrl.Result{}
	if poll {
		result.RequeueAfter = 5 * time.Second
	}
	return result, r.saveChallenge(ctx, c)
}

// sourceProblemを固定してから、最新ProblemのresourceVersionで排他的にロックを取得する。
func (r *ProblemChallengeReconciler) acquireProblem(ctx context.Context, c *labv1alpha1.ProblemChallenge) (*labv1alpha1.Problem, bool, ctrl.Result, error) {
	p := &labv1alpha1.Problem{}
	key := types.NamespacedName{Name: c.Spec.ProblemRef.Name, Namespace: c.Namespace}
	if err := r.reader().Get(ctx, key, p); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, ctrl.Result{}, err
		}
		if c.Status.SourceProblem != nil {
			result, e := r.failChallenge(ctx, c, "SourceProblemMissing", "The recorded Problem is missing")
			return nil, false, result, e
		}
		challengeCondition(c, "Accepted", false, "ProblemNotFound", "Waiting for the referenced Problem")
		result, e := r.waitChallenge(ctx, c, "ProblemNotFound", "Waiting for the referenced Problem", false)
		return nil, false, result, e
	}
	if c.Status.SourceProblem != nil && (c.Status.SourceProblem.UID != p.UID || c.Status.SourceProblem.Generation != p.Generation) {
		result, e := r.failChallenge(ctx, c, "SourceProblemChanged", "Problem identity or generation changed")
		return nil, false, result, e
	}
	if c.Status.SourceProblem != nil && c.Status.SourceProblem.DiskPVCUID != "" && (p.Status.DiskRef == nil || c.Status.SourceProblem.DiskPVCUID != p.Status.DiskRef.PVCUID) {
		result, e := r.failChallenge(ctx, c, "SourceProblemChanged", "Author disk PVC identity changed")
		return nil, false, result, e
	}
	if lock := p.Status.ActiveChallenge; lock != nil && lock.UID != c.UID {
		challengeCondition(c, "Accepted", false, "ProblemLocked", "Another Challenge owns the lock; recreate this Challenge to retry")
		result, e := r.waitChallenge(ctx, c, "ProblemLocked", "Another Challenge owns the lock", false)
		return nil, false, result, e
	}
	if p.Status.ActiveChallenge == nil {
		if !p.DeletionTimestamp.IsZero() {
			challengeCondition(c, "Accepted", false, "ProblemDeleting", "Do not accept a Problem being deleted")
			result, e := r.waitChallenge(ctx, c, "ProblemDeleting", "Do not accept a Problem being deleted", false)
			return nil, false, result, e
		}
		if apiMeta.IsStatusConditionTrue(c.Status.Conditions, "Accepted") {
			result, e := r.failChallenge(ctx, c, "LockLost", "The accepted Challenge no longer owns the lock")
			return nil, false, result, e
		}
		ready, err := r.sourceReady(ctx, p)
		if err != nil {
			return nil, false, ctrl.Result{}, err
		}
		if !ready {
			challengeCondition(c, "Accepted", false, "ProblemNotReady", "Waiting for the author VM and disk")
			result, e := r.waitChallenge(ctx, c, "ProblemNotReady", "Waiting for the author VM and disk", false)
			return nil, false, result, e
		}
		if c.Status.SourceProblem == nil || c.Status.SourceProblem.DiskPVCUID == "" {
			if p.Status.DiskRef == nil || p.Status.DiskRef.PVCUID == "" {
				return nil, false, ctrl.Result{Requeue: true}, nil
			}
			if c.Status.SourceProblem == nil {
				c.Status.SourceProblem = &labv1alpha1.SourceProblemReference{Name: p.Name, UID: p.UID, Generation: p.Generation}
			}
			c.Status.SourceProblem.DiskPVCUID = p.Status.DiskRef.PVCUID
			if err := r.saveChallenge(ctx, c); err != nil {
				return nil, false, ctrl.Result{}, err
			}
			// status保存中のProblem変更を、ロック取得前に検出する。
			if err := r.reader().Get(ctx, key, p); err != nil {
				return nil, false, ctrl.Result{}, err
			}
			if p.UID != c.Status.SourceProblem.UID || p.Generation != c.Status.SourceProblem.Generation {
				return nil, false, ctrl.Result{Requeue: true}, nil
			}
			if p.Status.ActiveChallenge != nil {
				return nil, false, ctrl.Result{Requeue: true}, nil
			}
			if !p.DeletionTimestamp.IsZero() {
				return nil, false, ctrl.Result{Requeue: true}, nil
			}
		}
		p.Status.ActiveChallenge = &labv1alpha1.ActiveChallengeStatus{ActiveChallengeName: c.Name, UID: c.UID, ProblemGeneration: p.Generation}
		if err := r.Status().Update(ctx, p); err != nil {
			// 更新競合後に勝者を確認する。別UIDが取得していれば、このChallengeは待ち直さず拒否する。
			if !apierrors.IsConflict(err) {
				return nil, false, ctrl.Result{}, err
			}
			if getErr := r.reader().Get(ctx, key, p); getErr != nil {
				return nil, false, ctrl.Result{}, getErr
			}
			if p.Status.ActiveChallenge != nil && p.Status.ActiveChallenge.UID != c.UID {
				challengeCondition(c, "Accepted", false, "ProblemLocked", "Another Challenge won the lock")
				result, e := r.waitChallenge(ctx, c, "ProblemLocked", "Another Challenge won the lock", false)
				return nil, false, result, e
			}
			return nil, false, ctrl.Result{}, err
		}
	}
	// 取得直後やcontroller再起動後に、保存した参照とロック所有者を照合する。
	if err := r.reader().Get(ctx, key, p); err != nil {
		return nil, false, ctrl.Result{}, err
	}
	if c.Status.SourceProblem == nil || p.UID != c.Status.SourceProblem.UID || p.Generation != c.Status.SourceProblem.Generation || p.Status.ActiveChallenge == nil || p.Status.ActiveChallenge.UID != c.UID || p.Status.ActiveChallenge.ActiveChallengeName != c.Name || p.Status.ActiveChallenge.ProblemGeneration != p.Generation {
		result, e := r.failChallenge(ctx, c, "SourceProblemChanged", "Source or lock changed during acceptance")
		return nil, false, result, e
	}
	if p.Status.DiskRef == nil {
		result, e := r.failChallenge(ctx, c, "SourceDiskMissing", "The recorded author disk is missing")
		return nil, false, result, e
	}
	if p.Status.DiskRef.PVCUID == "" {
		// 古いProblem statusにはPVC UIDがないため、ロック所有者が確認したPVCを一度だけ記録する。
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.reader().Get(ctx, types.NamespacedName{Name: p.Status.DiskRef.DiskName, Namespace: p.Namespace}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				result, e := r.failChallenge(ctx, c, "SourceDiskMissing", "The author root PVC is missing")
				return nil, false, result, e
			}
			return nil, false, ctrl.Result{}, err
		}
		if !ownedByDataVolume(pvc, p.Status.DiskRef.UID) || pvc.Status.Phase != corev1.ClaimBound || !pvc.DeletionTimestamp.IsZero() {
			result, e := r.failChallenge(ctx, c, "ResourceConflict", "The author root PVC cannot be verified")
			return nil, false, result, e
		}
		p.Status.DiskRef.PVCUID = pvc.UID
		if err := r.Status().Update(ctx, p); err != nil {
			return nil, false, ctrl.Result{}, err
		}
	}
	if c.Status.SourceProblem.DiskPVCUID == "" {
		c.Status.SourceProblem.DiskPVCUID = p.Status.DiskRef.PVCUID
	} else if c.Status.SourceProblem.DiskPVCUID != p.Status.DiskRef.PVCUID {
		result, e := r.failChallenge(ctx, c, "SourceProblemChanged", "Author disk PVC identity changed")
		return nil, false, result, e
	}
	challengeCondition(c, "Accepted", true, "ProblemAccepted", "The Challenge owns the author VM lock")
	if err := r.saveChallenge(ctx, c); err != nil {
		return nil, false, ctrl.Result{}, err
	}
	return p, true, ctrl.Result{}, nil
}

// status表示だけに頼らず、同じUIDのVM、DataVolume、PVCが実際に準備できているか確認する。
func (r *ProblemChallengeReconciler) sourceReady(ctx context.Context, p *labv1alpha1.Problem) (bool, error) {
	condition := apiMeta.FindStatusCondition(p.Status.Conditions, "VMReady")
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != p.Generation || p.Status.VMRef == nil || p.Status.DiskRef == nil {
		return false, nil
	}
	vm := &virtv1.VirtualMachine{}
	dv := &cdiv1.DataVolume{}
	pvc := &corev1.PersistentVolumeClaim{}
	for _, object := range []client.Object{vm, dv, pvc} {
		name := p.Status.DiskRef.DiskName
		if object == vm {
			name = p.Status.VMRef.VMName
		}
		if err := r.reader().Get(ctx, types.NamespacedName{Name: name, Namespace: p.Namespace}, object); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
	}
	return ownedByProblem(vm, p) && vm.UID == p.Status.VMRef.UID && vm.Status.Ready && vm.Status.ObservedGeneration >= vm.Generation && vm.DeletionTimestamp.IsZero() && ownedByProblem(dv, p) && dv.UID == p.Status.DiskRef.UID && dv.Status.Phase == cdiv1.Succeeded && dv.DeletionTimestamp.IsZero() && ownedByDataVolume(pvc, dv.UID) && p.Status.DiskRef.PVCUID != "" && pvc.UID == p.Status.DiskRef.PVCUID && pvc.Status.Phase == corev1.ClaimBound && pvc.DeletionTimestamp.IsZero(), nil
}

// 親Problemの変更を、同じnamespaceでその名前を参照するChallengeへ通知する。
func (r *ProblemChallengeReconciler) problemChanged(ctx context.Context, object client.Object) []reconcile.Request {
	return r.challengesForProblem(ctx, object.GetNamespace(), object.GetName())
}

// SetupWithManagerは親子の監視とKubeVirtへの初回start APIを登録する。
func (r *ProblemChallengeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.StartVM == nil {
		starter, err := newChallengeVMStarter(mgr.GetConfig(), r.Scheme)
		if err != nil {
			return err
		}
		r.StartVM = starter
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &labv1alpha1.ProblemChallenge{}, challengeProblemIndex, func(object client.Object) []string {
		return []string{object.(*labv1alpha1.ProblemChallenge).Spec.ProblemRef.Name}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).For(&labv1alpha1.ProblemChallenge{}).
		Owns(&snapshotv1.VirtualMachineSnapshot{}).Owns(&clonev1.VirtualMachineClone{}).Owns(&virtv1.VirtualMachine{}).
		Watches(&labv1alpha1.Problem{}, handler.EnqueueRequestsFromMapFunc(r.problemChanged)).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.childChanged)).
		Watches(&virtv1.VirtualMachineInstance{}, handler.EnqueueRequestsFromMapFunc(r.childChanged)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.childChanged)).
		Watches(&snapshotv1.VirtualMachineRestore{}, handler.EnqueueRequestsFromMapFunc(r.childChanged)).
		Watches(&snapshotv1.VirtualMachineSnapshotContent{}, handler.EnqueueRequestsFromMapFunc(r.childChanged)).
		Watches(&csiv1.VolumeSnapshot{}, handler.EnqueueRequestsFromMapFunc(r.childChanged)).
		Watches(&csiv1.VolumeSnapshotContent{}, handler.EnqueueRequestsFromMapFunc(r.childChanged)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.nodeChanged)).Named("problemchallenge").Complete(r)
}

// startはkubevirt.ioの通常のリソース操作ではなく、subresources.kubevirt.ioへPUTする。
func newChallengeVMStarter(source *rest.Config, scheme *runtime.Scheme) (func(context.Context, *virtv1.VirtualMachine) error, error) {
	config := rest.CopyConfig(source)
	config.GroupVersion = &virtv1.SubresourceGroupVersions[0]
	config.APIPath = "/apis"
	config.ContentType = "application/json"
	config.NegotiatedSerializer = serializer.NewCodecFactory(scheme).WithoutConversion()
	endpoint, err := rest.RESTClientFor(config)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, vm *virtv1.VirtualMachine) error {
		return endpoint.Put().SetHeader("Content-Type", "application/json").AbsPath("/apis/subresources.kubevirt.io/v1/namespaces", vm.Namespace, "virtualmachines", vm.Name, "start").Body([]byte("{}")).Do(ctx).Error()
	}, nil
}
