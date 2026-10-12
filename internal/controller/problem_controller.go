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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	virtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
)

// ProblemReconcilerは、同じ永続ディスクで作問VMの稼働と復旧を管理する。
// VMの再作成を許可するのは未ロック時だけで、ノード障害を理由にロックを解除しない。
// 別ノードへの復旧には、Kubernetes/KubeVirt/controllerとストレージの可用性が別途必要。
type ProblemReconciler struct {
	client.Client
	// APIReaderはキャッシュを通さずに読むためのClient、SchemeはAPI型とownerReferenceの対応に使う。
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// StorageClassは初回取り込み先のPVCに指定する。managerの起動引数から受け取る。
	StorageClass string
}

// 以下のマーカーから、controllerが各リソースを操作するためのRBACを生成する。
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *ProblemReconciler) reader() client.Reader {
	// 起動や所有関係の判断にはAPIReaderで最新の状態を読み、キャッシュの反映待ちを避ける。
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// Reconcileは通知されたProblemについて、実際の状態をspecが要求する状態へ近づける。
// 通常はディスク取り込み、VMの作成や設定変更、Readyの確認を順に進める。
// 取り込みや起動の完了をこの関数内で待たず、関連リソースの更新通知で続きを処理する。
func (r *ProblemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// リクエストにはnamespaceと名前だけが入る。毎回最新のProblemを取得して判断する。
	problem := &labv1alpha1.Problem{}
	if err := r.reader().Get(ctx, req.NamespacedName, problem); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.StorageClass == "" {
		return r.problemState(ctx, problem, "StorageClassRequired", "Configure --storage-class before provisioning", false, false, nil, nil)
	}
	if !problem.DeletionTimestamp.IsZero() {
		// DELETE要求を受けたProblemは起動処理へ進めず、子リソースの片付けへ回す。
		return r.deleteProblem(ctx, problem)
	}
	if !controllerutil.ContainsFinalizer(problem, problemCleanupFinalizer) {
		// 子リソースを作る前にfinalizerを付け、片付けが終わるまでProblemをAPI上に残す。
		controllerutil.AddFinalizer(problem, problemCleanupFinalizer)
		if err := r.Update(ctx, problem); err != nil {
			return ctrl.Result{}, err
		}
	}
	// UIDから決まる名前で既存VMを探す。同名でも別のProblemが所有するVMには触らない。
	vmName, diskName := problemResourceNames(problem)
	vm := &virtv1.VirtualMachine{}
	vmErr := r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: vmName}, vm)
	if vmErr != nil && !apierrors.IsNotFound(vmErr) {
		return ctrl.Result{}, vmErr
	}
	if vmErr == nil && !ownedByProblem(vm, problem) {
		return r.problemState(ctx, problem, "ResourceConflict", "VM name is occupied by another resource", false, false, nil, nil)
	}
	// ロック中はディスクの確認より先に停止する。ディスクが壊れていてもVMの停止は進める。
	if problem.Status.ActiveChallenge != nil {
		return r.haltLockedProblem(ctx, problem, vm, vmErr)
	}
	// DataVolumeは初回取り込みをCDIへ依頼するリソースで、実際の作問データは対応するPVCに入る。
	dv := &cdiv1.DataVolume{}
	diskErr := r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: diskName}, dv)
	if apierrors.IsNotFound(diskErr) {
		if problem.Status.DiskRef != nil {
			// HAで復旧する対象は作問済みのディスク。喪失を初期イメージで埋め合わせない。
			return r.problemState(ctx, problem, "DiskMissing", "Author disk is missing; automatic reimport would discard author data", false, false, nil, nil)
		}
		// 初回でも、同名PVCがすでにあれば上書きしない。他者のPVCや残存ディスクを保護する。
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: diskName}, pvc)
		if err == nil {
			return r.problemState(ctx, problem, "ResourceConflict", "Root PVC name is already occupied", false, false, nil, nil)
		}
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		dv = problemDataVolume(problem, r.StorageClass)
		// Problemを所有者にすることで、GCとOwnsによる変更通知の対象にする。
		if err := controllerutil.SetControllerReference(problem, dv, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, dv); err != nil {
			return ctrl.Result{}, err
		}
	} else if diskErr != nil {
		return ctrl.Result{}, diskErr
	}
	// 名前に加えて所有者と記録済みUIDを照合し、差し替えられたDataVolumeを使わない。
	if !ownedByProblem(dv, problem) || (problem.Status.DiskRef != nil && problem.Status.DiskRef.UID != dv.UID) {
		return r.problemState(ctx, problem, "ResourceConflict", "DataVolume identity does not match the author disk", false, false, nil, nil)
	}
	// 取り込みの途中でもDataVolumeのUIDをstatusへ記録し、次回以降に同じディスクか判別できるようにする。
	diskRef := &labv1alpha1.ProblemDiskReference{DiskName: dv.Name, UID: dv.UID}
	if !dv.DeletionTimestamp.IsZero() {
		return r.problemState(ctx, problem, "DiskDeleting", "Author disk is being deleted", false, false, nil, diskRef)
	}
	if dv.Status.Phase == cdiv1.Failed {
		return r.problemState(ctx, problem, "DiskImportFailed", "Initial disk import failed", false, false, nil, diskRef)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: diskName}, pvc)
	if apierrors.IsNotFound(err) {
		// 取り込み前のPVC未作成は待機できるが、Succeeded後のPVC消失はディスク喪失として扱う。
		reason := "DiskImportPending"
		if dv.Status.Phase == cdiv1.Succeeded {
			reason = "DiskMissing"
		}
		return r.problemState(ctx, problem, reason, "Waiting for the imported root PVC; do not recreate a completed import", false, false, nil, diskRef)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ownedByDataVolume(pvc, dv.UID) {
		return r.problemState(ctx, problem, "ResourceConflict", "Root PVC does not belong to the author DataVolume", false, false, nil, diskRef)
	}
	// CDIの取り込み完了とPVCの割り当てを両方確認するまで、VMにはディスクを接続しない。
	if dv.Status.Phase != cdiv1.Succeeded || pvc.Status.Phase != corev1.ClaimBound || !pvc.DeletionTimestamp.IsZero() {
		return r.problemState(ctx, problem, "DiskImportPending", "Waiting for CDI import completion and a bound PVC", false, false, nil, diskRef)
	}
	// ディスクを調べている間にChallengeがロックを取得する場合があるため、起動前にProblemを読み直す。
	if err := r.reader().Get(ctx, req.NamespacedName, problem); err != nil {
		return ctrl.Result{}, err
	}
	if problem.Status.ActiveChallenge != nil || !problem.DeletionTimestamp.IsZero() {
		// 更新後の状態で再判断するため、起動せずにもう一度Reconcileへ回す。
		return ctrl.Result{Requeue: true}, nil
	}
	desired := problemVirtualMachine(problem)
	if apierrors.IsNotFound(vmErr) {
		// VM自体を失っても、上で所有関係と取り込み完了を確認した既存PVCから再作成する。
		// VMI単体の障害処理と再起動はKubeVirtに委ね、ここでは強制削除しない。
		if err := controllerutil.SetControllerReference(problem, desired, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
		ref := &labv1alpha1.ProblemVMReference{VMName: desired.Name, UID: desired.UID}
		return r.problemState(ctx, problem, "VMStarting", "Waiting for the author VM to become ready", false, false, ref, diskRef)
	}
	if !vm.DeletionTimestamp.IsZero() {
		// 古いVMの終了と削除を待つ。同じ名前で新しいVMを先に起動しない。
		return r.problemState(ctx, problem, "VMDeleting", "Waiting for author VM deletion before recreating it with the same disk", false, false, nil, diskRef)
	}
	if vm.Spec.Template == nil {
		return r.problemState(ctx, problem, "ResourceConflict", "Author VM has no instance template", false, false, nil, diskRef)
	}
	// 管理するroot PVC以外が接続されていたら、設定変更で上書きせず競合として報告する。
	volumes := vm.Spec.Template.Spec.Volumes
	if len(volumes) != 1 || volumes[0].Name != "root" || volumes[0].PersistentVolumeClaim == nil || volumes[0].PersistentVolumeClaim.ClaimName != diskName {
		return r.problemState(ctx, problem, "ResourceConflict", "Author VM does not reference the managed root PVC", false, false, nil, diskRef)
	}
	ref := &labv1alpha1.ProblemVMReference{VMName: vm.Name, UID: vm.UID}
	// CPUとゲストメモリだけを比較する。KubeVirtが補完した他の設定は差分に含めない。
	domain := &vm.Spec.Template.Spec.Domain
	resize := domain.CPU == nil || domain.CPU.Cores != desired.Spec.Template.Spec.Domain.CPU.Cores ||
		domain.Memory == nil || domain.Memory.Guest == nil || domain.Memory.Guest.Cmp(problem.Spec.Machine.Memory) != 0
	if resize {
		// 設定変更は停止後に行う。起動中のVMIへCPUやメモリを直接反映する処理ではない。
		stopped, stopErr := r.problemStopped(ctx, problem, vm)
		if stopErr != nil {
			return ctrl.Result{}, stopErr
		}
		if !stopped {
			if vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != virtv1.RunStrategyHalted || vm.Spec.Running != nil {
				halted := virtv1.RunStrategyHalted
				vm.Spec.RunStrategy = &halted
				vm.Spec.Running = nil
				if err := r.Update(ctx, vm); err != nil {
					return ctrl.Result{}, err
				}
			}
			result, stateErr := r.problemState(ctx, problem, "VMReconfiguring", "Stopping the author VM before changing CPU or memory", false, false, ref, diskRef)
			// VMIやPodの変更通知に加え、停止待ちを5秒ごとに確認する。
			result.RequeueAfter = 5 * time.Second
			return result, stateErr
		}
		// VMIとPodが消えたので、VMの次回起動用テンプレートを更新する。root PVCは変更しない。
		if domain.CPU == nil {
			domain.CPU = &virtv1.CPU{}
		}
		domain.CPU.Cores = desired.Spec.Template.Spec.Domain.CPU.Cores
		if domain.Memory == nil {
			domain.Memory = &virtv1.Memory{}
		}
		domain.Memory.Guest = desired.Spec.Template.Spec.Domain.Memory.Guest
		vm.Spec.Template.Spec.Domain.Resources.Requests = desired.Spec.Template.Spec.Domain.Resources.Requests
	}
	// 設定変更後やロック解除後はAlwaysへ戻し、既存PVCからの起動をKubeVirtへ依頼する。
	// runningとrunStrategyは併用できないため、古いrunning指定は外す。
	if resize || vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != virtv1.RunStrategyAlways || vm.Spec.Running != nil {
		running := virtv1.RunStrategyAlways
		vm.Spec.RunStrategy = &running
		vm.Spec.Running = nil
		if err := r.Update(ctx, vm); err != nil {
			return ctrl.Result{}, err
		}
		return r.problemState(ctx, problem, "VMStarting", "Starting the author VM with the existing root disk", false, false, ref, diskRef)
	}
	// KubeVirtが現在のVM世代を確認したうえでReadyになったことを確認し、再起動前のReadyを使わない。
	ready := vm.Status.Ready && vm.Status.ObservedGeneration >= vm.Generation
	for _, condition := range vm.Status.Conditions {
		if condition.Type == virtv1.VirtualMachineReady {
			ready = ready && condition.Status == corev1.ConditionTrue
		}
	}
	reason, message := "VMStarting", "Waiting for the author VM to become ready"
	if ready {
		reason, message = "VMRunning", "Author VM is ready"
	}
	return r.problemState(ctx, problem, reason, message, ready, false, ref, diskRef)
}

// haltLockedProblemは、Challengeのロックが残る間、作問VMを停止した状態へ近づける。
// 停止完了はQuiescedで伝え、時間切れでもロック解除や独自の強制停止は行わない。
func (r *ProblemReconciler) haltLockedProblem(ctx context.Context, problem *labv1alpha1.Problem, vm *virtv1.VirtualMachine, vmErr error) (ctrl.Result, error) {
	if vmErr == nil {
		// Haltedへの更新は停止の依頼であり、VMIとPodが消えるまで停止完了とは扱わない。
		if vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != virtv1.RunStrategyHalted || vm.Spec.Running != nil {
			halted := virtv1.RunStrategyHalted
			vm.Spec.RunStrategy = &halted
			vm.Spec.Running = nil
			if err := r.Update(ctx, vm); err != nil {
				return ctrl.Result{}, err
			}
		}
		stopped, err := r.problemStopped(ctx, problem, vm)
		if err != nil {
			return ctrl.Result{}, err
		}
		ref := &labv1alpha1.ProblemVMReference{VMName: vm.Name, UID: vm.UID}
		reason, message := "ChallengeLocked", "Author VM remains halted while the Challenge owns the lock"
		// LockedがTrueになった時刻から停止待ちの時間を測る。再調整のたびに開始時刻を戻さない。
		if condition := apiMeta.FindStatusCondition(problem.Status.Conditions, "Locked"); !stopped && condition != nil && condition.Status == metav1.ConditionTrue && time.Since(condition.LastTransitionTime.Time) >= 5*time.Minute {
			reason, message = "ShutdownTimeout", "Author VM did not stop within five minutes; keep the lock and do not force stop"
		}
		result, err := r.problemState(ctx, problem, reason, message, false, stopped, ref, nil)
		if !stopped && reason != "ShutdownTimeout" {
			result.RequeueAfter = 5 * time.Second
		}
		return result, err
	}
	// 保存処理中にVMが失われても再作成せず、Challengeへ異常を伝える状態として記録する。
	return r.problemState(ctx, problem, "VMMissing", "Locked author VM is missing; do not recreate it during snapshot processing", false, false, nil, nil)
}

// ownedByProblemは、controllerとして指定された所有者がこのProblem本人かをUIDで確認する。
func ownedByProblem(object client.Object, problem *labv1alpha1.Problem) bool {
	owner := metav1.GetControllerOf(object)
	return owner != nil && owner.UID == problem.UID && owner.Kind == "Problem" && owner.APIVersion == labv1alpha1.GroupVersion.String()
}

// ownedByDataVolumeは、PVCのownerReferencesが記録したDataVolumeのUIDを含むか確認する。
// DataVolumeが削除済みでも、保存したUIDから残存PVCの所有関係を確認できる。
func ownedByDataVolume(object client.Object, uid types.UID) bool {
	for _, owner := range object.GetOwnerReferences() {
		if owner.UID == uid && owner.Kind == "DataVolume" && owner.APIVersion == cdiv1.SchemeGroupVersion.String() {
			return true
		}
	}
	return false
}

// problemStoppedは、VMの実行を担うVMIとlauncher Podが両方消えたかを調べる。
func (r *ProblemReconciler) problemStopped(ctx context.Context, problem *labv1alpha1.Problem, vm *virtv1.VirtualMachine) (bool, error) {
	// ノードの応答停止やVMIのFailedだけでは停止完了にしない。VMIとPodの消失を待つ。
	// API上の消失は実際の停止を保証しないため、障害時に管理者や基盤が強制削除する場合は、
	// 旧VMの停止またはディスクへの書き込み遮断を確認してから行う必要がある。
	vmi := &virtv1.VirtualMachineInstance{}
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(vm), vmi)
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	// VMI削除後もlauncher Podがディスクを使っている場合があるため、ProblemのUIDでPodを探す。
	pods := &corev1.PodList{}
	if err := r.reader().List(ctx, pods, client.InNamespace(problem.Namespace), client.MatchingLabels{problemUIDLabel: string(problem.UID), virtv1.AppLabel: "virt-launcher"}); err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}

