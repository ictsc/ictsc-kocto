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

type ProblemReconciler struct {
	client.Client
	APIReader    client.Reader
	Scheme       *runtime.Scheme
	StorageClass string
}

// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *ProblemReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ProblemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	problem := &labv1alpha1.Problem{}
	if err := r.reader().Get(ctx, req.NamespacedName, problem); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.StorageClass == "" {
		return r.problemState(ctx, problem, "StorageClassRequired", "Configure --storage-class before provisioning", false, false, nil, nil)
	}
	if !problem.DeletionTimestamp.IsZero() {
		return r.deleteProblem(ctx, problem)
	}
	if !controllerutil.ContainsFinalizer(problem, problemCleanupFinalizer) {
		controllerutil.AddFinalizer(problem, problemCleanupFinalizer)
		if err := r.Update(ctx, problem); err != nil {
			return ctrl.Result{}, err
		}
	}
	vmName, diskName := problemResourceNames(problem)
	vm := &virtv1.VirtualMachine{}
	vmErr := r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: vmName}, vm)
	if vmErr != nil && !apierrors.IsNotFound(vmErr) {
		return ctrl.Result{}, vmErr
	}
	if vmErr == nil && !ownedByProblem(vm, problem) {
		return r.problemState(ctx, problem, "ResourceConflict", "VM name is occupied by another resource", false, false, nil, nil)
	}
	// Stop an existing VM before checking the disk. A damaged disk must not prevent a lock from stopping the VM.
	if problem.Status.ActiveChallenge != nil {
		return r.haltLockedProblem(ctx, problem, vm, vmErr)
	}
	dv := &cdiv1.DataVolume{}
	diskErr := r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: diskName}, dv)
	if apierrors.IsNotFound(diskErr) {
		if problem.Status.DiskRef != nil {
			return r.problemState(ctx, problem, "DiskMissing", "Author disk is missing; automatic reimport would discard author data", false, false, nil, nil)
		}
		// Do not import over a PVC that was created by someone else, or left behind after a lost DataVolume.
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: problem.Namespace, Name: diskName}, pvc)
		if err == nil {
			return r.problemState(ctx, problem, "ResourceConflict", "Root PVC name is already occupied", false, false, nil, nil)
		}
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		dv = problemDataVolume(problem, r.StorageClass)
		if err := controllerutil.SetControllerReference(problem, dv, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, dv); err != nil {
			return ctrl.Result{}, err
		}
	} else if diskErr != nil {
		return ctrl.Result{}, diskErr
	}
	if !ownedByProblem(dv, problem) || (problem.Status.DiskRef != nil && problem.Status.DiskRef.UID != dv.UID) {
		return r.problemState(ctx, problem, "ResourceConflict", "DataVolume identity does not match the author disk", false, false, nil, nil)
	}
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
	if dv.Status.Phase != cdiv1.Succeeded || pvc.Status.Phase != corev1.ClaimBound || !pvc.DeletionTimestamp.IsZero() {
		return r.problemState(ctx, problem, "DiskImportPending", "Waiting for CDI import completion and a bound PVC", false, false, nil, diskRef)
	}
	// Re-read the parent before starting or changing a VM, so a newly acquired lock is observed.
	if err := r.reader().Get(ctx, req.NamespacedName, problem); err != nil {
		return ctrl.Result{}, err
	}
	if problem.Status.ActiveChallenge != nil || !problem.DeletionTimestamp.IsZero() {
		return ctrl.Result{Requeue: true}, nil
	}
	desired := problemVirtualMachine(problem)
	if apierrors.IsNotFound(vmErr) {
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
		return r.problemState(ctx, problem, "VMDeleting", "Waiting for author VM deletion before recreating it with the same disk", false, false, nil, diskRef)
	}
	if vm.Spec.Template == nil {
		return r.problemState(ctx, problem, "ResourceConflict", "Author VM has no instance template", false, false, nil, diskRef)
	}
	volumes := vm.Spec.Template.Spec.Volumes
	if len(volumes) != 1 || volumes[0].Name != "root" || volumes[0].PersistentVolumeClaim == nil || volumes[0].PersistentVolumeClaim.ClaimName != diskName {
		return r.problemState(ctx, problem, "ResourceConflict", "Author VM does not reference the managed root PVC", false, false, nil, diskRef)
	}
	ref := &labv1alpha1.ProblemVMReference{VMName: vm.Name, UID: vm.UID}
	domain := &vm.Spec.Template.Spec.Domain
	resize := domain.CPU == nil || domain.CPU.Cores != desired.Spec.Template.Spec.Domain.CPU.Cores ||
		domain.Memory == nil || domain.Memory.Guest == nil || domain.Memory.Guest.Cmp(problem.Spec.Machine.Memory) != 0
	if resize {
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
			result.RequeueAfter = 5 * time.Second
			return result, stateErr
		}
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
	if resize || vm.Spec.RunStrategy == nil || *vm.Spec.RunStrategy != virtv1.RunStrategyAlways || vm.Spec.Running != nil {
		running := virtv1.RunStrategyAlways
		vm.Spec.RunStrategy = &running
		vm.Spec.Running = nil
		if err := r.Update(ctx, vm); err != nil {
			return ctrl.Result{}, err
		}
		return r.problemState(ctx, problem, "VMStarting", "Starting the author VM with the existing root disk", false, false, ref, diskRef)
	}
	// KubeVirt records the VM generation observed when the VMI started. Do not reuse readiness from before a restart.
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

