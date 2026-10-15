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
	"encoding/json"
	"fmt"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	csiv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	clonev1 "kubevirt.io/api/clone/v1beta1"
	virtv1 "kubevirt.io/api/core/v1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"k8s.io/utils/ptr"
)

type challengeFixture struct {
	*problemFixture
	challenge  *labv1alpha1.ProblemChallenge
	reconciler *ProblemChallengeReconciler
	starts     map[string]int
}

type deletingAfterProblemLockReader struct {
	client.Reader
	problemKey types.NamespacedName
}

func (r deletingAfterProblemLockReader) Get(ctx context.Context, key types.NamespacedName, object client.Object, options ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	problem, ok := object.(*labv1alpha1.Problem)
	if ok && key == r.problemKey && problem.Status.ActiveChallenge != nil {
		deleting := metav1.Now()
		problem.DeletionTimestamp = &deleting
	}
	return nil
}

func newChallengeFixture() *challengeFixture {
	author := newProblemFixture()
	author.boot()
	scheme := author.client.Scheme()
	Expect(clonev1.AddToScheme(scheme)).To(Succeed())
	Expect(snapshotv1.AddToScheme(scheme)).To(Succeed())
	Expect(csiv1.AddToScheme(scheme)).To(Succeed())
	c := &labv1alpha1.ProblemChallenge{ObjectMeta: metav1.ObjectMeta{Name: "round", Namespace: "default", UID: "challenge-uid", Generation: 1}, Spec: labv1alpha1.ProblemChallengeSpec{ProblemRef: labv1alpha1.ProblemReference{Name: author.problem.Name}, Placement: labv1alpha1.ProblemChallengePlacementSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": "worker"}}, Teams: []labv1alpha1.ProblemChallengeTeamSpec{{ID: "a"}, {ID: "b"}}}}
	counter := 100
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjectTracker(k8stesting.NewObjectTracker(scheme, serializer.NewCodecFactory(scheme).UniversalDecoder())).
		WithObjects(author.problem, author.vm(), author.disk(), func() *corev1.PersistentVolumeClaim {
			p := &corev1.PersistentVolumeClaim{}
			Expect(author.client.Get(ctx, client.ObjectKeyFromObject(author.disk()), p)).To(Succeed())
			return p
		}(), c, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"kubernetes.io/hostname": "worker"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}).
		WithStatusSubresource(&labv1alpha1.Problem{}, &labv1alpha1.ProblemChallenge{}, &virtv1.VirtualMachine{}, &virtv1.VirtualMachineInstance{}, &cdiv1.DataVolume{}, &corev1.PersistentVolumeClaim{}, &corev1.Node{}, &clonev1.VirtualMachineClone{}, &snapshotv1.VirtualMachineSnapshot{}, &snapshotv1.VirtualMachineSnapshotContent{}, &snapshotv1.VirtualMachineRestore{}, &csiv1.VolumeSnapshot{}, &csiv1.VolumeSnapshotContent{}).
		WithIndex(&labv1alpha1.ProblemChallenge{}, challengeProblemIndex, func(object client.Object) []string {
			return []string{object.(*labv1alpha1.ProblemChallenge).Spec.ProblemRef.Name}
		}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, object client.Object, options ...client.CreateOption) error {
			counter++
			object.SetUID(types.UID(fmt.Sprintf("resource-%d", counter)))
			object.SetGeneration(1)
			return cl.Create(ctx, object, options...)
		}}).Build()
	author.client = cl
	author.reconciler.Client = cl
	author.reconciler.APIReader = cl
	f := &challengeFixture{problemFixture: author, challenge: c, starts: map[string]int{}}
	f.reconciler = &ProblemChallengeReconciler{Client: cl, APIReader: cl, Scheme: scheme, StartVM: f.start}
	return f
}

func (f *challengeFixture) reconcileChallenge() {
	_, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.challenge)})
	Expect(err).NotTo(HaveOccurred())
	err = f.client.Get(ctx, client.ObjectKeyFromObject(f.challenge), f.challenge)
	Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue())
}