// problemStateは、観測したVMとディスクの参照、VMReady/Locked/QuiescedをProblemのstatusへ反映する。
func (r *ProblemReconciler) problemState(ctx context.Context, observed *labv1alpha1.Problem, reason, message string, ready, stopped bool, vmRef *labv1alpha1.ProblemVMReference, diskRef *labv1alpha1.ProblemDiskReference) (ctrl.Result, error) {
	// 最新statusを取り直し、Challengeが書いたactiveChallengeを残して参照とconditionsだけを更新する。
	// Status().UpdateのresourceVersion競合で並行更新を検出し、別の処理が取得したロックを消さない。
	current := &labv1alpha1.Problem{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(observed), current); err != nil {
		return ctrl.Result{}, err
	}
	if current.UID != observed.UID || current.Generation != observed.Generation {
		// 別のProblemや変更前のspecについて観測した結果は書き込まず、再調整へ回す。
		return ctrl.Result{}, apierrors.NewConflict(labv1alpha1.GroupVersion.WithResource("problems").GroupResource(), current.Name, fmt.Errorf("problem changed during reconciliation"))
	}
	// nilの参照は既存値を残す。異常時や削除途中にもディスクの所有UIDを保持する。
	before := current.Status.DeepCopy()
	if vmRef != nil {
		current.Status.VMRef = vmRef
	}
	if diskRef != nil {
		current.Status.DiskRef = diskRef
	}
	locked := current.Status.ActiveChallenge != nil
	// 観測中にロックの有無が変わった場合、その観測結果ではReadyを更新しない。
	if locked != (observed.Status.ActiveChallenge != nil) {
		return ctrl.Result{Requeue: true}, nil
	}
	state := func(value bool) metav1.ConditionStatus {
		if value {
			return metav1.ConditionTrue
		}
		return metav1.ConditionFalse
	}
	// VMReadyは稼働状態、Lockedはロックの有無、Quiescedはロック中の停止完了を示す。
	// SetStatusConditionはtypeごとに条件を更新し、状態が変わったときに時刻を更新する。
	for _, condition := range []metav1.Condition{
		{Type: "VMReady", Status: state(ready), Reason: reason, Message: message, ObservedGeneration: current.Generation},
		{Type: "Locked", Status: state(locked), Reason: "Unlocked", Message: "No Challenge owns the lock", ObservedGeneration: current.Generation},
		{Type: "Quiesced", Status: state(stopped && locked), Reason: reason, Message: message, ObservedGeneration: current.Generation},
	} {
		if condition.Type == "Locked" && locked {
			condition.Reason, condition.Message = "ChallengeLocked", "A Challenge owns the author VM lock"
		}
		apiMeta.SetStatusCondition(&current.Status.Conditions, condition)
	}
	if equality.Semantic.DeepEqual(before, &current.Status) {
		// 同じstatusを繰り返し書かず、自分の更新通知だけでReconcileが回り続けることを避ける。
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Status().Update(ctx, current)
}

// deleteProblemは、finalizerでProblemの削除を保留しながら、所有するVMとディスクを片付ける。
func (r *ProblemReconciler) deleteProblem(ctx context.Context, problem *labv1alpha1.Problem) (ctrl.Result, error) {
	// ロック中は作問ディスクを保持する。解除後にVM/VMI/Pod、DataVolume/PVCの順で片付け、
	// すべての削除を確認してからProblemのfinalizerを外す。削除時にも所有UIDを照合する。
	if !controllerutil.ContainsFinalizer(problem, problemCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	if problem.Status.ActiveChallenge != nil {
		// Challengeの保存や展開に使うディスクを残し、作問VMの停止だけを進める。
		vmName, _ := problemResourceNames(problem)
		vm := &virtv1.VirtualMachine{}
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: vmName}, vm)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil && !ownedByProblem(vm, problem) {
			return r.problemState(ctx, problem, "ResourceConflict", "Do not change another owner's VM", false, false, nil, nil)
		}
		return r.haltLockedProblem(ctx, problem, vm, err)
	}
	vmName, diskName := problemResourceNames(problem)
	vm := &virtv1.VirtualMachine{}
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: vmName}, vm)
	if err == nil {
		if !ownedByProblem(vm, problem) {
			return r.problemState(ctx, problem, "ResourceConflict", "Do not delete another owner's VM", false, false, nil, nil)
		}
		if vm.DeletionTimestamp.IsZero() {
			// Foregroundで子VMIの削除を待たせ、UID指定で読み取り後に差し替わったVMの削除を防ぐ。
			if err := r.Delete(ctx, vm, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &vm.UID}); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	// VMが消えた後も、VMIとPodが残っていないことを確認してからディスクを削除する。
	stopped, err := r.problemStopped(ctx, problem, &virtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: problem.Namespace}})
	if err != nil || !stopped {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}
	dv := &cdiv1.DataVolume{}
	err = r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: diskName}, dv)
	if err == nil {
		if !ownedByProblem(dv, problem) || (problem.Status.DiskRef != nil && problem.Status.DiskRef.UID != dv.UID) {
			return r.problemState(ctx, problem, "ResourceConflict", "Do not delete another owner's DataVolume", false, false, nil, nil)
		}
		// DataVolume作成直後にcontrollerが落ちると、statusへのUID記録がまだない場合がある。
		// 先にUIDを保存し、DataVolume削除後にPVCだけ残っても所有関係を確認できるようにする。
		if problem.Status.DiskRef == nil {
			result, stateErr := r.problemState(ctx, problem, "CleanupPending", "Recording author disk identity before cleanup", false, false, nil,
				&labv1alpha1.ProblemDiskReference{DiskName: dv.Name, UID: dv.UID})
			result.RequeueAfter = 5 * time.Second
			return result, stateErr
		}
		if err := r.Delete(ctx, dv, client.Preconditions{UID: &dv.UID}); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	// DataVolumeの削除後にPVCが残った場合も、保存した所有UIDを照合して片付ける。
	pvc := &corev1.PersistentVolumeClaim{}
	err = r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: diskName}, pvc)
	if err == nil {
		if problem.Status.DiskRef == nil || !ownedByDataVolume(pvc, problem.Status.DiskRef.UID) {
			return r.problemState(ctx, problem, "ResourceConflict", "Cannot prove root PVC ownership", false, false, nil, nil)
		}
		if err := r.Delete(ctx, pvc, client.Preconditions{UID: &pvc.UID}); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	// 子リソースが消えた後にロックを再確認してから、Problem自身の削除を許可する。
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(problem), problem); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if problem.Status.ActiveChallenge != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	controllerutil.RemoveFinalizer(problem, problemCleanupFinalizer)
	return ctrl.Result{}, r.Update(ctx, problem)
}

