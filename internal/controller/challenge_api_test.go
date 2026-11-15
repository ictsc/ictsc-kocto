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
	"io"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	"github.com/ictsc/ictsc-kocto/internal/bundle"
	csiv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clonev1 "kubevirt.io/api/clone/v1beta1"
	virtv1 "kubevirt.io/api/core/v1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var _ = Describe("Challenge API lock acquisition", func() {
	It("locks a bundle-backed Problem and retains both disk identities across a controller restart", func() {
		p := &labv1alpha1.Problem{ObjectMeta: metav1.ObjectMeta{GenerateName: "challenge-bundle-source-", Namespace: "default"}, Spec: labv1alpha1.ProblemSpec{Source: labv1alpha1.ProblemSourceSpec{Repository: "https://github.com/example/lab", CommitHash: "0123456789012345678901234567890123456789", TopologyFile: "topology.yaml"}, Machine: labv1alpha1.ProblemMachineSpec{CPU: 1, Memory: ptr.To(resource.MustParse("128Mi")), Disk: resource.MustParse("14Gi"), Image: labv1alpha1.ProblemMachineImageSpec{ContainerDisk: "example.invalid/disk:test"}}, Access: []labv1alpha1.AccessEndpoint{{Name: "console", Node: "router", Interface: "eth0"}}}}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		cleanup := []client.Object{p}
		DeferCleanup(func() {
			for i := len(cleanup) - 1; i >= 0; i-- {
				object := cleanup[i]
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(object), object); apierrors.IsNotFound(err) {
					continue
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
				object.SetFinalizers(nil)
				Expect(k8sClient.Update(ctx, object)).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, object))).To(Succeed())
			}
		})
		dv := problemDataVolume(p, "test-immediate")
		Expect(controllerutil.SetControllerReference(p, dv, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, dv)).To(Succeed())
		cleanup = append(cleanup, dv)
		dv.Status.Phase = cdiv1.Succeeded
		Expect(k8sClient.Status().Update(ctx, dv)).To(Succeed())
		root := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dv.Name, Namespace: p.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(dv, cdiv1.SchemeGroupVersion.WithKind("DataVolume"))}}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: dv.Spec.Storage.AccessModes, StorageClassName: dv.Spec.Storage.StorageClassName, VolumeMode: dv.Spec.Storage.VolumeMode, Resources: dv.Spec.Storage.Resources}}
		Expect(k8sClient.Create(ctx, root)).To(Succeed())
		cleanup = append(cleanup, root)
		root.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, root)).To(Succeed())
		p.Status.BundleRef = &labv1alpha1.ProblemBundleReference{SourceHash: bundle.SourceHash(p.Spec.Source), BuilderImage: "example.invalid/builder", StorageClassName: "test-immediate", Ready: true}
		bundlePVC := problemBundlePVC(p)
		Expect(controllerutil.SetControllerReference(p, bundlePVC, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, bundlePVC)).To(Succeed())
		cleanup = append(cleanup, bundlePVC)
		bundlePVC.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, bundlePVC)).To(Succeed())
		p.Status.BundleRef.PVCUID = bundlePVC.UID
		vm := problemVirtualMachine(p)
		Expect(controllerutil.SetControllerReference(p, vm, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		cleanup = append(cleanup, vm)
		vm.Status.Ready = true
		vm.Status.ObservedGeneration = vm.Generation
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
		p.Status.DiskRef = &labv1alpha1.ProblemDiskReference{DiskName: dv.Name, UID: dv.UID, PVCUID: root.UID}
		p.Status.VMRef = &labv1alpha1.ProblemVMReference{VMName: vm.Name, UID: vm.UID}
		apiMeta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "VMReady", Status: metav1.ConditionTrue, Reason: "VMReady", ObservedGeneration: p.Generation})
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		c := &labv1alpha1.ProblemChallenge{ObjectMeta: metav1.ObjectMeta{GenerateName: "challenge-bundle-api-", Namespace: p.Namespace}, Spec: labv1alpha1.ProblemChallengeSpec{ProblemRef: labv1alpha1.ProblemReference{Name: p.Name}, Placement: labv1alpha1.ProblemChallengePlacementSpec{NodeSelector: map[string]string{"node": "worker"}}, Teams: []labv1alpha1.ProblemChallengeTeamSpec{{ID: "a"}}}}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		cleanup = append(cleanup, c)
		r := &ProblemChallengeReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		Expect(apiMeta.IsStatusConditionTrue(c.Status.Conditions, "Accepted")).To(BeTrue())
		Expect(c.Status.SourceProblem.DiskPVCUID).To(Equal(root.UID))
		Expect(c.Status.SourceProblem.BundlePVCUID).To(Equal(bundlePVC.UID))
		Expect(c.Status.SourceProblem.BundleSourceHash).To(Equal(p.Status.BundleRef.SourceHash))
		r = &ProblemChallengeReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		Expect(p.Status.ActiveChallenge.UID).To(Equal(c.UID))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), vm)).To(Succeed())
		vm.Spec.RunStrategy = ptr.To(virtv1.RunStrategyHalted)
		Expect(k8sClient.Update(ctx, vm)).To(Succeed())
		apiMeta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Quiesced", Status: metav1.ConditionTrue, Reason: "AuthorStopped", ObservedGeneration: p.Generation})
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		Expect(err).NotTo(HaveOccurred())
		s := &snapshotv1.VirtualMachineSnapshot{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: challengeSnapshotName(c), Namespace: p.Namespace}, s)).To(Succeed())
		cleanup = append(cleanup, s)
		Expect(s.Spec.Source.Name).To(Equal(vm.Name))
		Expect(ownedByChallenge(s, c)).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		Expect(c.Status.SnapshotRef.UID).To(Equal(s.UID))
		content := &snapshotv1.VirtualMachineSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: challengeSnapshotContentName(s.UID), Namespace: p.Namespace}, Spec: snapshotv1.VirtualMachineSnapshotContentSpec{VirtualMachineSnapshotName: &s.Name, Source: snapshotv1.SourceSpec{VirtualMachine: &snapshotv1.VirtualMachine{ObjectMeta: vm.ObjectMeta, Spec: vm.Spec}}}}
		for _, disk := range []struct {
			name string
			pvc  *corev1.PersistentVolumeClaim
		}{{"root", root}, {"bundle", bundlePVC}} {
			volumeName := string(s.UID) + "-" + disk.name
			content.Spec.VolumeBackups = append(content.Spec.VolumeBackups, snapshotv1.VolumeBackup{VolumeName: disk.name, PersistentVolumeClaim: snapshotv1.PersistentVolumeClaim{ObjectMeta: disk.pvc.ObjectMeta, Spec: disk.pvc.Spec}, VolumeSnapshotName: &volumeName})
		}
		Expect(k8sClient.Create(ctx, content)).To(Succeed())
		cleanup = append(cleanup, content)
		content.Status = &snapshotv1.VirtualMachineSnapshotContentStatus{ReadyToUse: ptr.To(true)}
		Expect(k8sClient.Status().Update(ctx, content)).To(Succeed())
		for _, backup := range content.Spec.VolumeBackups {
			storageName := "storage-" + *backup.VolumeSnapshotName
			volume := &csiv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: *backup.VolumeSnapshotName, Namespace: p.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(content, snapshotv1.SchemeGroupVersion.WithKind("VirtualMachineSnapshotContent"))}}, Spec: csiv1.VolumeSnapshotSpec{Source: csiv1.VolumeSnapshotSource{PersistentVolumeClaimName: ptr.To(backup.PersistentVolumeClaim.Name)}}}
			Expect(k8sClient.Create(ctx, volume)).To(Succeed())
			cleanup = append(cleanup, volume)
			volume.Status = &csiv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true), BoundVolumeSnapshotContentName: &storageName}
			Expect(k8sClient.Status().Update(ctx, volume)).To(Succeed())
			storage := &csiv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: storageName}, Spec: csiv1.VolumeSnapshotContentSpec{VolumeSnapshotRef: corev1.ObjectReference{Name: volume.Name, Namespace: volume.Namespace, UID: volume.UID}, DeletionPolicy: csiv1.VolumeSnapshotContentDelete, Driver: "test.csi", Source: csiv1.VolumeSnapshotContentSource{VolumeHandle: ptr.To("test-handle")}}}
			Expect(k8sClient.Create(ctx, storage)).To(Succeed())
			cleanup = append(cleanup, storage)
		}
		s.Status = &snapshotv1.VirtualMachineSnapshotStatus{SourceUID: &vm.UID, VirtualMachineSnapshotContentName: &content.Name, Phase: snapshotv1.Succeeded, ReadyToUse: ptr.To(true), SnapshotVolumes: &snapshotv1.SnapshotVolumesLists{IncludedVolumes: []string{"root", "bundle"}}}
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		Expect(apiMeta.IsStatusConditionTrue(c.Status.Conditions, "SnapshotReady")).To(BeTrue())
		Expect(c.Status.SnapshotRef.VolumeSnapshotRef).NotTo(BeNil())
		Expect(c.Status.SnapshotRef.BundleVolumeSnapshotRef).NotTo(BeNil())
		Expect(c.Status.SnapshotRef.VolumeSnapshotContentRef).NotTo(BeNil())
		Expect(c.Status.SnapshotRef.BundleVolumeSnapshotContentRef).NotTo(BeNil())
		cloneName, teamVMName, teamRootName := challengeTeamNames(c, "a")
		clone := &clonev1.VirtualMachineClone{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cloneName, Namespace: p.Namespace}, clone)).To(Succeed())
		cleanup = append(cleanup, clone)
		Expect(clone.Spec.Source.Name).To(Equal(s.Name))
		clone.Status.Phase = clonev1.Succeeded
		Expect(k8sClient.Status().Update(ctx, clone)).To(Succeed())
		teamVM := problemVirtualMachine(p)
		teamVM.Name = teamVMName
		teamVM.Spec.RunStrategy = ptr.To(virtv1.RunStrategyManual)
		teamVM.Annotations = map[string]string{"restore.kubevirt.io/lastRestoreUID": "tmp-restore-" + string(clone.UID) + "-test"}
		teamVM.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = teamRootName
		teamVM.Spec.Template.Spec.Volumes[1].PersistentVolumeClaim.ClaimName = teamVMName + "-bundle"
		Expect(k8sClient.Create(ctx, teamVM)).To(Succeed())
		cleanup = append(cleanup, teamVM)
		for _, disk := range []struct {
			name     string
			source   *corev1.PersistentVolumeClaim
			snapshot string
		}{{teamRootName, root, c.Status.SnapshotRef.VolumeSnapshotRef.Name}, {teamVMName + "-bundle", bundlePVC, c.Status.SnapshotRef.BundleVolumeSnapshotRef.Name}} {
			restored := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: disk.name, Namespace: p.Namespace}, Spec: *disk.source.Spec.DeepCopy()}
			restored.Spec.DataSource = &corev1.TypedLocalObjectReference{APIGroup: ptr.To("snapshot.storage.k8s.io"), Kind: "VolumeSnapshot", Name: disk.snapshot}
			Expect(k8sClient.Create(ctx, restored)).To(Succeed())
			cleanup = append(cleanup, restored)
			restored.Status.Phase = corev1.ClaimBound
			Expect(k8sClient.Status().Update(ctx, restored)).To(Succeed())
		}
		starts := 0
		r.StartVM = func(context.Context, *virtv1.VirtualMachine) error { starts++; return nil }
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		Expect(err).NotTo(HaveOccurred())
		Expect(starts).To(Equal(1))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
		Expect(c.Status.Teams[0].PVCRef.Name).To(Equal(teamRootName))
		Expect(c.Status.Teams[0].BundlePVCRef.Name).To(Equal(teamVMName + "-bundle"))
	})
	It("preserves bundle source, snapshot and team identities in API status", func() {
		c := &labv1alpha1.ProblemChallenge{ObjectMeta: metav1.ObjectMeta{GenerateName: "challenge-bundle-status-", Namespace: "default"}, Spec: labv1alpha1.ProblemChallengeSpec{ProblemRef: labv1alpha1.ProblemReference{Name: "source"}, Placement: labv1alpha1.ProblemChallengePlacementSpec{NodeSelector: map[string]string{"node": "worker"}}, Teams: []labv1alpha1.ProblemChallengeTeamSpec{{ID: "a"}}}}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, c)).To(Succeed()) })
		c.Status.SourceProblem = &labv1alpha1.SourceProblemReference{Name: "source", UID: "problem-uid", Generation: 1, DiskPVCUID: "root-uid", BundlePVCUID: "bundle-uid", BundleSourceHash: "bundle-hash"}
		c.Status.SnapshotRef = &labv1alpha1.ChallengeSnapshotReference{Name: "snapshot", UID: "snapshot-uid", BundleVolumeSnapshotRef: &labv1alpha1.TeamResourceReference{Name: "bundle-snapshot", UID: "bundle-snapshot-uid"}, BundleVolumeSnapshotContentRef: &labv1alpha1.TeamResourceReference{Name: "bundle-content", UID: "bundle-content-uid"}}
		c.Status.Teams = []labv1alpha1.ProblemChallengeTeamStatus{{ID: "a", BundlePVCRef: &labv1alpha1.TeamResourceReference{Name: "team-bundle", UID: "team-bundle-uid"}}}
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
		actual := &labv1alpha1.ProblemChallenge{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), actual)).To(Succeed())
		Expect(actual.Status).To(Equal(c.Status))
	})
	It("sends the initial start to the KubeVirt subresource API with the manager credentials", func() {
		called := 0
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			defer GinkgoRecover()
			called++
			Expect(request.Method).To(Equal("PUT"))
			Expect(request.URL.Path).To(Equal("/apis/subresources.kubevirt.io/v1/namespaces/default/virtualmachines/team/start"))
			Expect(request.Header.Get("Authorization")).To(Equal("Bearer test-controller-token"))
			Expect(request.Header.Get("Content-Type")).To(Equal("application/json"))
			body, err := io.ReadAll(request.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(body)).To(Equal("{}"))
			writer.WriteHeader(http.StatusAccepted)
		}))
		DeferCleanup(server.Close)
		scheme := runtime.NewScheme()
		starter, err := newChallengeVMStarter(&rest.Config{Host: server.URL, BearerToken: "test-controller-token"}, scheme)
		Expect(err).NotTo(HaveOccurred())
		Expect(starter(context.Background(), &virtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "default"}})).To(Succeed())
		Expect(called).To(Equal(1))
	})

	It("has one winner under concurrent API updates, preserves the lock on restart, and releases it only after cleanup", func() {
		p := &labv1alpha1.Problem{ObjectMeta: metav1.ObjectMeta{GenerateName: "challenge-source-", Namespace: "default"}, Spec: labv1alpha1.ProblemSpec{Source: labv1alpha1.ProblemSourceSpec{Repository: "https://example.invalid/problems", CommitHash: "test", TopologyFile: "topology.yaml"}, Machine: labv1alpha1.ProblemMachineSpec{CPU: 1, Memory: ptr.To(resource.MustParse("128Mi")), Disk: resource.MustParse("1Gi"), Image: labv1alpha1.ProblemMachineImageSpec{ContainerDisk: "example.invalid/disk:test"}}, Access: []labv1alpha1.AccessEndpoint{{Name: "console", Node: "router", Interface: "eth0"}}}}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		vmName, diskName := problemResourceNames(p)
		cleanup := []client.Object{p, &virtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: vmName, Namespace: p.Namespace}}, &cdiv1.DataVolume{ObjectMeta: metav1.ObjectMeta{Name: diskName, Namespace: p.Namespace}}, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: diskName, Namespace: p.Namespace}}}
		DeferCleanup(func() {
			for _, object := range cleanup {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(object), object)
				if apierrors.IsNotFound(err) {
					continue
				}
				Expect(err).NotTo(HaveOccurred())
				object.SetFinalizers(nil)
				Expect(k8sClient.Update(ctx, object)).To(Succeed())
				err = k8sClient.Delete(ctx, object)
				Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue())
			}
		})
		author := &ProblemReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), StorageClass: "test-immediate"}
		sourceRequest := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}
		_, err := author.Reconcile(ctx, sourceRequest)
		Expect(err).NotTo(HaveOccurred())
		dv := &cdiv1.DataVolume{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: diskName, Namespace: p.Namespace}, dv)).To(Succeed())
		dv.Status.Phase = cdiv1.Succeeded
		Expect(k8sClient.Status().Update(ctx, dv)).To(Succeed())
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: diskName, Namespace: p.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: cdiv1.SchemeGroupVersion.String(), Kind: "DataVolume", Name: dv.Name, UID: dv.UID}}}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: dv.Spec.Storage.AccessModes, StorageClassName: dv.Spec.Storage.StorageClassName, VolumeMode: dv.Spec.Storage.VolumeMode, Resources: dv.Spec.Storage.Resources}}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		pvc.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
		_, err = author.Reconcile(ctx, sourceRequest)
		Expect(err).NotTo(HaveOccurred())
		vm := &virtv1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: vmName, Namespace: p.Namespace}, vm)).To(Succeed())
		vm.Status.Ready = true
		vm.Status.ObservedGeneration = vm.Generation
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
		_, err = author.Reconcile(ctx, sourceRequest)
		Expect(err).NotTo(HaveOccurred())
		challenges := []*labv1alpha1.ProblemChallenge{}
		for range 2 {
			c := &labv1alpha1.ProblemChallenge{ObjectMeta: metav1.ObjectMeta{GenerateName: "challenge-race-", Namespace: p.Namespace}, Spec: labv1alpha1.ProblemChallengeSpec{ProblemRef: labv1alpha1.ProblemReference{Name: p.Name}, Placement: labv1alpha1.ProblemChallengePlacementSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": "worker"}}, Teams: []labv1alpha1.ProblemChallengeTeamSpec{{ID: "a"}}}}
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			challenges = append(challenges, c)
			cleanup = append(cleanup, c)
		}
		r := &ProblemChallengeReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		var wait sync.WaitGroup
		results := make(chan error, 2)
		for _, c := range challenges {
			wait.Go(func() {
				defer GinkgoRecover()
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
				results <- err
			})
		}
		wait.Wait()
		close(results)
		for err := range results {
			Expect(err == nil || apierrors.IsConflict(err)).To(BeTrue(), "unexpected reconcile error: %v", err)
		}
		for _, c := range challenges {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		Expect(p.Status.ActiveChallenge).NotTo(BeNil())
		var winner, loser *labv1alpha1.ProblemChallenge
		for _, c := range challenges {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
			if c.UID == p.Status.ActiveChallenge.UID {
				winner = c
			} else {
				loser = c
			}
		}
		Expect(apiMeta.IsStatusConditionTrue(winner.Status.Conditions, "Accepted")).To(BeTrue())
		Expect(apiMeta.FindStatusCondition(loser.Status.Conditions, "Accepted").Reason).To(Equal("ProblemLocked"))
		locked := p.DeepCopy()
		locked.Spec.Machine.CPU = 2
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, locked))).To(BeTrue())
		_, err = author.Reconcile(ctx, sourceRequest)
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(winner)})
		Expect(err).NotTo(HaveOccurred())
		s := &snapshotv1.VirtualMachineSnapshot{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: challengeSnapshotName(winner), Namespace: p.Namespace}, s)).To(Succeed())
		cleanup = append(cleanup, s)
		// 新しい参照フィールドもstatus subresourceから保存できることを実APIで確認する。
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(winner), winner)).To(Succeed())
		winner.Status.SnapshotRef.ContentRef = &labv1alpha1.TeamResourceReference{Name: "content", UID: "content-uid"}
		winner.Status.SnapshotRef.VolumeSnapshotRef = &labv1alpha1.TeamResourceReference{Name: "volume", UID: "volume-uid"}
		winner.Status.SnapshotRef.VolumeSnapshotContentRef = &labv1alpha1.TeamResourceReference{Name: "storage", UID: "storage-uid"}
		Expect(k8sClient.Status().Update(ctx, winner)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(winner), winner)).To(Succeed())
		Expect(winner.Status.SnapshotRef.ContentRef.UID).To(Equal(types.UID("content-uid")))
		r = &ProblemChallengeReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(winner)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		Expect(p.Status.ActiveChallenge.UID).To(Equal(winner.UID))
		Expect(k8sClient.Delete(ctx, winner)).To(Succeed())
		Eventually(func() bool {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(winner)})
			if err != nil {
				return false
			}
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(winner), &labv1alpha1.ProblemChallenge{}))
		}, 5*time.Second).Should(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		Expect(p.Status.ActiveChallenge).To(BeNil())
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(loser)})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		Expect(p.Status.ActiveChallenge).To(BeNil())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
	})
})
