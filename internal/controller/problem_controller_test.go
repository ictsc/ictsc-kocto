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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
)

var _ = Describe("Problem Controller", func() {
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
		problem := &labv1alpha1.Problem{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Problem")
			err := k8sClient.Get(ctx, typeNamespacedName, problem)
			if err != nil && errors.IsNotFound(err) {
				resource := &labv1alpha1.Problem{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: labv1alpha1.ProblemSpec{
						Source: labv1alpha1.ProblemSourceSpec{
							Repository:   "https://example.invalid/problems.git",
							CommitHash:   "0123456789012345678901234567890123456789",
							TopologyFile: "topology.yaml",
						},
						Machine: labv1alpha1.ProblemMachineSpec{
							CPU:    2,
							Memory: resource.MustParse("4Gi"),
							Disk:   resource.MustParse("10Gi"),
						},
						Access: []labv1alpha1.AccessEndpoint{{
							Name:      "web",
							Node:      "web-node",
							Interface: "eth0",
						}},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &labv1alpha1.Problem{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Problem")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &ProblemReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
