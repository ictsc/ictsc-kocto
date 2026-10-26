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
	"errors"
	"fmt"
	"strings"
	"sync"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newBundleFixture() *problemFixture {
	f := newProblemFixture()
	f.reconciler.BundleBuilderImage = "example.invalid/trusted-builder@sha256:fixed"
	f.problem.Spec.Source = labv1alpha1.ProblemSourceSpec{Repository: "https://github.com/ictsc/labs", CommitHash: strings.Repeat("a", 40), Directory: "labs", TopologyFile: "nested/custom.yml"}
	f.problem.Spec.Machine.Disk = resource.MustParse("14Gi")
	Expect(f.client.Update(ctx, f.problem)).To(Succeed())
	return f
}
func bundlePVC(f *problemFixture) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{}
	Expect(f.client.Get(ctx, types.NamespacedName{Name: problemBundleName(f.problem), Namespace: f.problem.Namespace}, pvc)).To(Succeed())
	return pvc
}
func bundleJob(f *problemFixture) *batchv1.Job {
	job := &batchv1.Job{}
	Expect(f.client.Get(ctx, types.NamespacedName{Name: problemBundleName(f.problem), Namespace: f.problem.Namespace}, job)).To(Succeed())
	return job
}
func startBundle(f *problemFixture) {
	f.reconcile() // fix initial source
	f.reconcile() // create and record PVC
	f.reconcile() // create and record Job
	f.reconcile() // observe pending
}
func finishBundle(f *problemFixture) {
	pvc := bundlePVC(f)
	pvc.Status.Phase = corev1.ClaimBound
	Expect(f.client.Status().Update(ctx, pvc)).To(Succeed())
	job := bundleJob(f)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	Expect(f.client.Status().Update(ctx, job)).To(Succeed())
	f.reconcile() // remember completion
	f.reconcile() // create root DataVolume
	f.finishImport()
	f.reconcile() // create VM
}

