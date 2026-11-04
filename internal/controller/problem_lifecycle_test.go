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

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type problemFixture struct {
	client     client.Client
	reconciler *ProblemReconciler
	problem    *labv1alpha1.Problem
}

func newProblemFixture() *problemFixture {
	scheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(batchv1.AddToScheme(scheme)).To(Succeed())
	Expect(virtv1.AddToScheme(scheme)).To(Succeed())
	Expect(cdiv1.AddToScheme(scheme)).To(Succeed())
	Expect(labv1alpha1.AddToScheme(scheme)).To(Succeed())
	problem := &labv1alpha1.Problem{
		ObjectMeta: metav1.ObjectMeta{Name: "author", Namespace: "default", UID: types.UID("problem-uid"), Generation: 1},
		Spec: labv1alpha1.ProblemSpec{Machine: labv1alpha1.ProblemMachineSpec{
			CPU: 1, Memory: ptr.To(resource.MustParse("128Mi")), Disk: resource.MustParse("1Gi"),
			Image: labv1alpha1.ProblemMachineImageSpec{ContainerDisk: "example.invalid/disk@sha256:test"},
		}},
	}
	created := 0
	// managed-fieldsの追跡処理がKubeVirt VMIのuint64フィールドを扱えないため、通常のUpdateで検証する。
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjectTracker(k8stesting.NewObjectTracker(scheme, serializer.NewCodecFactory(scheme).UniversalDecoder())).WithObjects(problem).
		WithStatusSubresource(&batchv1.Job{}, &labv1alpha1.Problem{}, &cdiv1.DataVolume{}, &virtv1.VirtualMachine{}, &corev1.PersistentVolumeClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, object client.Object, options ...client.CreateOption) error {
			created++
			object.SetUID(types.UID(fmt.Sprintf("created-%d", created)))
			object.SetGeneration(1)
			return cl.Create(ctx, object, options...)
		}}).Build()
	return &problemFixture{client: cl, problem: problem, reconciler: &ProblemReconciler{Client: cl, APIReader: cl, Scheme: scheme, StorageClass: "test-immediate"}}
}

func (f *problemFixture) reconcile() {
	_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.problem)})
	Expect(err).NotTo(HaveOccurred())
	Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.problem), f.problem)).To(Succeed())
}

func (f *problemFixture) disk() *cdiv1.DataVolume {
	_, name := problemResourceNames(f.problem)
	disk := &cdiv1.DataVolume{}
	Expect(f.client.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, disk)).To(Succeed())
	return disk
}

func (f *problemFixture) vm() *virtv1.VirtualMachine {
	name, _ := problemResourceNames(f.problem)
	vm := &virtv1.VirtualMachine{}
	Expect(f.client.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, vm)).To(Succeed())
	return vm
}

func (f *problemFixture) finishImport() *corev1.PersistentVolumeClaim {
	dv := f.disk()
	dv.Status.Phase = cdiv1.Succeeded
	dv.Status.Conditions = []cdiv1.DataVolumeCondition{{Type: cdiv1.DataVolumeReady, Status: corev1.ConditionTrue}}
	Expect(f.client.Status().Update(ctx, dv)).To(Succeed())
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dv.Name, Namespace: dv.Namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: cdiv1.SchemeGroupVersion.String(), Kind: "DataVolume", Name: dv.Name, UID: dv.UID}}},
		Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: dv.Spec.Storage.StorageClassName, VolumeMode: dv.Spec.Storage.VolumeMode, AccessModes: dv.Spec.Storage.AccessModes},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	Expect(f.client.Create(ctx, pvc)).To(Succeed())
	return pvc
}

func (f *problemFixture) boot() *corev1.PersistentVolumeClaim {
	f.reconcile()
	pvc := f.finishImport()
	f.reconcile()
	vm := f.vm()
	vm.Status.Ready = true
	vm.Status.ObservedGeneration = vm.Generation
	vm.Status.Conditions = []virtv1.VirtualMachineCondition{{Type: virtv1.VirtualMachineReady, Status: corev1.ConditionTrue}}
	Expect(f.client.Status().Update(ctx, vm)).To(Succeed())
	f.reconcile()
	return pvc
}