// problemEventsは、VMI/Pod/PVCの更新を、そのリソースを使うProblemの再調整リクエストへ変換する。
func (r *ProblemReconciler) problemEvents(ctx context.Context, object client.Object) []reconcile.Request {
	// VMIとlauncher PodにはVMテンプレートからProblemのUIDラベルが引き継がれる。
	uid := object.GetLabels()[problemUIDLabel]
	if uid == "" {
		// CDIが作ったPVCにはラベルがない場合があるので、ownerReferenceからDataVolumeをたどる。
		for _, owner := range object.GetOwnerReferences() {
			if owner.Kind == "DataVolume" {
				dv := &cdiv1.DataVolume{}
				if err := r.Get(ctx, types.NamespacedName{Namespace: object.GetNamespace(), Name: owner.Name}, dv); err == nil && owner.UID == dv.UID {
					uid = dv.Labels[problemUIDLabel]
				}
			}
		}
	}
	if uid == "" {
		return nil
	}
	// 同じnamespaceでUIDが一致するProblemだけを通知する。無関係なリソースの更新は無視する。
	problems := &labv1alpha1.ProblemList{}
	if err := r.List(ctx, problems, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	for _, problem := range problems.Items {
		if string(problem.UID) == uid {
			return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&problem)}}
		}
	}
	return nil
}

// SetupWithManagerは、Problemと関連リソースの監視をmanagerへ登録する。
func (r *ProblemReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.StorageClass == "" {
		return fmt.Errorf("--storage-class is required")
	}
	// VMとDataVolumeはProblemの直接の子なのでOwnsで通知できる。
	// VMI/Pod/PVCは直接の子ではないため、WatchesとproblemEventsでProblemへ結び付ける。
	return ctrl.NewControllerManagedBy(mgr).
		For(&labv1alpha1.Problem{}).
		Owns(&virtv1.VirtualMachine{}).
		Owns(&cdiv1.DataVolume{}).
		Watches(&virtv1.VirtualMachineInstance{}, handler.EnqueueRequestsFromMapFunc(r.problemEvents)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.problemEvents)).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.problemEvents)).
		Named("problem").Complete(r)
}