var _ = Describe("Offline Problem bundles", func() {
	It("waits for bundle completion, connects two exact volumes and stays idempotent", func() {
		f := newBundleFixture()
		startBundle(f)
		vmName, _ := problemResourceNames(f.problem)
		Expect(apierrors.IsNotFound(f.client.Get(ctx, types.NamespacedName{Name: vmName, Namespace: f.problem.Namespace}, &virtv1.VirtualMachine{}))).To(BeTrue())
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("BundleBuildPending"))
		job := bundleJob(f)
		Expect(job.Spec.Template.Spec.AutomountServiceAccountToken).To(Equal(ptr.To(false)))
		Expect(job.Spec.Template.Spec.SecurityContext.RunAsUser).To(Equal(ptr.To(int64(107))))
		Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--topology=nested/custom.yml"))
		Expect(job.Spec.BackoffLimit).To(Equal(ptr.To(int32(2))))
		finishBundle(f)
		Expect(apiMeta.IsStatusConditionTrue(f.problem.Status.Conditions, "BundleReady")).To(BeTrue())
		vm := f.vm()
		Expect(problemVolumesMatch(f.problem, vm)).To(BeTrue())
		Expect(vm.Spec.Template.Spec.Volumes).To(HaveLen(2))
		Expect(vm.Spec.Template.Spec.Domain.Devices.AutoattachPodInterface).To(Equal(ptr.To(false)))
		previous := f.problem.Status.BundleRef.DeepCopy()
		vmUID := vm.UID
		for range 3 {
			f.reconcile()
		}
		Expect(f.problem.Status.BundleRef).To(Equal(previous))
		Expect(f.vm().UID).To(Equal(vmUID))
		Expect(bundleJob(f).UID).To(Equal(job.UID))
	})
	It("records a bounded build failure without creating a VM or rebuilding", func() {
		f := newBundleFixture()
		startBundle(f)
		job := bundleJob(f)
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
		Expect(f.client.Status().Update(ctx, job)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("BundleBuildFailed"))
		f.reconcile()
		Expect(bundleJob(f).UID).To(Equal(job.UID))
		Expect(f.problem.Status.VMRef).To(BeNil())
	})
	It("does not rebuild a missing or replaced bundle PVC", func() {
		f := newBundleFixture()
		startBundle(f)
		pvc := bundlePVC(f)
		Expect(f.client.Delete(ctx, pvc)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("BundleMissing"))
		replacement := problemBundlePVC(f.problem)
		replacement.UID = "replacement"
		replacement.OwnerReferences = pvc.OwnerReferences
		Expect(f.client.Create(ctx, replacement)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
	})
	It("does not recreate a recorded Job or accept modified build arguments", func() {
		f := newBundleFixture()
		startBundle(f)
		job := bundleJob(f)
		job.Spec.Template.Spec.Containers[0].Args = []string{"--commit=other"}
		Expect(f.client.Update(ctx, job)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
		Expect(f.client.Delete(ctx, job)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("BundleJobMissing"))
	})
	It("rejects source changes while keeping the original bundle and author disk", func() {
		f := newBundleFixture()
		startBundle(f)
		uid := bundlePVC(f).UID
		f.problem.Spec.Source.CommitHash = strings.Repeat("b", 40)
		Expect(f.client.Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("BundleSourceChanged"))
		Expect(bundlePVC(f).UID).To(Equal(uid))
	})
	It("does not add a bundle to existing author disks when the feature is enabled", func() {
		f := newProblemFixture()
		f.reconcile()
		f.finishImport()
		f.reconcile()
		uid := f.vm().UID
		f.reconciler.BundleBuilderImage = "example.invalid/new-builder"
		f.reconcile()
		Expect(f.problem.Status.BundleRef).To(BeNil())
		Expect(f.vm().UID).To(Equal(uid))
		Expect(f.vm().Spec.Template.Spec.Volumes).To(HaveLen(1))
	})
	It("rejects unexpected volumes and writable bundle devices", func() {
		f := newBundleFixture()
		startBundle(f)
		vm := problemVirtualMachine(f.problem)
		vm.Spec.Template.Spec.Volumes = append(vm.Spec.Template.Spec.Volumes, virtv1.Volume{Name: "foreign"})
		Expect(problemVolumesMatch(f.problem, vm)).To(BeFalse())
		vm = problemVirtualMachine(f.problem)
		vm.Spec.Template.Spec.Domain.Devices.Disks[1].Disk.ReadOnly = false
		Expect(problemVolumesMatch(f.problem, vm)).To(BeFalse())
		vm = problemVirtualMachine(f.problem)
		vm.Spec.Template.Spec.Volumes[1].Secret = &virtv1.SecretVolumeSource{SecretName: "foreign"}
		Expect(problemVolumesMatch(f.problem, vm)).To(BeFalse())
	})
	It("waits for all writers even after the Job complete condition is set", func() {
		f := newBundleFixture()
		startBundle(f)
		job := bundleJob(f)
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		Expect(f.client.Status().Update(ctx, job)).To(Succeed())
		pvc := bundlePVC(f)
		pvc.Status.Phase = corev1.ClaimBound
		Expect(f.client.Status().Update(ctx, pvc)).To(Succeed())
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: f.problem.Namespace, Labels: map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID)}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		Expect(f.client.Create(ctx, pod)).To(Succeed())
		f.reconcile()
		Expect(f.problem.Status.BundleRef.Ready).To(BeFalse())
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("BundleWriterStopping"))
	})
	It("preserves bundle resources while locked and deletes the writer before its PVC", func() {
		f := newBundleFixture()
		startBundle(f)
		f.problem.Status.ActiveChallenge = &labv1alpha1.ActiveChallengeStatus{UID: "lock", ActiveChallengeName: "challenge"}
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(bundlePVC(f)).NotTo(BeNil())
		Expect(bundleJob(f)).NotTo(BeNil())
		f.problem.Status.ActiveChallenge = nil
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		done, err := f.reconciler.cleanupProblemBundle(ctx, f.problem)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(bundlePVC(f)).NotTo(BeNil())
		// Fake client has no foreground garbage collector; remove the finalizer to simulate it.
		job := &batchv1.Job{}
		key := client.ObjectKey{Name: problemBundleName(f.problem), Namespace: f.problem.Namespace}
		if err := f.client.Get(ctx, key, job); err == nil {
			job.Finalizers = nil
			Expect(f.client.Update(ctx, job)).To(Succeed())
		}
		done, err = f.reconciler.cleanupProblemBundle(ctx, f.problem)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		done, err = f.reconciler.cleanupProblemBundle(ctx, f.problem)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
	})
	It("rejects bundle-backed Challenge before locking or stopping the author VM", func() {
		f := newChallengeFixture()
		f.problem.Status.BundleRef = &labv1alpha1.ProblemBundleReference{SourceHash: "fixed", BuilderImage: "builder", StorageClassName: "test"}
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		vm := f.vm()
		strategy := *vm.Spec.RunStrategy
		f.reconcileChallenge()
		Expect(apiMeta.FindStatusCondition(f.challenge.Status.Conditions, "Failed").Reason).To(Equal("BundleSnapshotUnsupported"))
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.problem), f.problem)).To(Succeed())
		Expect(f.problem.Status.ActiveChallenge).To(BeNil())
		Expect(*f.vm().Spec.RunStrategy).To(Equal(strategy))
	})

	It("rejects invalid sources and insufficient Flatcar root capacity before allocating a bundle", func() {
		f := newBundleFixture()
		f.problem.Spec.Source.Directory = "../escape"
		Expect(f.client.Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("InvalidBundleSource"))
		Expect(f.problem.Status.BundleRef).To(BeNil())
		f.problem.Spec.Source.Directory = "labs"
		f.problem.Spec.Machine.Disk = resource.MustParse("1Gi")
		Expect(f.client.Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("BundleDiskTooSmall"))
		Expect(f.problem.Status.BundleRef).To(BeNil())
	})
	It("does not adopt a PVC owned by another Problem", func() {
		f := newBundleFixture()
		f.reconcile()
		foreign := problemBundlePVC(f.problem)
		foreign.OwnerReferences = []metav1.OwnerReference{{APIVersion: labv1alpha1.GroupVersion.String(), Kind: "Problem", Name: "other", UID: "other-uid", Controller: ptr.To(true)}}
		Expect(f.client.Create(ctx, foreign)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
		Expect(f.problem.Status.BundleRef.PVCUID).To(BeEmpty())
	})
	It("preserves a replacement bundle PVC during cleanup", func() {
		f := newBundleFixture()
		startBundle(f)
		pvc := bundlePVC(f)
		Expect(f.client.Delete(ctx, pvc)).To(Succeed())
		replacement := problemBundlePVC(f.problem)
		replacement.OwnerReferences = pvc.OwnerReferences
		Expect(f.client.Create(ctx, replacement)).To(Succeed())
		job := bundleJob(f)
		Expect(f.client.Delete(ctx, job)).To(Succeed())
		done, err := f.reconciler.cleanupProblemBundle(ctx, f.problem)
		Expect(done).To(BeFalse())
		Expect(err).To(MatchError(ContainSubstring("UID changed")))
		Expect(bundlePVC(f).UID).To(Equal(replacement.UID))
	})

	It("removes terminal writer Pods and observes their disappearance before readiness", func() {
		f := newBundleFixture()
		startBundle(f)
		pvc := bundlePVC(f)
		pvc.Status.Phase = corev1.ClaimBound
		Expect(f.client.Status().Update(ctx, pvc)).To(Succeed())
		job := bundleJob(f)
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		Expect(f.client.Status().Update(ctx, job)).To(Succeed())
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "terminal-writer", Namespace: f.problem.Namespace, Labels: map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID)}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
		Expect(f.client.Create(ctx, pod)).To(Succeed())
		f.reconcile()
		Expect(f.problem.Status.BundleRef.Ready).To(BeFalse())
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))).To(BeTrue())
		f.reconcile()
		Expect(f.problem.Status.BundleRef.Ready).To(BeTrue())
	})
	It("rejects a writer Pod with a spoofed Job label without deleting it", func() {
		f := newBundleFixture()
		startBundle(f)
		pvc := bundlePVC(f)
		pvc.Status.Phase = corev1.ClaimBound
		Expect(f.client.Status().Update(ctx, pvc)).To(Succeed())
		job := bundleJob(f)
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		Expect(f.client.Status().Update(ctx, job)).To(Succeed())
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign-writer", Namespace: f.problem.Namespace, Labels: map[string]string{"batch.kubernetes.io/controller-uid": string(job.UID)}}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
		Expect(f.client.Create(ctx, pod)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
	})
	It("keeps the initial builder and pull Secret after manager configuration changes", func() {
		f := newBundleFixture()
		f.reconciler.BundleBuilderPullSecret = "ghcr-pull"
		startBundle(f)
		job := bundleJob(f)
		Expect(job.Spec.Template.Spec.ImagePullSecrets).To(Equal([]corev1.LocalObjectReference{{Name: "ghcr-pull"}}))
		f.reconciler.BundleBuilderImage = "other-builder"
		f.reconciler.BundleBuilderPullSecret = "other-secret"
		f.reconcile()
		Expect(bundleJob(f).UID).To(Equal(job.UID))
		Expect(f.problem.Status.BundleRef.ImagePullSecret).To(Equal("ghcr-pull"))
	})
	It("keeps new Problems on the legacy path when the feature is disabled", func() {
		f := newBundleFixture()
		f.reconciler.BundleBuilderImage = ""
		f.reconcile()
		Expect(f.problem.Status.BundleRef).To(BeNil())
		Expect(f.disk()).NotTo(BeNil())
	})
	It("retries a PVC API error without recording a fictitious identity", func() {
		f := newBundleFixture()
		f.reconcile()
		original := f.client
		blocked := true
		wrapped := interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.PersistentVolumeClaim); ok && blocked {
				return errors.New("injected PVC create failure")
			}
			return c.Create(ctx, obj, opts...)
		}})
		f.reconciler.Client = wrapped
		f.reconciler.APIReader = wrapped
		_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.problem)})
		Expect(err).To(MatchError("injected PVC create failure"))
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.problem), f.problem)).To(Succeed())
		Expect(f.problem.Status.BundleRef.PVCUID).To(BeEmpty())
		blocked = false
		f.reconcile()
		Expect(f.problem.Status.BundleRef.PVCUID).NotTo(BeEmpty())
	})
	It("serializes concurrent initialization through API resourceVersion conflicts", func() {
		// Real envtest API provides atomic resourceVersion checking, unlike a mocked
		// in-memory observation. No Job controller or workloads run in envtest.
		f := newBundleFixture()
		p := f.problem.DeepCopy()
		p.UID = ""
		p.ResourceVersion = ""
		p.Name = ""
		p.GenerateName = "bundle-concurrency-"
		p.Namespace = "default"
		p.Spec.Access = []labv1alpha1.AccessEndpoint{{Name: "console", Node: "host1", Interface: "eth1"}}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, p)).To(Succeed()) })
		a, b := p.DeepCopy(), p.DeepCopy()
		reconciler := &ProblemReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), StorageClass: "test", BundleBuilderImage: "builder"}
		var group sync.WaitGroup
		errs := make(chan error, 2)
		for _, observed := range []*labv1alpha1.Problem{a, b} {
			group.Add(1)
			go func(p *labv1alpha1.Problem) {
				defer group.Done()
				_, _, err := reconciler.ensureProblemBundle(ctx, p, false)
				errs <- err
			}(observed)
		}
		group.Wait()
		close(errs)
		successful := 0
		for err := range errs {
			if err == nil {
				successful++
			} else {
				Expect(apierrors.IsConflict(err)).To(BeTrue(), fmt.Sprintf("unexpected race error: %v", err))
			}
		}
		Expect(successful).To(Equal(1))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		Expect(p.Status.BundleRef).NotTo(BeNil())
	})

	It("keeps a completed bundle while waiting for the root import", func() {
		f := newBundleFixture()
		startBundle(f)
		pvc := bundlePVC(f)
		pvc.Status.Phase = corev1.ClaimBound
		Expect(f.client.Status().Update(ctx, pvc)).To(Succeed())
		job := bundleJob(f)
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		Expect(f.client.Status().Update(ctx, job)).To(Succeed())
		f.reconcile()
		f.reconcile()
		f.reconcile()
		Expect(f.problem.Status.VMRef).To(BeNil())
		Expect(f.problem.Status.BundleRef.Ready).To(BeTrue())
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("DiskImportPending"))
		Expect(bundleJob(f).UID).To(Equal(job.UID))
		rootUID := f.disk().UID
		f.reconcile()
		Expect(f.disk().UID).To(Equal(rootUID))
	})
	It("reports a terminating Job without replacing it", func() {
		f := newBundleFixture()
		startBundle(f)
		job := bundleJob(f)
		job.Finalizers = []string{"example.invalid/hold"}
		Expect(f.client.Update(ctx, job)).To(Succeed())
		Expect(f.client.Delete(ctx, job)).To(Succeed())
		f.reconcile()
		Expect(apiMeta.FindStatusCondition(f.problem.Status.Conditions, "VMReady").Reason).To(Equal("ResourceConflict"))
		Expect(bundleJob(f).UID).To(Equal(job.UID))
	})

	It("uses the frozen bundle storage class for the later root import", func() {
		f := newBundleFixture()
		startBundle(f)
		storageClass := f.problem.Status.BundleRef.StorageClassName
		f.reconciler.StorageClass = "changed-default"
		finishBundle(f)
		Expect(f.disk().Spec.Storage.StorageClassName).To(Equal(ptr.To(storageClass)))
	})

})
