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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
)

var _ = Describe("ProblemChallenge Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		problemchallenge := &labv1alpha1.ProblemChallenge{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ProblemChallenge")
			err := k8sClient.Get(ctx, typeNamespacedName, problemchallenge)
			if err != nil && errors.IsNotFound(err) {
				resource := &labv1alpha1.ProblemChallenge{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: labv1alpha1.ProblemChallengeSpec{
						ProblemRef: labv1alpha1.ProblemReference{Name: "problem-sample"},
						Placement: labv1alpha1.ProblemChallengePlacementSpec{
							NodeSelector: map[string]string{
								"lab.icttoracon.net/kubevirt-node": "worker-01",
							},
						},
						Teams: []labv1alpha1.ProblemChallengeTeamSpec{{ID: "team-001"}},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// テストで作成したProblemChallengeを削除する。
			resource := &labv1alpha1.ProblemChallenge{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ProblemChallenge")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ProblemChallengeReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// Reconcile後の状態を確認する。
			// 必要なstatus条件はここで検証する。
		})

		It("rejects an empty Problem reference name", func() {
			resource := &labv1alpha1.ProblemChallenge{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-problem-reference",
					Namespace: resourceNamespace,
				},
				Spec: labv1alpha1.ProblemChallengeSpec{
					ProblemRef: labv1alpha1.ProblemReference{},
					Placement: labv1alpha1.ProblemChallengePlacementSpec{
						NodeSelector: map[string]string{
							"lab.icttoracon.net/kubevirt-node": "worker-01",
						},
					},
					Teams: []labv1alpha1.ProblemChallengeTeamSpec{{ID: "team-001"}},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(errors.IsInvalid(err)).To(BeTrue())
		})
	})
})