func (f *challengeFixture) start(ctx context.Context, vm *virtv1.VirtualMachine) error {
	f.starts[vm.Name]++
	vmi := &virtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Name: vm.Name, Namespace: vm.Namespace, Labels: vm.Spec.Template.ObjectMeta.Labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(vm, virtv1.GroupVersion.WithKind("VirtualMachine"))}}, Spec: vm.Spec.Template.Spec, Status: virtv1.VirtualMachineInstanceStatus{Phase: virtv1.Running, NodeName: "worker"}}
	if err := f.client.Create(ctx, vmi); err != nil {
		return err
	}
	vm.Status.Ready = true
	vm.Status.ObservedGeneration = vm.Generation
	return f.client.Status().Update(ctx, vm)
}

func (f *challengeFixture) snapshot() *snapshotv1.VirtualMachineSnapshot {
	object := &snapshotv1.VirtualMachineSnapshot{}
	Expect(f.client.Get(ctx, types.NamespacedName{Name: challengeSnapshotName(f.challenge), Namespace: f.challenge.Namespace}, object)).To(Succeed())
	return object
}

func (f *challengeFixture) quiesce() {
	f.reconcileChallenge()
	f.reconcile()
	f.reconcileChallenge()
}

func (f *challengeFixture) finishSnapshot() {
	s := f.snapshot()
	vm := f.vm()
	sourcePVC := &corev1.PersistentVolumeClaim{}
	Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.disk()), sourcePVC)).To(Succeed())
	content := &snapshotv1.VirtualMachineSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: challengeSnapshotContentName(s.UID), Namespace: "default"}, Spec: snapshotv1.VirtualMachineSnapshotContentSpec{VirtualMachineSnapshotName: &s.Name, Source: snapshotv1.SourceSpec{VirtualMachine: &snapshotv1.VirtualMachine{ObjectMeta: vm.ObjectMeta, Spec: vm.Spec}}, VolumeBackups: []snapshotv1.VolumeBackup{{VolumeName: "root", PersistentVolumeClaim: snapshotv1.PersistentVolumeClaim{ObjectMeta: sourcePVC.ObjectMeta, Spec: sourcePVC.Spec}, VolumeSnapshotName: ptr.To("saved-volume")}}}, Status: &snapshotv1.VirtualMachineSnapshotContentStatus{ReadyToUse: ptr.To(true)}}
	Expect(f.client.Create(ctx, content)).To(Succeed())
	volume := &csiv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "saved-volume", Namespace: "default", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(content, snapshotv1.SchemeGroupVersion.WithKind("VirtualMachineSnapshotContent"))}}, Spec: csiv1.VolumeSnapshotSpec{Source: csiv1.VolumeSnapshotSource{PersistentVolumeClaimName: &sourcePVC.Name}}, Status: &csiv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true), BoundVolumeSnapshotContentName: ptr.To("saved-storage-content")}}
	Expect(f.client.Create(ctx, volume)).To(Succeed())
	storage := &csiv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "saved-storage-content"}, Spec: csiv1.VolumeSnapshotContentSpec{VolumeSnapshotRef: corev1.ObjectReference{Name: volume.Name, Namespace: volume.Namespace, UID: volume.UID}, DeletionPolicy: csiv1.VolumeSnapshotContentDelete, Driver: "test.csi", Source: csiv1.VolumeSnapshotContentSource{VolumeHandle: ptr.To("source-handle")}}, Status: &csiv1.VolumeSnapshotContentStatus{ReadyToUse: ptr.To(true)}}
	Expect(f.client.Create(ctx, storage)).To(Succeed())
	s.Status = &snapshotv1.VirtualMachineSnapshotStatus{SourceUID: &vm.UID, VirtualMachineSnapshotContentName: &content.Name, Phase: snapshotv1.Succeeded, ReadyToUse: ptr.To(true), SnapshotVolumes: &snapshotv1.SnapshotVolumesLists{IncludedVolumes: []string{"root"}}}
	Expect(f.client.Status().Update(ctx, s)).To(Succeed())
	f.reconcileChallenge()
}