func (f *problemFixture) instanceAndPod() (*virtv1.VirtualMachineInstance, *corev1.Pod) {
	vm := f.vm()
	vmi := &virtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Name: vm.Name, Namespace: vm.Namespace, Labels: problemLabels(f.problem)}}
	Expect(f.client.Create(ctx, vmi)).To(Succeed())
	labels := problemLabels(f.problem)
	labels[virtv1.AppLabel] = "virt-launcher"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "launcher", Namespace: "default", Labels: labels}}
	Expect(f.client.Create(ctx, pod)).To(Succeed())
	return vmi, pod
}

var _ = Describe("Author VM lifecycle", func() {
	It("rejects a replacement root PVC with the DataVolume's original owner UID", func() {
		f := newProblemFixture()
		pvc := f.boot()
		originalUID := pvc.UID
		Expect(originalUID).NotTo(BeEmpty())
		Expect(f.client.Delete(ctx, pvc)).To(Succeed())

		replacement := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: pvc.Name, Namespace: pvc.Namespace, OwnerReferences: pvc.OwnerReferences},
			Spec:       *pvc.Spec.DeepCopy(),
			Status:     *pvc.Status.DeepCopy(),
		}
		Expect(f.client.Create(ctx, replacement)).To(Succeed())
		Expect(replacement.UID).NotTo(Equal(originalUID))

		f.reconcile()

		Expect(f.problem.Status.DiskRef.PVCUID).To(Equal(originalUID))
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(replacement), replacement)).To(Succeed())
		Expect(replacement.UID).NotTo(Equal(originalUID))
	})

	It("records the existing root PVC UID when upgrading an older Problem status", func() {
		f := newProblemFixture()
		pvc := f.boot()
		f.problem.Status.DiskRef.PVCUID = ""
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())

		f.reconcile()

		Expect(f.problem.Status.DiskRef.PVCUID).To(Equal(pvc.UID))
	})

	DescribeTable("creates balloon policies without changing the disk",
		func(config *labv1alpha1.ProblemMemoryBalloonSpec, guest, request string, enabled *bool) {
			f := newProblemFixture()
			f.problem.Spec.Machine.Memory = ptr.To(resource.MustParse("4Gi"))
			f.problem.Spec.Machine.MemoryBalloon = config
			if config != nil && config.Enabled {
				f.problem.Spec.Machine.Memory = nil
			}
			Expect(f.client.Update(ctx, f.problem)).To(Succeed())
			pvc := f.boot()
			domain := f.vm().Spec.Template.Spec.Domain
			Expect(domain.Memory.Guest.Cmp(resource.MustParse(guest))).To(BeZero())
			requested := domain.Resources.Requests[corev1.ResourceMemory]
			Expect(requested.Cmp(resource.MustParse(request))).To(BeZero())
			Expect(domain.Devices.AutoattachMemBalloon).To(Equal(enabled))
			Expect(f.vm().Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvc.Name))
			version := f.vm().ResourceVersion
			f.reconcile()
			Expect(f.vm().ResourceVersion).To(Equal(version))
		},
		Entry("legacy default", nil, "4Gi", "4Gi", (*bool)(nil)),
		Entry("disabled", &labv1alpha1.ProblemMemoryBalloonSpec{Enabled: false}, "4Gi", "4Gi", ptr.To(false)),
		Entry("enabled", &labv1alpha1.ProblemMemoryBalloonSpec{
			Enabled: true, Min: ptr.To(resource.MustParse("1Gi")), Max: ptr.To(resource.MustParse("4Gi")),
		}, "4Gi", "1Gi", ptr.To(true)),
	)

	DescribeTable("stops the VMI and Pod before changing memory policy on the same PVC",
		func(change func(*labv1alpha1.ProblemMachineSpec), guest, request string, enabled bool) {
			f := newProblemFixture()
			f.problem.Spec.Machine.Memory = nil
			f.problem.Spec.Machine.MemoryBalloon = &labv1alpha1.ProblemMemoryBalloonSpec{
				Enabled: true, Min: ptr.To(resource.MustParse("1Gi")), Max: ptr.To(resource.MustParse("4Gi")),
			}
			Expect(f.client.Update(ctx, f.problem)).To(Succeed())
			pvc := f.boot()
			vm := f.vm()
			vm.Spec.Template.Spec.Domain.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
			Expect(f.client.Update(ctx, vm)).To(Succeed())
			vmi, pod := f.instanceAndPod()
			change(&f.problem.Spec.Machine)
			Expect(f.client.Update(ctx, f.problem)).To(Succeed())
			f.reconcile()
			Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyHalted))
			requested := f.vm().Spec.Template.Spec.Domain.Resources.Requests[corev1.ResourceMemory]
			Expect(requested.Cmp(resource.MustParse("1Gi"))).To(BeZero())
			Expect(f.client.Delete(ctx, vmi)).To(Succeed())
			f.reconcile()
			Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyHalted))
			Expect(f.client.Delete(ctx, pod)).To(Succeed())
			f.reconcile()
			vm = f.vm()
			Expect(*vm.Spec.RunStrategy).To(Equal(virtv1.RunStrategyAlways))
			Expect(vm.Spec.Template.Spec.Domain.Memory.Guest.Cmp(resource.MustParse(guest))).To(BeZero())
			requested = vm.Spec.Template.Spec.Domain.Resources.Requests[corev1.ResourceMemory]
			Expect(requested.Cmp(resource.MustParse(request))).To(BeZero())
			cpu := vm.Spec.Template.Spec.Domain.Resources.Requests[corev1.ResourceCPU]
			Expect(cpu.Cmp(resource.MustParse("100m"))).To(BeZero())
			device := vm.Spec.Template.Spec.Domain.Devices.AutoattachMemBalloon
			Expect(device == nil || *device).To(Equal(enabled))
			storedPVC := &corev1.PersistentVolumeClaim{}
			Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), storedPVC)).To(Succeed())
			Expect(storedPVC.UID).To(Equal(pvc.UID))
			Expect(vm.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvc.Name))
			version := vm.ResourceVersion
			f.reconcile()
			Expect(f.vm().ResourceVersion).To(Equal(version))
		},
		Entry("request only", func(s *labv1alpha1.ProblemMachineSpec) {
			s.MemoryBalloon.Min = ptr.To(resource.MustParse("2Gi"))
		}, "4Gi", "2Gi", true),
		Entry("guest only", func(s *labv1alpha1.ProblemMachineSpec) {
			s.MemoryBalloon.Max = ptr.To(resource.MustParse("8Gi"))
		}, "8Gi", "1Gi", true),
		Entry("disable balloon", func(s *labv1alpha1.ProblemMachineSpec) {
			s.MemoryBalloon = &labv1alpha1.ProblemMemoryBalloonSpec{Enabled: false}
			s.Memory = ptr.To(resource.MustParse("4Gi"))
		}, "4Gi", "4Gi", false),
		Entry("return to legacy", func(s *labv1alpha1.ProblemMachineSpec) {
			s.MemoryBalloon = nil
			s.Memory = ptr.To(resource.MustParse("4Gi"))
		}, "4Gi", "4Gi", true),
	)

	It("stops before disabling only the balloon device", func() {
		f := newProblemFixture()
		pvc := f.boot()
		vmi, pod := f.instanceAndPod()
		f.problem.Spec.Machine.MemoryBalloon = &labv1alpha1.ProblemMemoryBalloonSpec{Enabled: false}
		Expect(f.client.Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyHalted))
		Expect(f.vm().Spec.Template.Spec.Domain.Devices.AutoattachMemBalloon).To(BeNil())
		Expect(f.client.Delete(ctx, vmi)).To(Succeed())
		f.reconcile()
		Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyHalted))
		Expect(f.client.Delete(ctx, pod)).To(Succeed())
		f.reconcile()
		vm := f.vm()
		Expect(vm.Spec.Template.Spec.Domain.Devices.AutoattachMemBalloon).To(HaveValue(BeFalse()))
		Expect(*vm.Spec.RunStrategy).To(Equal(virtv1.RunStrategyAlways))
		Expect(vm.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvc.Name))
	})

	DescribeTable("provisions explicit storage without a manager default and preserves it on recovery",
		func(mode corev1.PersistentVolumeMode) {
			f := newProblemFixture()
			f.problem.Spec.Storage = &labv1alpha1.ProblemStorageSpec{StorageClassName: "per-problem", VolumeMode: mode}
			Expect(f.client.Update(ctx, f.problem)).To(Succeed())
			f.reconciler.StorageClass = ""
			pvc := f.boot()
			dv := f.disk()
			Expect(*dv.Spec.Storage.StorageClassName).To(Equal("per-problem"))
			Expect(*dv.Spec.Storage.VolumeMode).To(Equal(mode))
			Expect(*pvc.Spec.VolumeMode).To(Equal(mode))
			f.reconciler.StorageClass = "different-default"
			Expect(f.client.Delete(ctx, f.vm())).To(Succeed())
			f.reconcile()
			Expect(f.disk().UID).To(Equal(dv.UID))
			stored := &corev1.PersistentVolumeClaim{}
			Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), stored)).To(Succeed())
			Expect(stored.UID).To(Equal(pvc.UID))
			Expect(*stored.Spec.StorageClassName).To(Equal("per-problem"))
			Expect(*stored.Spec.VolumeMode).To(Equal(mode))
		},
		Entry("Filesystem", corev1.PersistentVolumeFilesystem),
		Entry("Block", corev1.PersistentVolumeBlock),
	)

	It("resumes legacy disks even when the manager default changes or disappears", func() {
		f := newProblemFixture()
		pvc := f.boot()
		for _, fallback := range []string{"other", ""} {
			f.reconciler.StorageClass = fallback
			Expect(f.client.Delete(ctx, f.vm())).To(Succeed())
			f.reconcile()
			Expect(f.vm().Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvc.Name))
			stored := &corev1.PersistentVolumeClaim{}
			Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), stored)).To(Succeed())
			Expect(stored.UID).To(Equal(pvc.UID))
			Expect(*stored.Spec.StorageClassName).To(Equal("test-immediate"))
			Expect(pvcVolumeMode(stored.Spec)).To(Equal(corev1.PersistentVolumeFilesystem))
		}
	})

	It("refuses a mismatched author disk without converting or recreating it", func() {
		f := newProblemFixture()
		f.problem.Spec.Storage = &labv1alpha1.ProblemStorageSpec{StorageClassName: "per-problem", VolumeMode: corev1.PersistentVolumeBlock}
		Expect(f.client.Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		pvc := f.finishImport()
		mode := corev1.PersistentVolumeFilesystem
		pvc.Spec.VolumeMode = &mode
		Expect(f.client.Update(ctx, pvc)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
		Expect(f.disk().UID).To(Equal(f.problem.Status.DiskRef.UID))
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		Expect(*pvc.Spec.VolumeMode).To(Equal(corev1.PersistentVolumeFilesystem))
	})
	It("creates author resources through the pinned API schemas and resumes after a controller restart", func() {
		problem := &labv1alpha1.Problem{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "author-api-", Namespace: "default"},
			Spec: labv1alpha1.ProblemSpec{
				Source:  labv1alpha1.ProblemSourceSpec{Repository: "https://example.invalid/problems.git", CommitHash: "test", TopologyFile: "topology.yaml"},
				Machine: labv1alpha1.ProblemMachineSpec{CPU: 1, Memory: ptr.To(resource.MustParse("128Mi")), Disk: resource.MustParse("1Gi"), Image: labv1alpha1.ProblemMachineImageSpec{ContainerDisk: "example.invalid/disk:test"}},
				Access:  []labv1alpha1.AccessEndpoint{{Name: "console", Node: "router", Interface: "eth0"}},
			},
		}
		Expect(k8sClient.Create(ctx, problem)).To(Succeed())
		vmName, diskName := problemResourceNames(problem)
		DeferCleanup(func() {
			for _, object := range []client.Object{
				&virtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: "default"}},
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: diskName, Namespace: "default"}},
				&cdiv1.DataVolume{ObjectMeta: metav1.ObjectMeta{Name: diskName, Namespace: "default"}}, problem,
			} {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(object), object)
				if apierrors.IsNotFound(err) {
					continue
				}
				Expect(err).NotTo(HaveOccurred())
				object.SetFinalizers(nil)
				Expect(k8sClient.Update(ctx, object)).To(Succeed())
				Expect(k8sClient.Delete(ctx, object)).To(Succeed())
			}
		})
		reconciler := &ProblemReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), StorageClass: "test-immediate"}
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(problem)}
		_, err := reconciler.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		dv := &cdiv1.DataVolume{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: diskName, Namespace: "default"}, dv)).To(Succeed())
		dvUID := dv.UID
		Expect(ownedByProblem(dv, problem)).To(BeTrue())
		dv.Status.Phase = cdiv1.Succeeded
		dv.Status.Conditions = []cdiv1.DataVolumeCondition{{Type: cdiv1.DataVolumeReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, dv)).To(Succeed())
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: diskName, Namespace: "default", OwnerReferences: []metav1.OwnerReference{{APIVersion: cdiv1.SchemeGroupVersion.String(), Kind: "DataVolume", Name: diskName, UID: dvUID}}},
			Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: dv.Spec.Storage.AccessModes, StorageClassName: dv.Spec.Storage.StorageClassName, VolumeMode: dv.Spec.Storage.VolumeMode, Resources: dv.Spec.Storage.Resources},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		pvc.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		vm := &virtv1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: vmName, Namespace: "default"}, vm)).To(Succeed())
		Expect(vm.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvc.Name))
		vm.Status.Ready = true
		vm.Status.ObservedGeneration = vm.Generation
		vm.Status.Conditions = []virtv1.VirtualMachineCondition{{Type: virtv1.VirtualMachineReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
		restarted := &ProblemReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), StorageClass: "test-immediate"}
		_, err = restarted.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(problem), problem)).To(Succeed())
		Expect(problem.Status.DiskRef.UID).To(Equal(dvUID))
		Expect(problem.Status.VMRef.UID).To(Equal(vm.UID))
		Expect(apiMeta.FindStatusCondition(problem.Status.Conditions, "VMReady").Status).To(Equal(metav1.ConditionTrue))
	})

	It("builds a persistent disk and VM without guest NICs or cloud-init", func() {
		f := newProblemFixture()
		dv := problemDataVolume(f.problem, f.reconciler.StorageClass)
		vm := problemVirtualMachine(f.problem)
		vmName, diskName := problemResourceNames(f.problem)
		Expect(len(vmName)).To(BeNumerically("<=", 63))
		Expect(len(diskName)).To(BeNumerically("<=", 63))
		Expect(dv.Name).To(Equal(diskName))
		Expect(*dv.Spec.Source.Registry.URL).To(Equal("docker://" + f.problem.Spec.Machine.Image.ContainerDisk))
		Expect(*dv.Spec.Storage.StorageClassName).To(Equal("test-immediate"))
		Expect(*dv.Spec.Storage.VolumeMode).To(Equal(corev1.PersistentVolumeFilesystem))
		Expect(dv.Spec.Storage.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}))
		size := dv.Spec.Storage.Resources.Requests[corev1.ResourceStorage]
		Expect(size.Cmp(f.problem.Spec.Machine.Disk)).To(BeZero())
		Expect(*vm.Spec.RunStrategy).To(Equal(virtv1.RunStrategyAlways))
		Expect(vm.Spec.Template.Spec.Domain.CPU.Cores).To(Equal(uint32(1)))
		Expect(vm.Spec.Template.Spec.Domain.Memory.Guest.Cmp(*f.problem.Spec.Machine.Memory)).To(BeZero())
		Expect(*vm.Spec.Template.Spec.Domain.Devices.AutoattachPodInterface).To(BeFalse())
		Expect(vm.Spec.Template.Spec.Networks).To(BeEmpty())
		Expect(vm.Spec.Template.Spec.Domain.Devices.Interfaces).To(BeEmpty())
		Expect(vm.Spec.Template.Spec.Volumes).To(HaveLen(1))
		Expect(vm.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(diskName))
		Expect(vm.Spec.Template.Spec.Domain.Devices.Disks[0].Disk.Bus).To(Equal(virtv1.DiskBusVirtio))
		Expect(vm.Spec.DataVolumeTemplates).To(BeEmpty())
		copy := f.problem.DeepCopy()
		copy.UID = "other-problem"
		otherVM, otherDisk := problemResourceNames(copy)
		Expect(otherVM).NotTo(Equal(vmName))
		Expect(otherDisk).NotTo(Equal(diskName))
	})

	It("imports once, waits for CDI and PVC, and preserves the disk when a VM is recreated", func() {
		f := newProblemFixture()
		f.reconcile()
		name, _ := problemResourceNames(f.problem)
		Expect(apierrors.IsNotFound(f.client.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &virtv1.VirtualMachine{}))).To(BeTrue())
		diskUID := f.disk().UID
		f.reconcile()
		Expect(f.disk().UID).To(Equal(diskUID))
		pvc := f.finishImport()
		f.reconcile()
		vm := f.vm()
		Expect(ownedByProblem(vm, f.problem)).To(BeTrue())
		oldUID := vm.UID
		Expect(f.client.Delete(ctx, vm)).To(Succeed())
		f.reconcile()
		Expect(f.vm().UID).NotTo(Equal(oldUID))
		Expect(f.disk().UID).To(Equal(diskUID))
		storedPVC := &corev1.PersistentVolumeClaim{}
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), storedPVC)).To(Succeed())
		Expect(storedPVC.UID).To(Equal(pvc.UID))
	})

	It("does not recreate an imported disk that disappears", func() {
		f := newProblemFixture()
		f.boot()
		dv := f.disk()
		Expect(f.client.Delete(ctx, dv)).To(Succeed())
		f.reconcile()
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(dv), &cdiv1.DataVolume{}))).To(BeTrue())
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("DiskMissing"))
	})

	It("keeps reconciliation idempotent and preserves platform CPU defaults", func() {
		f := newProblemFixture()
		f.boot()
		vm := f.vm()
		vm.Spec.Template.Spec.Domain.CPU.Model = "host-model"
		vm.Spec.Template.Spec.Domain.CPU.Sockets = 1
		vm.Spec.Template.Spec.Domain.CPU.Threads = 1
		Expect(f.client.Update(ctx, vm)).To(Succeed())
		version := vm.ResourceVersion
		statusVersion := f.problem.ResourceVersion
		f.reconcile()
		f.reconcile()
		Expect(f.vm().ResourceVersion).To(Equal(version))
		Expect(f.problem.ResourceVersion).To(Equal(statusVersion))
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Status).To(Equal(metav1.ConditionTrue))
	})

	It("does not report readiness from an earlier VM generation", func() {
		f := newProblemFixture()
		f.boot()
		vm := f.vm()
		vm.Generation++
		Expect(f.client.Update(ctx, vm)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Status).To(Equal(metav1.ConditionFalse))
		vm = f.vm()
		vm.Status.ObservedGeneration = vm.Generation
		Expect(f.client.Status().Update(ctx, vm)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Status).To(Equal(metav1.ConditionTrue))
	})

	It("waits for both VMI and launcher Pod before resizing on the same disk", func() {
		f := newProblemFixture()
		pvc := f.boot()
		vmi, pod := f.instanceAndPod()
		f.problem.Spec.Machine.CPU = 2
		f.problem.Spec.Machine.Memory = ptr.To(resource.MustParse("256Mi"))
		Expect(f.client.Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyHalted))
		Expect(f.vm().Spec.Template.Spec.Domain.CPU.Cores).To(Equal(uint32(1)))
		Expect(f.client.Delete(ctx, vmi)).To(Succeed())
		f.reconcile()
		Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyHalted))
		Expect(f.client.Delete(ctx, pod)).To(Succeed())
		f.reconcile()
		vm := f.vm()
		Expect(*vm.Spec.RunStrategy).To(Equal(virtv1.RunStrategyAlways))
		Expect(vm.Spec.Template.Spec.Domain.CPU.Cores).To(Equal(uint32(2)))
		Expect(vm.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvc.Name))
		storedPVC := &corev1.PersistentVolumeClaim{}
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), storedPVC)).To(Succeed())
		Expect(storedPVC.UID).To(Equal(pvc.UID))
	})

	It("holds a lock through stopping and restarts with the original disk after release", func() {
		f := newProblemFixture()
		pvc := f.boot()
		vmi, pod := f.instanceAndPod()
		f.problem.Status.ActiveChallenge = &labv1alpha1.ActiveChallengeStatus{ActiveChallengeName: "challenge", UID: "challenge-uid", ProblemGeneration: 1}
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyHalted))
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "Quiesced").Status).To(Equal(metav1.ConditionFalse))
		Expect(f.client.Delete(ctx, vmi)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "Quiesced").Status).To(Equal(metav1.ConditionFalse))
		Expect(f.client.Delete(ctx, pod)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "Quiesced").Status).To(Equal(metav1.ConditionTrue))
		f.problem.Status.ActiveChallenge = nil
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyAlways))
		Expect(f.vm().Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvc.Name))
	})

	It("does not overwrite a DataVolume or root PVC owned by another resource", func() {
		for _, occupied := range []string{"datavolume", "pvc"} {
			f := newProblemFixture()
			_, name := problemResourceNames(f.problem)
			var object client.Object = &cdiv1.DataVolume{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
			if occupied == "pvc" {
				object = &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
			}
			Expect(f.client.Create(ctx, object)).To(Succeed())
			version := object.GetResourceVersion()
			f.reconcile()
			Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
			Expect(f.client.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
			Expect(object.GetResourceVersion()).To(Equal(version))
		}
	})

	It("reports a stop timeout without releasing the lock or deleting the VMI", func() {
		f := newProblemFixture()
		f.boot()
		vmi, _ := f.instanceAndPod()
		f.problem.Status.ActiveChallenge = &labv1alpha1.ActiveChallengeStatus{ActiveChallengeName: "challenge", UID: "challenge-uid", ProblemGeneration: 1}
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		condition := apiMeta.FindStatusCondition(f.problem.Status.Conditions, "Locked")
		condition.LastTransitionTime = metav1.NewTime(time.Now().Add(-6 * time.Minute))
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "Quiesced").Reason).To(Equal("ShutdownTimeout"))
		Expect(f.problem.Status.ActiveChallenge.UID).To(Equal(types.UID("challenge-uid")))
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(vmi), &virtv1.VirtualMachineInstance{})).To(Succeed())
	})

	It("reports failed imports and does not replace their disks", func() {
		f := newProblemFixture()
		f.reconcile()
		dv := f.disk()
		originalUID := dv.UID
		dv.Status.Phase = cdiv1.Failed
		Expect(f.client.Status().Update(ctx, dv)).To(Succeed())
		f.reconcile()
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("DiskImportFailed"))
		Expect(f.disk().UID).To(Equal(originalUID))
	})

	It("cleans up VM, DataVolume, and proven PVC before removing the Problem finalizer", func() {
		f := newProblemFixture()
		pvc := f.boot()
		vm := f.vm()
		dv := f.disk()
		Expect(f.client.Delete(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(vm), &virtv1.VirtualMachine{}))).To(BeTrue())
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(dv), &cdiv1.DataVolume{})).To(Succeed())
		f.reconcile()
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(dv), &cdiv1.DataVolume{}))).To(BeTrue())
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{})).To(Succeed())
		f.reconcile()
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}))).To(BeTrue())
		_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.problem)})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(f.problem), &labv1alpha1.Problem{}))).To(BeTrue())
	})

	It("recovers disk identity before cleanup when a crash prevented saving its reference", func() {
		f := newProblemFixture()
		pvc := f.boot()
		dv := f.disk()
		f.problem.Status.DiskRef = nil
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		Expect(f.client.Delete(ctx, f.problem)).To(Succeed())
		f.reconcile()
		f.reconcile()
		Expect(f.problem.Status.DiskRef.UID).To(Equal(dv.UID))
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(dv), &cdiv1.DataVolume{})).To(Succeed())
		f.reconcile()
		f.reconcile()
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}))).To(BeTrue())
		_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.problem)})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(f.problem), &labv1alpha1.Problem{}))).To(BeTrue())
	})

	It("does not delete a foreign PVC left at the fixed root name during cleanup", func() {
		f := newProblemFixture()
		pvc := f.boot()
		Expect(f.client.Delete(ctx, pvc)).To(Succeed())
		foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvc.Name, Namespace: pvc.Namespace}}
		Expect(f.client.Create(ctx, foreign)).To(Succeed())
		version := foreign.ResourceVersion
		Expect(f.client.Delete(ctx, f.problem)).To(Succeed())
		f.reconcile()
		f.reconcile()
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
		Expect(controllerutil.ContainsFinalizer(f.problem, problemCleanupFinalizer)).To(BeTrue())
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(foreign), foreign)).To(Succeed())
		Expect(foreign.ResourceVersion).To(Equal(version))
	})

	It("does not delete a replacement root PVC during cleanup even when its DataVolume owner UID matches", func() {
		f := newProblemFixture()
		pvc := f.boot()
		dv := f.disk()
		Expect(f.client.Delete(ctx, pvc)).To(Succeed())
		replacement := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: pvc.Name, Namespace: pvc.Namespace, OwnerReferences: pvc.OwnerReferences},
			Spec:       *pvc.Spec.DeepCopy(),
			Status:     *pvc.Status.DeepCopy(),
		}
		Expect(f.client.Create(ctx, replacement)).To(Succeed())
		Expect(replacement.UID).NotTo(Equal(pvc.UID))
		version := replacement.ResourceVersion

		Expect(f.client.Delete(ctx, f.problem)).To(Succeed())
		f.reconcile()
		f.reconcile()

		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
		Expect(controllerutil.ContainsFinalizer(f.problem, problemCleanupFinalizer)).To(BeTrue())
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(dv), &cdiv1.DataVolume{})).To(Succeed())
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(replacement), replacement)).To(Succeed())
		Expect(replacement.ResourceVersion).To(Equal(version))
	})

	It("retains author resources on Problem deletion while locked", func() {
		f := newProblemFixture()
		pvc := f.boot()
		f.problem.Status.ActiveChallenge = &labv1alpha1.ActiveChallengeStatus{ActiveChallengeName: "challenge", UID: "challenge-uid", ProblemGeneration: 1}
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		Expect(f.client.Delete(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(controllerutil.ContainsFinalizer(f.problem, problemCleanupFinalizer)).To(BeTrue())
		Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyHalted))
		Expect(f.disk()).NotTo(BeNil())
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{})).To(Succeed())
	})
})
