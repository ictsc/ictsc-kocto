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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
)

var _ = Describe("API transition validation", func() {
	newProblem := func() *labv1alpha1.Problem {
		return &labv1alpha1.Problem{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-problem-", Namespace: "default"},
			Spec: labv1alpha1.ProblemSpec{
				Source: labv1alpha1.ProblemSourceSpec{
					Repository: "https://example.invalid/problems.git", CommitHash: "initial", TopologyFile: "topology.yaml",
				},
				Machine: labv1alpha1.ProblemMachineSpec{
					CPU: 1, Memory: ptr.To(resource.MustParse("128Mi")), Disk: resource.MustParse("1Gi"),
					Image: labv1alpha1.ProblemMachineImageSpec{ContainerDisk: "example.invalid/disk:initial"},
				},
				Access: []labv1alpha1.AccessEndpoint{{Name: "console", Node: "router", Interface: "eth0"}},
			},
		}
	}

	createProblem := func() *labv1alpha1.Problem {
		problem := newProblem()
		Expect(k8sClient.Create(ctx, problem)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, problem)).To(Succeed()) })
		return problem
	}

	lockProblem := func(problem *labv1alpha1.Problem) {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
		problem.Status.ActiveChallenge = &labv1alpha1.ActiveChallengeStatus{
			ActiveChallengeName: "challenge", UID: types.UID("challenge-uid"), ProblemGeneration: problem.Generation,
		}
		Expect(k8sClient.Status().Update(ctx, problem)).To(Succeed())
	}

	DescribeTable("validates explicit storage on creation",
		func(storage map[string]any, accepted bool) {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newProblem())
			Expect(err).NotTo(HaveOccurred())
			p := &unstructured.Unstructured{Object: object}
			p.SetGroupVersionKind(labv1alpha1.GroupVersion.WithKind("Problem"))
			Expect(unstructured.SetNestedMap(p.Object, storage, "spec", "storage")).To(Succeed())
			err = k8sClient.Create(ctx, p)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { Expect(k8sClient.Delete(ctx, p)).To(Succeed()) })
			} else {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "creation error: %v", err)
			}
		},
		Entry("Filesystem", map[string]any{"storageClassName": "test.filesystem", "volumeMode": "Filesystem"}, true),
		Entry("Block", map[string]any{"storageClassName": "test-block", "volumeMode": "Block"}, true),
		Entry("missing class", map[string]any{"volumeMode": "Block"}, false),
		Entry("empty class", map[string]any{"storageClassName": "", "volumeMode": "Block"}, false),
		Entry("invalid class", map[string]any{"storageClassName": "Invalid/Class", "volumeMode": "Block"}, false),
		Entry("missing mode", map[string]any{"storageClassName": "test"}, false),
		Entry("unknown mode", map[string]any{"storageClassName": "test", "volumeMode": "Raw"}, false),
	)

	DescribeTable("keeps storage immutable across Problem updates",
		func(explicit bool, change func(*labv1alpha1.ProblemSpec)) {
			p := newProblem()
			if explicit {
				p.Spec.Storage = &labv1alpha1.ProblemStorageSpec{StorageClassName: "test", VolumeMode: corev1.PersistentVolumeBlock}
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, p)).To(Succeed()) })
			// storageを省略した既存Problemも含め、他の編集可能なフィールドは変更できる。
			p.Spec.Machine.CPU = 2
			Expect(k8sClient.Update(ctx, p)).To(Succeed())
			change(&p.Spec)
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, p))).To(BeTrue())
		},
		Entry("class", true, func(s *labv1alpha1.ProblemSpec) { s.Storage.StorageClassName = "other" }),
		Entry("mode", true, func(s *labv1alpha1.ProblemSpec) { s.Storage.VolumeMode = corev1.PersistentVolumeFilesystem }),
		Entry("removal", true, func(s *labv1alpha1.ProblemSpec) { s.Storage = nil }),
		Entry("legacy addition", false, func(s *labv1alpha1.ProblemSpec) {
			s.Storage = &labv1alpha1.ProblemStorageSpec{StorageClassName: "test", VolumeMode: corev1.PersistentVolumeBlock}
		}),
	)

	DescribeTable("validates memory and disk quantities on creation",
		func(field string, value any, accepted bool) {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newProblem())
			Expect(err).NotTo(HaveOccurred())
			problem := &unstructured.Unstructured{Object: object}
			problem.SetGroupVersionKind(labv1alpha1.GroupVersion.WithKind("Problem"))
			Expect(unstructured.SetNestedField(problem.Object, value, "spec", "machine", field)).To(Succeed())
			err = k8sClient.Create(ctx, problem)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { Expect(k8sClient.Delete(ctx, problem)).To(Succeed()) })
			} else {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "creation error: %v", err)
				Expect(err.Error()).To(ContainSubstring("spec.machine." + field))
			}
		},
		Entry("zero memory", "memory", "0", false),
		Entry("zero disk with a unit", "disk", "0Gi", false),
		Entry("negative memory", "memory", "-128Mi", false),
		Entry("negative disk", "disk", "-1Gi", false),
		Entry("integer zero memory", "memory", int64(0), false),
		Entry("integer negative disk", "disk", int64(-1), false),
		Entry("malformed memory", "memory", "invalid", false),
		Entry("malformed disk", "disk", "1.2.3Gi", false),
		Entry("positive binary memory", "memory", "128Mi", true),
		Entry("positive binary disk", "disk", "1Gi", true),
		Entry("positive decimal memory", "memory", "128M", true),
		Entry("positive decimal disk", "disk", "1G", true),
		Entry("positive fractional memory", "memory", "1m", true),
		Entry("positive fractional disk", "disk", "1e-3", true),
		Entry("positive integer memory", "memory", int64(128), true),
		Entry("positive integer disk", "disk", int64(1), true),
	)

	DescribeTable("validates fixed memory and balloon ranges",
		func(memory any, balloon map[string]any, accepted bool) {
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newProblem())
			Expect(err).NotTo(HaveOccurred())
			p := &unstructured.Unstructured{Object: object}
			p.SetGroupVersionKind(labv1alpha1.GroupVersion.WithKind("Problem"))
			unstructured.RemoveNestedField(p.Object, "spec", "machine", "memory")
			if memory != nil {
				Expect(unstructured.SetNestedField(p.Object, memory, "spec", "machine", "memory")).To(Succeed())
			}
			if balloon != nil {
				Expect(unstructured.SetNestedMap(p.Object, balloon, "spec", "machine", "memoryBalloon")).To(Succeed())
			}
			err = k8sClient.Create(ctx, p)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { Expect(k8sClient.Delete(ctx, p)).To(Succeed()) })
			} else {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "creation error: %v", err)
			}
		},
		Entry("legacy fixed", "4Gi", nil, true),
		Entry("disabled with fixed memory", "4Gi", map[string]any{"enabled": false}, true),
		Entry("enabled range", nil, map[string]any{"enabled": true, "min": "1Gi", "max": "4Gi"}, true),
		Entry("equal quantities with different units", nil, map[string]any{"enabled": true, "min": "1024Mi", "max": "1Gi"}, true),
		Entry("integer quantities", nil, map[string]any{"enabled": true, "min": int64(1024), "max": int64(4096)}, true),
		Entry("missing fixed memory", nil, nil, false),
		Entry("missing disabled memory", nil, map[string]any{"enabled": false}, false),
		Entry("missing enabled flag", "4Gi", map[string]any{}, false),
		Entry("missing min", nil, map[string]any{"enabled": true, "max": "4Gi"}, false),
		Entry("missing max", nil, map[string]any{"enabled": true, "min": "1Gi"}, false),
		Entry("both fixed memory and range", "4Gi", map[string]any{"enabled": true, "min": "1Gi", "max": "4Gi"}, false),
		Entry("disabled with a range", "4Gi", map[string]any{"enabled": false, "min": "1Gi", "max": "4Gi"}, false),
		Entry("inverted range", nil, map[string]any{"enabled": true, "min": "4Gi", "max": "1Gi"}, false),
		Entry("zero min", nil, map[string]any{"enabled": true, "min": "0", "max": "4Gi"}, false),
		Entry("negative max", nil, map[string]any{"enabled": true, "min": "1Gi", "max": "-4Gi"}, false),
		Entry("zero max", nil, map[string]any{"enabled": true, "min": "1Gi", "max": int64(0)}, false),
		Entry("malformed min", nil, map[string]any{"enabled": true, "min": "invalid", "max": "4Gi"}, false),
		Entry("malformed max", nil, map[string]any{"enabled": true, "min": "1Gi", "max": "invalid"}, false),
	)

	It("allows switching memory policies only while unlocked", func() {
		p := createProblem()
		p.Spec.Machine.Memory = nil
		p.Spec.Machine.MemoryBalloon = &labv1alpha1.ProblemMemoryBalloonSpec{
			Enabled: true, Min: ptr.To(resource.MustParse("1Gi")), Max: ptr.To(resource.MustParse("4Gi")),
		}
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		// 型付きClientでも、省略した固定量のmemoryが送信データに含まれないことを確認する。
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		Expect(p.Spec.Machine.Memory).To(BeNil())
		lockProblem(p)
		p.Spec.Machine.MemoryBalloon.Min = ptr.To(resource.MustParse("2Gi"))
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, p))).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		p.Status.ActiveChallenge = nil
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		p.Spec.Machine.MemoryBalloon.Min = ptr.To(resource.MustParse("2Gi"))
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		p.Spec.Machine.MemoryBalloon = &labv1alpha1.ProblemMemoryBalloonSpec{Enabled: false}
		p.Spec.Machine.Memory = ptr.To(resource.MustParse("4Gi"))
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		p.Spec.Machine.MemoryBalloon = nil
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
	})

	DescribeTable("rejects nonpositive quantities on an unlocked Problem update",
		func(field, value string) {
			problem := createProblem()
			if field == "memory" {
				problem.Spec.Machine.Memory = ptr.To(resource.MustParse(value))
			} else {
				problem.Spec.Machine.Disk = resource.MustParse(value)
			}
			err := k8sClient.Update(ctx, problem)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "update error: %v", err)
			Expect(err.Error()).To(ContainSubstring("machine." + field + "は有効なQuantityで0より大きい値にしてください"))
		},
		Entry("zero memory", "memory", "0"),
		Entry("negative memory", "memory", "-1Gi"),
		Entry("zero disk", "disk", "0"),
		Entry("negative disk", "disk", "-1Gi"),
	)

	DescribeTable("rejects empty Problem status reference names",
		func(change func(*labv1alpha1.ProblemStatus), field string) {
			problem := createProblem()
			change(&problem.Status)
			err := k8sClient.Status().Update(ctx, problem)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "status update error: %v", err)
			Expect(err.Error()).To(ContainSubstring(field))
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
			Expect(problem.Status.VMRef).To(BeNil())
			Expect(problem.Status.DiskRef).To(BeNil())
			Expect(problem.Status.ActiveChallenge).To(BeNil())
		},
		Entry("VM name", func(status *labv1alpha1.ProblemStatus) {
			status.VMRef = &labv1alpha1.ProblemVMReference{UID: types.UID("vm-uid")}
		}, "status.vmRef.vmName"),
		Entry("DataVolume name", func(status *labv1alpha1.ProblemStatus) {
			status.DiskRef = &labv1alpha1.ProblemDiskReference{UID: types.UID("disk-uid")}
		}, "status.diskRef.diskName"),
		Entry("lock owner name", func(status *labv1alpha1.ProblemStatus) {
			status.ActiveChallenge = &labv1alpha1.ActiveChallengeStatus{UID: types.UID("challenge-uid"), ProblemGeneration: 1}
		}, "status.activeChallenge.activeChallengeName"),
	)

	It("accepts complete VM and DataVolume references in status", func() {
		problem := createProblem()
		problem.Status.VMRef = &labv1alpha1.ProblemVMReference{VMName: "author-vm", UID: types.UID("vm-uid")}
		problem.Status.DiskRef = &labv1alpha1.ProblemDiskReference{DiskName: "author-disk", UID: types.UID("disk-uid")}
		Expect(k8sClient.Status().Update(ctx, problem)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
		Expect(problem.Status.VMRef.VMName).To(Equal("author-vm"))
		Expect(problem.Status.DiskRef.DiskName).To(Equal("author-disk"))
	})

	DescribeTable("rejects every Problem spec change while locked",
		func(change func(*labv1alpha1.ProblemSpec)) {
			problem := createProblem()
			lockProblem(problem)
			change(&problem.Spec)
			err := k8sClient.Update(ctx, problem)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "update error: %v", err)
			Expect(err.Error()).To(ContainSubstring("ProblemのspecはChallengeによるロック中は変更できません"))
		},
		Entry("CPU", func(spec *labv1alpha1.ProblemSpec) { spec.Machine.CPU = 2 }),
		Entry("memory", func(spec *labv1alpha1.ProblemSpec) { spec.Machine.Memory = ptr.To(resource.MustParse("256Mi")) }),
		Entry("source", func(spec *labv1alpha1.ProblemSpec) { spec.Source.CommitHash = "updated" }),
		Entry("access", func(spec *labv1alpha1.ProblemSpec) { spec.Access[0].Node = "another-router" }),
	)

	DescribeTable("allows editable Problem fields after releasing the lock",
		func(change func(*labv1alpha1.ProblemSpec)) {
			problem := createProblem()
			lockProblem(problem)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
			base := problem.DeepCopy()
			problem.Status.ActiveChallenge = nil
			Expect(k8sClient.Status().Patch(ctx, problem,
				client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
			Expect(problem.Status.ActiveChallenge).To(BeNil())
			change(&problem.Spec)
			Expect(k8sClient.Update(ctx, problem)).To(Succeed())
		},
		Entry("CPU", func(spec *labv1alpha1.ProblemSpec) { spec.Machine.CPU = 2 }),
		Entry("memory", func(spec *labv1alpha1.ProblemSpec) { spec.Machine.Memory = ptr.To(resource.MustParse("256Mi")) }),
		Entry("source", func(spec *labv1alpha1.ProblemSpec) { spec.Source.CommitHash = "updated" }),
		Entry("access", func(spec *labv1alpha1.ProblemSpec) { spec.Access[0].Node = "another-router" }),
	)

	DescribeTable("keeps initial disk and image immutable even after releasing the lock",
		func(change func(*labv1alpha1.ProblemMachineSpec), message string) {
			problem := createProblem()
			change(&problem.Spec.Machine)
			err := k8sClient.Update(ctx, problem)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "update error: %v", err)
			Expect(err.Error()).To(ContainSubstring(message))

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
			lockProblem(problem)
			problem.Status.ActiveChallenge = nil
			Expect(k8sClient.Status().Update(ctx, problem)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
			change(&problem.Spec.Machine)
			err = k8sClient.Update(ctx, problem)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "update error: %v", err)
			Expect(err.Error()).To(ContainSubstring(message))
		},
		Entry("disk", func(spec *labv1alpha1.ProblemMachineSpec) { spec.Disk = resource.MustParse("2Gi") }, "machine.diskは変更できません"),
		Entry("image", func(spec *labv1alpha1.ProblemMachineSpec) { spec.Image.ContainerDisk = "example.invalid/disk:updated" }, "machine.imageは変更できません"),
	)

	It("allows metadata and condition updates while keeping the lock", func() {
		problem := createProblem()
		lockProblem(problem)
		lockGeneration := problem.Generation
		problem.Labels = map[string]string{"review": "approved"}
		problem.Finalizers = []string{"lab.icttoracon.net/test-cleanup"}
		Expect(k8sClient.Update(ctx, problem)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
			problem.Finalizers = nil
			Expect(k8sClient.Update(ctx, problem)).To(Succeed())
		})

		reconciler := &ProblemReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(problem)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
		Expect(problem.Status.Conditions).NotTo(BeEmpty())
		Expect(problem.Status.ActiveChallenge.UID).To(Equal(types.UID("challenge-uid")))
		Expect(problem.Status.ActiveChallenge.ProblemGeneration).To(Equal(lockGeneration))
		Expect(problem.Generation).To(Equal(lockGeneration))
	})

	It("does not allow a regular update to clear the lock and change spec together", func() {
		problem := createProblem()
		lockProblem(problem)
		problem.Status.ActiveChallenge = nil
		problem.Spec.Machine.CPU = 2
		err := k8sClient.Update(ctx, problem)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "update error: %v", err)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
		Expect(problem.Status.ActiveChallenge).NotTo(BeNil())
		Expect(problem.Spec.Machine.CPU).To(Equal(int32(1)))
	})

	It("rejects stale lock and status writers using resourceVersion", func() {
		problem := createProblem()
		competingLock := problem.DeepCopy()
		staleConditions := problem.DeepCopy()
		lockProblem(problem)
		competingLock.Status.ActiveChallenge = &labv1alpha1.ActiveChallengeStatus{
			ActiveChallengeName: "other", UID: types.UID("other-uid"), ProblemGeneration: competingLock.Generation,
		}
		Expect(apierrors.IsConflict(k8sClient.Status().Update(ctx, competingLock))).To(BeTrue())
		staleConditions.Status.Conditions = []metav1.Condition{{
			Type: "VMReady", Status: metav1.ConditionFalse, Reason: "Pending", Message: "VM pending",
			ObservedGeneration: staleConditions.Generation, LastTransitionTime: metav1.Now(),
		}}
		Expect(apierrors.IsConflict(k8sClient.Status().Update(ctx, staleConditions))).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
		Expect(problem.Status.ActiveChallenge.UID).To(Equal(types.UID("challenge-uid")))
	})

	It("rejects lock acquisition based on a stale Problem generation", func() {
		problem := createProblem()
		staleLock := problem.DeepCopy()
		problem.Spec.Machine.CPU = 2
		Expect(k8sClient.Update(ctx, problem)).To(Succeed())
		staleLock.Status.ActiveChallenge = &labv1alpha1.ActiveChallengeStatus{
			ActiveChallengeName: "challenge", UID: types.UID("challenge-uid"), ProblemGeneration: staleLock.Generation,
		}
		Expect(apierrors.IsConflict(k8sClient.Status().Update(ctx, staleLock))).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
		Expect(problem.Status.ActiveChallenge).To(BeNil())
	})

	DescribeTable("keeps Challenge spec immutable while allowing status updates",
		func(change func(*labv1alpha1.ProblemChallengeSpec)) {
			challenge := &labv1alpha1.ProblemChallenge{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-challenge-", Namespace: "default"},
				Spec: labv1alpha1.ProblemChallengeSpec{
					ProblemRef: labv1alpha1.ProblemReference{Name: "problem"},
					Placement:  labv1alpha1.ProblemChallengePlacementSpec{NodeSelector: map[string]string{"node": "worker"}},
					Teams:      []labv1alpha1.ProblemChallengeTeamSpec{{ID: "team-a"}},
				},
			}
			Expect(k8sClient.Create(ctx, challenge)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, challenge)).To(Succeed()) })
			challenge.Status.SourceProblem = &labv1alpha1.SourceProblemReference{
				Name: "problem", UID: types.UID("problem-uid"), Generation: 1,
			}
			Expect(k8sClient.Status().Update(ctx, challenge)).To(Succeed())
			change(&challenge.Spec)
			err := k8sClient.Update(ctx, challenge)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "update error: %v", err)
			Expect(err.Error()).To(ContainSubstring("ProblemChallengeのspecは変更できません"))
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(challenge), challenge)).To(Succeed())
			Expect(challenge.Status.SourceProblem.UID).To(Equal(types.UID("problem-uid")))
		},
		Entry("Problem reference", func(spec *labv1alpha1.ProblemChallengeSpec) { spec.ProblemRef.Name = "other-problem" }),
		Entry("placement", func(spec *labv1alpha1.ProblemChallengeSpec) { spec.Placement.NodeSelector["node"] = "other-worker" }),
		Entry("teams", func(spec *labv1alpha1.ProblemChallengeSpec) {
			spec.Teams = append(spec.Teams, labv1alpha1.ProblemChallengeTeamSpec{ID: "team-b"})
		}),
	)
})