// KubeVirtがSnapshotを復元する部分だけを模擬し、controllerが生成したJSON patchesも実際に適用する。
func (f *challengeFixture) finishClone(id string) *virtv1.VirtualMachine {
	cloneName, vmName, pvcName := challengeTeamNames(f.challenge, id)
	clone := &clonev1.VirtualMachineClone{}
	Expect(f.client.Get(ctx, types.NamespacedName{Name: cloneName, Namespace: "default"}, clone)).To(Succeed())
	source := f.vm().DeepCopy()
	source.Name = vmName
	source.UID = ""
	source.ResourceVersion = ""
	source.Generation = 0
	source.Finalizers = nil
	source.Status = virtv1.VirtualMachineStatus{}
	source.OwnerReferences = nil
	source.Labels = nil
	source.Spec.Template.ObjectMeta.Labels = nil
	source.Annotations = map[string]string{"restore.kubevirt.io/lastRestoreUID": "tmp-restore-" + string(clone.UID) + "-restored-uid"}
	data, err := json.Marshal(source)
	Expect(err).NotTo(HaveOccurred())
	patchData := []byte("[")
	for index, patch := range clone.Spec.Patches {
		if index != 0 {
			patchData = append(patchData, ',')
		}
		patchData = append(patchData, []byte(patch)...)
	}
	patchData = append(patchData, ']')
	patch, err := jsonpatch.DecodePatch(patchData)
	Expect(err).NotTo(HaveOccurred())
	data, err = patch.Apply(data)
	Expect(err).NotTo(HaveOccurred())
	vm := &virtv1.VirtualMachine{}
	Expect(json.Unmarshal(data, vm)).To(Succeed())
	vm.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = pvcName
	Expect(f.client.Create(ctx, vm)).To(Succeed())
	sourcePVC := &corev1.PersistentVolumeClaim{}
	Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.disk()), sourcePVC)).To(Succeed())
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: "default"}, Spec: *sourcePVC.Spec.DeepCopy(), Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{APIGroup: ptr.To("snapshot.storage.k8s.io"), Kind: "VolumeSnapshot", Name: "saved-volume"}
	pvc.Spec.VolumeName = "team-pv-" + id
	Expect(f.client.Create(ctx, pvc)).To(Succeed())
	clone.Status.Phase = clonev1.Succeeded
	clone.Status.TargetName = &vmName
	Expect(f.client.Status().Update(ctx, clone)).To(Succeed())
	return vm
}

func (f *challengeFixture) bootTeams() {
	f.quiesce()
	f.finishSnapshot()
	f.finishClone("a")
	f.finishClone("b")
	f.reconcileChallenge()
	f.reconcileChallenge()
}

