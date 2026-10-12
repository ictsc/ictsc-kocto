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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	virtv1 "kubevirt.io/api/core/v1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Challenge API lock acquisition", func() {
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
		p := &labv1alpha1.Problem{ObjectMeta: metav1.ObjectMeta{GenerateName: "challenge-source-", Namespace: "default"}, Spec: labv1alpha1.ProblemSpec{Source: labv1alpha1.ProblemSourceSpec{Repository: "https://example.invalid/problems", CommitHash: "test", TopologyFile: "topology.yaml"}, Machine: labv1alpha1.ProblemMachineSpec{CPU: 1, Memory: resource.MustParse("128Mi"), Disk: resource.MustParse("1Gi"), Image: labv1alpha1.ProblemMachineImageSpec{ContainerDisk: "example.invalid/disk:test"}}, Access: []labv1alpha1.AccessEndpoint{{Name: "console", Node: "router", Interface: "eth0"}}}}
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