func (r *ProblemReconciler) haltLockedProblem(ctx context.Context, problem *labv1alpha1.Problem, vm *virtv1.VirtualMachine, vmErr error) (ctrl.Result, error) {
	if vmErr == nil {
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
		if condition := apiMeta.FindStatusCondition(problem.Status.Conditions, "Locked"); !stopped && condition != nil && condition.Status == metav1.ConditionTrue && time.Since(condition.LastTransitionTime.Time) >= 5*time.Minute {
			reason, message = "ShutdownTimeout", "Author VM did not stop within five minutes; keep the lock and do not force stop"
		}
		result, err := r.problemState(ctx, problem, reason, message, false, stopped, ref, nil)
		if !stopped && reason != "ShutdownTimeout" {
			result.RequeueAfter = 5 * time.Second
		}
		return result, err
	}
	return r.problemState(ctx, problem, "VMMissing", "Locked author VM is missing; do not recreate it during snapshot processing", false, false, nil, nil)
}

func ownedByProblem(object client.Object, problem *labv1alpha1.Problem) bool {
	owner := metav1.GetControllerOf(object)
	return owner != nil && owner.UID == problem.UID && owner.Kind == "Problem" && owner.APIVersion == labv1alpha1.GroupVersion.String()
}

func ownedByDataVolume(object client.Object, uid types.UID) bool {
	for _, owner := range object.GetOwnerReferences() {
		if owner.UID == uid && owner.Kind == "DataVolume" && owner.APIVersion == cdiv1.SchemeGroupVersion.String() {
			return true
		}
	}
	return false
}

func (r *ProblemReconciler) problemStopped(ctx context.Context, problem *labv1alpha1.Problem, vm *virtv1.VirtualMachine) (bool, error) {
	vmi := &virtv1.VirtualMachineInstance{}
	err := r.reader().Get(ctx, client.ObjectKeyFromObject(vm), vmi)
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := r.reader().List(ctx, pods, client.InNamespace(problem.Namespace), client.MatchingLabels{problemUIDLabel: string(problem.UID), virtv1.AppLabel: "virt-launcher"}); err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}

func (r *ProblemReconciler) problemState(ctx context.Context, observed *labv1alpha1.Problem, reason, message string, ready, stopped bool, vmRef *labv1alpha1.ProblemVMReference, diskRef *labv1alpha1.ProblemDiskReference) (ctrl.Result, error) {
	current := &labv1alpha1.Problem{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(observed), current); err != nil {
		return ctrl.Result{}, err
	}
	if current.UID != observed.UID || current.Generation != observed.Generation {
		return ctrl.Result{}, apierrors.NewConflict(labv1alpha1.GroupVersion.WithResource("problems").GroupResource(), current.Name, fmt.Errorf("problem changed during reconciliation"))
	}
	before := current.Status.DeepCopy()
	if vmRef != nil {
		current.Status.VMRef = vmRef
	}
	if diskRef != nil {
		current.Status.DiskRef = diskRef
	}
	locked := current.Status.ActiveChallenge != nil
	// A lock may have been acquired while child resources were being read. Never publish readiness based on the unlocked observation.
	if locked != (observed.Status.ActiveChallenge != nil) {
		return ctrl.Result{Requeue: true}, nil
	}
	state := func(value bool) metav1.ConditionStatus {
		if value {
			return metav1.ConditionTrue
		}
		return metav1.ConditionFalse
	}
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
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Status().Update(ctx, current)
}

func (r *ProblemReconciler) deleteProblem(ctx context.Context, problem *labv1alpha1.Problem) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(problem, problemCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	if problem.Status.ActiveChallenge != nil {
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
			if err := r.Delete(ctx, vm, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &vm.UID}); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
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
		// A crash may have happened after DataVolume creation and before its UID was saved.
		// Persist ownership evidence before deleting the DataVolume so its leftover PVC can still be verified.
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
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(problem), problem); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if problem.Status.ActiveChallenge != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	controllerutil.RemoveFinalizer(problem, problemCleanupFinalizer)
	return ctrl.Result{}, r.Update(ctx, problem)
}

func (r *ProblemReconciler) problemEvents(ctx context.Context, object client.Object) []reconcile.Request {
	uid := object.GetLabels()[problemUIDLabel]
	if uid == "" {
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

func (r *ProblemReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.StorageClass == "" {
		return fmt.Errorf("--storage-class is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&labv1alpha1.Problem{}).
		Owns(&virtv1.VirtualMachine{}).
		Owns(&cdiv1.DataVolume{}).
		Watches(&virtv1.VirtualMachineInstance{}, handler.EnqueueRequestsFromMapFunc(r.problemEvents)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.problemEvents)).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.problemEvents)).
		Named("problem").Complete(r)
}