var _ = Describe("Challenge lifecycle", func() {
	It("keeps the acquired lock when Problem deletion starts before Accepted status is saved", func() {
		f := newChallengeFixture()
		f.reconciler.APIReader = deletingAfterProblemLockReader{
			Reader:     f.client,
			problemKey: client.ObjectKeyFromObject(f.problem),
		}

		f.reconcileChallenge()

		Expect(apiMeta.IsStatusConditionTrue(f.challenge.Status.Conditions, "Accepted")).To(BeTrue())
		Expect(apiMeta.IsStatusConditionTrue(f.challenge.Status.Conditions, "Failed")).To(BeFalse())
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.problem), f.problem)).To(Succeed())
		Expect(f.problem.Status.ActiveChallenge).NotTo(BeNil())
		Expect(f.problem.Status.ActiveChallenge.UID).To(Equal(f.challenge.UID))
	})

	DescribeTable("restores and verifies the saved PVC storage rather than the manager default",
		func(mode corev1.PersistentVolumeMode) {
			f := newChallengeFixture()
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.disk()), pvc)).To(Succeed())
			pvc.Spec.StorageClassName = ptr.To("saved-class")
			pvc.Spec.VolumeMode = &mode
			Expect(f.client.Update(ctx, pvc)).To(Succeed())
			f.problemFixture.reconciler.StorageClass = "unrelated-current-default"
			f.bootTeams()
			Expect(apiMeta.IsStatusConditionTrue(f.challenge.Status.Conditions, "Ready")).To(BeTrue())
			for _, team := range f.challenge.Status.Teams {
				Expect(f.client.Get(ctx, types.NamespacedName{Name: team.PVCRef.Name, Namespace: "default"}, pvc)).To(Succeed())
				Expect(*pvc.Spec.StorageClassName).To(Equal("saved-class"))
				Expect(*pvc.Spec.VolumeMode).To(Equal(mode))
			}
		},
		Entry("Filesystem", corev1.PersistentVolumeFilesystem),
		Entry("Block", corev1.PersistentVolumeBlock),
	)

	DescribeTable("rejects a restored PVC whose storage differs from the snapshot",
		func(change func(*corev1.PersistentVolumeClaimSpec)) {
			f := newChallengeFixture()
			f.quiesce()
			f.finishSnapshot()
			vm := f.finishClone("a")
			_, _, name := challengeTeamNames(f.challenge, "a")
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(f.client.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, pvc)).To(Succeed())
			change(&pvc.Spec)
			Expect(f.client.Update(ctx, pvc)).To(Succeed())
			f.reconcileChallenge()
			Expect(f.starts[vm.Name]).To(BeZero())
			Expect(apiMeta.FindStatusCondition(f.challenge.Status.Teams[0].Conditions, "Failed").Reason).To(Equal("ResourceConflict"))
		},
		Entry("class", func(s *corev1.PersistentVolumeClaimSpec) { s.StorageClassName = ptr.To("wrong-class") }),
		Entry("mode", func(s *corev1.PersistentVolumeClaimSpec) { s.VolumeMode = ptr.To(corev1.PersistentVolumeBlock) }),
	)
	DescribeTable("preserves the saved memory policy in Manual team VMs",
		func(enabled bool, request string) {
			f := newChallengeFixture()
			f.problem.Spec.Machine.Memory = ptr.To(resource.MustParse("4Gi"))
			f.problem.Spec.Machine.MemoryBalloon = &labv1alpha1.ProblemMemoryBalloonSpec{Enabled: enabled}
			if enabled {
				f.problem.Spec.Machine.Memory = nil
				f.problem.Spec.Machine.MemoryBalloon.Min = ptr.To(resource.MustParse(request))
				f.problem.Spec.Machine.MemoryBalloon.Max = ptr.To(resource.MustParse("4Gi"))
			}
			Expect(f.client.Update(ctx, f.problem)).To(Succeed())
			f.reconcile()
			// テンプレートの変更後、作問VMのReadyを確認してからロックを取得する。
			f.reconcile()
			f.bootTeams()
			content := &snapshotv1.VirtualMachineSnapshotContent{}
			Expect(f.client.Get(ctx, types.NamespacedName{Name: f.challenge.Status.SnapshotRef.ContentRef.Name, Namespace: "default"}, content)).To(Succeed())
			domains := []virtv1.DomainSpec{content.Spec.Source.VirtualMachine.Spec.Template.Spec.Domain}
			for _, team := range f.challenge.Status.Teams {
				vm := &virtv1.VirtualMachine{}
				Expect(f.client.Get(ctx, types.NamespacedName{Name: team.VMRef.Name, Namespace: "default"}, vm)).To(Succeed())
				Expect(*vm.Spec.RunStrategy).To(Equal(virtv1.RunStrategyManual))
				domains = append(domains, vm.Spec.Template.Spec.Domain)
			}
			for _, domain := range domains {
				Expect(domain.Memory.Guest.Cmp(resource.MustParse("4Gi"))).To(BeZero())
				requested := domain.Resources.Requests[corev1.ResourceMemory]
				Expect(requested.Cmp(resource.MustParse(request))).To(BeZero())
				Expect(domain.Devices.AutoattachMemBalloon).To(HaveValue(Equal(enabled)))
			}
		},
		Entry("enabled with overcommit", true, "1Gi"),
		Entry("disabled with fixed memory", false, "4Gi"),
	)

	It("locks the source, saves one snapshot and starts independent Manual VMs only once", func() {
		f := newChallengeFixture()
		f.bootTeams()
		Expect(apiMeta.IsStatusConditionTrue(f.challenge.Status.Conditions, "Ready")).To(BeTrue())
		Expect(f.challenge.Status.SnapshotRef.ContentRef).NotTo(BeNil())
		Expect(f.challenge.Status.SnapshotRef.VolumeSnapshotContentRef).NotTo(BeNil())
		f.reconcile()
		Expect(f.problem.Status.ActiveChallenge.UID).To(Equal(f.challenge.UID))
		Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyHalted))
		for _, team := range f.challenge.Status.Teams {
			vm := &virtv1.VirtualMachine{}
			Expect(f.client.Get(ctx, types.NamespacedName{Name: team.VMRef.Name, Namespace: "default"}, vm)).To(Succeed())
			Expect(*vm.Spec.RunStrategy).To(Equal(virtv1.RunStrategyManual))
			Expect(vm.Spec.Template.Spec.Domain.Devices.AutoattachPodInterface).To(HaveValue(BeFalse()))
			Expect(vm.Labels).NotTo(HaveKey(problemUIDLabel))
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(f.client.Get(ctx, types.NamespacedName{Name: team.PVCRef.Name, Namespace: "default"}, pvc)).To(Succeed())
			Expect(ownedByChallenge(pvc, f.challenge)).To(BeTrue())
			Expect(metav1.GetControllerOf(pvc)).To(BeNil())
			Expect(f.starts[vm.Name]).To(Equal(1))
		}
		Expect(f.challenge.Status.Teams[0].PVCRef.UID).NotTo(Equal(f.challenge.Status.Teams[1].PVCRef.UID))
		f.reconciler = &ProblemChallengeReconciler{Client: f.client, APIReader: f.client, Scheme: f.client.Scheme(), StartVM: f.start}
		f.reconcileChallenge()
		for _, count := range f.starts {
			Expect(count).To(Equal(1))
		}
	})
	It("does not resnapshot or restart team VMs when the author VM readiness is lost after deployment", func() {
		f := newChallengeFixture()
		f.bootTeams()
		vm := f.vm()
		vm.Status.Ready = false
		Expect(f.client.Status().Update(ctx, vm)).To(Succeed())
		f.reconcile()
		f.reconcileChallenge()
		Expect(apiMeta.IsStatusConditionTrue(f.challenge.Status.Conditions, "Ready")).To(BeTrue())
		for _, count := range f.starts {
			Expect(count).To(Equal(1))
		}
	})
	It("waits for both source VMI and launcher Pod to disappear before saving", func() {
		f := newChallengeFixture()
		vmi, pod := f.instanceAndPod()
		f.reconcileChallenge()
		f.reconcile()
		f.reconcileChallenge()
		Expect(apierrors.IsNotFound(f.client.Get(ctx, types.NamespacedName{Name: challengeSnapshotName(f.challenge), Namespace: "default"}, &snapshotv1.VirtualMachineSnapshot{}))).To(BeTrue())
		Expect(f.client.Delete(ctx, vmi)).To(Succeed())
		f.reconcile()
		f.reconcileChallenge()
		Expect(apierrors.IsNotFound(f.client.Get(ctx, types.NamespacedName{Name: challengeSnapshotName(f.challenge), Namespace: "default"}, &snapshotv1.VirtualMachineSnapshot{}))).To(BeTrue())
		Expect(f.client.Delete(ctx, pod)).To(Succeed())
		f.reconcile()
		f.reconcileChallenge()
		Expect(f.snapshot()).NotTo(BeNil())
	})
	It("permanently rejects a losing Challenge even after the winner releases the lock", func() {
		f := newChallengeFixture()
		f.reconcileChallenge()
		loser := f.challenge.DeepCopy()
		loser.Name = "loser"
		loser.ResourceVersion = ""
		loser.Status = labv1alpha1.ProblemChallengeStatus{}
		loser.Finalizers = nil
		Expect(f.client.Create(ctx, loser)).To(Succeed())
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(loser)}
		_, err := f.reconciler.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.client.Get(ctx, request.NamespacedName, loser)).To(Succeed())
		Expect(apiMeta.FindStatusCondition(loser.Status.Conditions, "Accepted").Reason).To(Equal("ProblemLocked"))
		f.reconcile()
		f.problem.Status.ActiveChallenge = nil
		Expect(f.client.Status().Update(ctx, f.problem)).To(Succeed())
		f.reconcile()
		_, err = f.reconciler.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.problem), f.problem)).To(Succeed())
		Expect(f.problem.Status.ActiveChallenge).To(BeNil())
	})
	It("rejects a configuration-only snapshot that excludes the root disk", func() {
		f := newChallengeFixture()
		f.quiesce()
		f.finishSnapshot()
		s := f.snapshot()
		s.Status.SnapshotVolumes.IncludedVolumes = nil
		s.Status.SnapshotVolumes.ExcludedVolumes = []string{"root"}
		Expect(f.client.Status().Update(ctx, s)).To(Succeed())
		f.reconcileChallenge()
		Expect(apiMeta.IsStatusConditionTrue(f.challenge.Status.Conditions, "Failed")).To(BeTrue())
		Expect(f.starts).To(BeEmpty())
		f.reconcile()
		Expect(f.problem.Status.ActiveChallenge).NotTo(BeNil())
	})
	It("never creates a replacement for a lost recorded snapshot", func() {
		f := newChallengeFixture()
		f.quiesce()
		Expect(f.client.Delete(ctx, f.snapshot())).To(Succeed())
		f.reconcileChallenge()
		f.reconcileChallenge()
		Expect(apiMeta.FindStatusCondition(f.challenge.Status.Conditions, "Failed").Reason).To(Equal("SnapshotMissing"))
		Expect(f.starts).To(BeEmpty())
	})
	It("does not restart an initial VMI after it disappears, including after a controller restart", func() {
		f := newChallengeFixture()
		f.bootTeams()
		team := f.challenge.Status.Teams[0]
		vmi := &virtv1.VirtualMachineInstance{}
		Expect(f.client.Get(ctx, types.NamespacedName{Name: team.VMRef.Name, Namespace: "default"}, vmi)).To(Succeed())
		Expect(f.client.Delete(ctx, vmi)).To(Succeed())
		f.reconciler = &ProblemChallengeReconciler{Client: f.client, APIReader: f.client, Scheme: f.client.Scheme(), StartVM: f.start}
		f.reconcileChallenge()
		f.reconcileChallenge()
		Expect(apiMeta.FindStatusCondition(f.challenge.Status.Teams[0].Conditions, "Failed").Reason).To(Equal("VMStopped"))
		Expect(f.starts[team.VMRef.Name]).To(Equal(1))
		Expect(apiMeta.IsStatusConditionTrue(f.challenge.Status.Conditions, "Ready")).To(BeFalse())
	})
	It("does not recreate a lost competition VM", func() {
		f := newChallengeFixture()
		f.bootTeams()
		team := f.challenge.Status.Teams[0]
		vm := &virtv1.VirtualMachine{}
		Expect(f.client.Get(ctx, types.NamespacedName{Name: team.VMRef.Name, Namespace: "default"}, vm)).To(Succeed())
		Expect(f.client.Delete(ctx, vm)).To(Succeed())
		f.reconcileChallenge()
		f.reconcileChallenge()
		Expect(apiMeta.FindStatusCondition(f.challenge.Status.Teams[0].Conditions, "Failed").Reason).To(Equal("VMMissing"))
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(vm), vm))).To(BeTrue())
	})
	It("keeps a team failed after its node returns, without issuing another start", func() {
		f := newChallengeFixture()
		f.bootTeams()
		node := &corev1.Node{}
		Expect(f.client.Get(ctx, types.NamespacedName{Name: "worker"}, node)).To(Succeed())
		node.Status.Conditions[0].Status = corev1.ConditionUnknown
		Expect(f.client.Status().Update(ctx, node)).To(Succeed())
		f.reconcileChallenge()
		Expect(apiMeta.FindStatusCondition(f.challenge.Status.Teams[0].Conditions, "Failed").Reason).To(Equal("NodeUnavailable"))
		node.Status.Conditions[0].Status = corev1.ConditionTrue
		Expect(f.client.Status().Update(ctx, node)).To(Succeed())
		f.reconcileChallenge()
		Expect(apiMeta.IsStatusConditionTrue(f.challenge.Status.Conditions, "Ready")).To(BeFalse())
		for _, count := range f.starts {
			Expect(count).To(Equal(1))
		}
	})
	It("does not resend an ambiguous first start request", func() {
		f := newChallengeFixture()
		f.quiesce()
		f.finishSnapshot()
		f.finishClone("a")
		f.finishClone("b")
		calls := 0
		f.reconciler.StartVM = func(context.Context, *virtv1.VirtualMachine) error {
			calls++
			return fmt.Errorf("connection lost after sending request")
		}
		f.reconcileChallenge()
		f.reconcileChallenge()
		Expect(calls).To(Equal(2))
		for _, team := range f.challenge.Status.Teams {
			Expect(apiMeta.FindStatusCondition(team.Conditions, "Failed").Reason).To(Equal("StartUnconfirmed"))
		}
	})
	It("does not adopt a replacement PVC with the same name", func() {
		f := newChallengeFixture()
		f.bootTeams()
		team := f.challenge.Status.Teams[0]
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(f.client.Get(ctx, types.NamespacedName{Name: team.PVCRef.Name, Namespace: "default"}, pvc)).To(Succeed())
		Expect(f.client.Delete(ctx, pvc)).To(Succeed())
		pvc.ResourceVersion = ""
		pvc.UID = ""
		pvc.OwnerReferences = nil
		Expect(f.client.Create(ctx, pvc)).To(Succeed())
		f.reconcileChallenge()
		Expect(apiMeta.FindStatusCondition(f.challenge.Status.Teams[0].Conditions, "Failed").Reason).To(Equal("ResourceConflict"))
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		Expect(ownedByChallenge(pvc, f.challenge)).To(BeFalse())
	})
	It("holds the lock until VMIs, PVCs and CSI snapshot content finish cleanup", func() {
		f := newChallengeFixture()
		f.bootTeams()
		sourceUID := f.disk().UID
		storage := &csiv1.VolumeSnapshotContent{}
		Expect(f.client.Get(ctx, types.NamespacedName{Name: "saved-storage-content"}, storage)).To(Succeed())
		storage.Finalizers = []string{"test-backend-cleanup"}
		Expect(f.client.Update(ctx, storage)).To(Succeed())
		Expect(f.client.Delete(ctx, f.challenge)).To(Succeed())
		for range 4 {
			f.reconcileChallenge()
		}
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.problem), f.problem)).To(Succeed())
		Expect(f.problem.Status.ActiveChallenge).NotTo(BeNil())
		for _, team := range f.challenge.Status.Teams {
			vmi := &virtv1.VirtualMachineInstance{}
			if f.client.Get(ctx, types.NamespacedName{Name: team.VMRef.Name, Namespace: "default"}, vmi) == nil {
				Expect(f.client.Delete(ctx, vmi)).To(Succeed())
			}
		}
		for range 8 {
			f.reconcileChallenge()
		}
		Expect(f.client.Get(ctx, client.ObjectKeyFromObject(f.problem), f.problem)).To(Succeed())
		Expect(f.problem.Status.ActiveChallenge).NotTo(BeNil())
		Expect(f.disk().UID).To(Equal(sourceUID))
		Expect(f.client.Get(ctx, types.NamespacedName{Name: storage.Name}, storage)).To(Succeed())
		Expect(storage.DeletionTimestamp.IsZero()).To(BeFalse())
		storage.Finalizers = nil
		Expect(f.client.Update(ctx, storage)).To(Succeed())
		f.reconcileChallenge()
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(f.challenge), &labv1alpha1.ProblemChallenge{}))).To(BeTrue())
		f.reconcile()
		Expect(f.problem.Status.ActiveChallenge).To(BeNil())
		Expect(*f.vm().Spec.RunStrategy).To(Equal(virtv1.RunStrategyAlways))
		Expect(f.disk().UID).To(Equal(sourceUID))
	})
	It("fails closed if the root PVC is replaced after the Challenge acquires the lock", func() {
		f := newChallengeFixture()
		f.reconcileChallenge()
		f.reconcile()
		original := &corev1.PersistentVolumeClaim{}
		Expect(f.client.Get(ctx, types.NamespacedName{Name: f.disk().Name, Namespace: f.problem.Namespace}, original)).To(Succeed())
		originalUID := original.UID
		Expect(originalUID).NotTo(BeEmpty())
		Expect(f.client.Delete(ctx, original)).To(Succeed())

		replacement := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: original.Name, Namespace: original.Namespace, OwnerReferences: original.OwnerReferences},
			Spec:       *original.Spec.DeepCopy(),
			Status:     *original.Status.DeepCopy(),
		}
		Expect(f.client.Create(ctx, replacement)).To(Succeed())
		Expect(replacement.UID).NotTo(Equal(originalUID))

		f.reconcileChallenge()

		Expect(apiMeta.FindStatusCondition(f.challenge.Status.Conditions, "Failed").Reason).To(Equal("ResourceConflict"))
		Expect(apierrors.IsNotFound(f.client.Get(ctx, types.NamespacedName{Name: challengeSnapshotName(f.challenge), Namespace: f.challenge.Namespace}, &snapshotv1.VirtualMachineSnapshot{}))).To(BeTrue())
		Expect(f.problem.Status.ActiveChallenge.UID).To(Equal(f.challenge.UID))
	})
	It("discovers restore-created resources during cleanup even if team refs were never saved", func() {
		f := newChallengeFixture()
		f.quiesce()
		f.finishSnapshot()
		f.finishClone("a")
		f.finishClone("b")
		f.challenge.Status.Teams = nil
		Expect(f.client.Status().Update(ctx, f.challenge)).To(Succeed())
		Expect(f.client.Delete(ctx, f.challenge)).To(Succeed())
		for range 12 {
			f.reconcileChallenge()
		}
		Expect(apierrors.IsNotFound(f.client.Get(ctx, client.ObjectKeyFromObject(f.challenge), &labv1alpha1.ProblemChallenge{}))).To(BeTrue())
		Expect(f.disk()).NotTo(BeNil())
	})
	It("maps indirect CSI events and source Problem changes to the correct Challenge", func() {
		f := newChallengeFixture()
		f.quiesce()
		f.finishSnapshot()
		storage := &csiv1.VolumeSnapshotContent{}
		Expect(f.client.Get(ctx, types.NamespacedName{Name: "saved-storage-content"}, storage)).To(Succeed())
		Expect(f.reconciler.childChanged(ctx, storage)).To(ContainElement(ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.challenge)}))
		Expect(f.reconciler.problemChanged(ctx, f.problem)).To(ContainElement(ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.challenge)}))
	})
})
