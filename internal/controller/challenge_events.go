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

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	csiv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"k8s.io/apimachinery/pkg/types"
	clonev1 "kubevirt.io/api/clone/v1beta1"
	virtv1 "kubevirt.io/api/core/v1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *ProblemChallengeReconciler) challengesForProblem(ctx context.Context, namespace, name string) []reconcile.Request {
	objects := &labv1alpha1.ProblemChallengeList{}
	if err := r.List(ctx, objects, client.InNamespace(namespace), client.MatchingFields{challengeProblemIndex: name}); err != nil {
		return nil
	}
	result := make([]reconcile.Request, 0, len(objects.Items))
	for _, c := range objects.Items {
		result = append(result, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&c)})
	}
	return result
}

// 直接の子でないVMI/Pod/Restore/CSI snapshotも、所有参照をたどってChallengeへ通知する。
func (r *ProblemChallengeReconciler) childChanged(ctx context.Context, object client.Object) []reconcile.Request {
	return r.childRequests(ctx, object, 0)
}

func (r *ProblemChallengeReconciler) childRequests(ctx context.Context, object client.Object, depth int) []reconcile.Request {
	if depth > 6 {
		return nil
	}
	namespace := object.GetNamespace()
	uid := object.GetLabels()[challengeUIDLabel]
	for _, owner := range object.GetOwnerReferences() {
		if owner.Kind == "ProblemChallenge" {
			uid = string(owner.UID)
		}
	}
	if uid != "" {
		objects := &labv1alpha1.ProblemChallengeList{}
		if err := r.List(ctx, objects, client.InNamespace(namespace)); err != nil {
			return nil
		}
		for _, c := range objects.Items {
			if string(c.UID) == uid {
				return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&c)}}
			}
		}
	}
	if content, ok := object.(*snapshotv1.VirtualMachineSnapshotContent); ok && content.Spec.VirtualMachineSnapshotName != nil {
		parent := &snapshotv1.VirtualMachineSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Name: *content.Spec.VirtualMachineSnapshotName, Namespace: namespace}, parent); err == nil && content.Name == challengeSnapshotContentName(parent.UID) {
			return r.childRequests(ctx, parent, depth+1)
		}
	}
	if volume, ok := object.(*csiv1.VolumeSnapshotContent); ok {
		namespace = volume.Spec.VolumeSnapshotRef.Namespace
		parent := &csiv1.VolumeSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Name: volume.Spec.VolumeSnapshotRef.Name, Namespace: namespace}, parent); err == nil && parent.UID == volume.Spec.VolumeSnapshotRef.UID {
			return r.childRequests(ctx, parent, depth+1)
		}
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.Kind == "Problem" {
			return r.challengesForProblem(ctx, namespace, owner.Name)
		}
		var parent client.Object
		switch owner.Kind {
		case "VirtualMachine":
			parent = &virtv1.VirtualMachine{}
		case "VirtualMachineInstance":
			parent = &virtv1.VirtualMachineInstance{}
		case "VirtualMachineClone":
			parent = &clonev1.VirtualMachineClone{}
		case "VirtualMachineSnapshot":
			parent = &snapshotv1.VirtualMachineSnapshot{}
		case "VirtualMachineSnapshotContent":
			parent = &snapshotv1.VirtualMachineSnapshotContent{}
		}
		if parent != nil {
			if err := r.Get(ctx, types.NamespacedName{Name: owner.Name, Namespace: namespace}, parent); err == nil && parent.GetUID() == owner.UID {
				return r.childRequests(ctx, parent, depth+1)
			}
		}
	}
	// 親が削除済みの場合も、statusに保存したUIDから削除待ちのChallengeへ通知できる。
	objects := &labv1alpha1.ProblemChallengeList{}
	if err := r.List(ctx, objects, client.InNamespace(namespace)); err != nil {
		return nil
	}
	for _, c := range objects.Items {
		refs := []*labv1alpha1.TeamResourceReference{}
		if s := c.Status.SnapshotRef; s != nil {
			if s.UID == object.GetUID() {
				return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&c)}}
			}
			refs = append(refs, s.ContentRef, s.VolumeSnapshotRef, s.VolumeSnapshotContentRef)
		}
		for _, team := range c.Status.Teams {
			refs = append(refs, team.CloneRef, team.VMRef, team.PVCRef)
		}
		for _, ref := range refs {
			if ref != nil && ref.UID == object.GetUID() {
				return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&c)}}
			}
		}
	}
	return nil
}

// 競技ノードのReady変化も通知する。VM側のstatus更新を待たず、障害を競技側の条件へ反映する。
func (r *ProblemChallengeReconciler) nodeChanged(ctx context.Context, object client.Object) []reconcile.Request {
	result := map[types.NamespacedName]bool{}
	objects := &labv1alpha1.ProblemChallengeList{}
	if err := r.List(ctx, objects); err != nil {
		return nil
	}
	for _, c := range objects.Items {
		matches := true
		for key, value := range c.Spec.Placement.NodeSelector {
			if object.GetLabels()[key] != value {
				matches = false
			}
		}
		if matches {
			result[client.ObjectKeyFromObject(&c)] = true
		}
	}
	// ノードのラベルが外れた場合も、すでにそのノードで実行しているVMIから通知先を探す。
	instances := &virtv1.VirtualMachineInstanceList{}
	if err := r.List(ctx, instances); err == nil {
		for index := range instances.Items {
			vmi := &instances.Items[index]
			if vmi.Status.NodeName == object.GetName() {
				for _, request := range r.childChanged(ctx, vmi) {
					result[request.NamespacedName] = true
				}
			}
		}
	}
	requests := make([]reconcile.Request, 0, len(result))
	for key := range result {
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}
