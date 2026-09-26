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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
)

// ProblemReconcilerはProblemリソースの状態を調整します。
type ProblemReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problems/finalizers,verbs=update

// ReconcileはKubernetesの調整ループから呼び出され、
// 現在の状態をProblemで指定された状態に近づけます。
// TODO: Problemのspecと現在の状態を比較し、
// specに合うよう必要な処理を実装します。
//
// ReconcileとResultの詳細は次を参照してください。
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/reconcile
func (r *ProblemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// TODO: Problemを取得して状態を確認する処理を実装します。
	problem := &labv1alpha1.Problem{}
	if err := r.Get(ctx, req.NamespacedName, problem); err != nil {
		if apierrors.IsNotFound(err) {
			// Problemリソースが見つからない場合は、削除された可能性があるため何もせず終了します。
			return ctrl.Result{}, nil
		}
		// その他のエラーの場合はリトライするためにエラーを返します。
		return ctrl.Result{}, err
	}
	log := logf.FromContext(ctx)
	log.Info("Found Problem resource", "namespace", problem.Namespace, "name", problem.Name)

	base := problem.DeepCopy() // DeepCopyして元の状態を保持しておく DeepCopyは元のオブジェクトを変更せずに安全に操作するために使用します。

	// Status条件を更新します。ここでは、Provisionerが未実装であることを示すReady条件を設定しています。
	changed := apiMeta.SetStatusCondition(&problem.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "ProvisionerNotImplemented",
		Message:            "The process for provisioning the Problem resource is not implemented yet.",
		ObservedGeneration: problem.Generation,
	})

	// 変更があった場合はStatusを更新します。
	if changed {
		if err := r.Status().Patch(ctx, problem, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManagerはManagerにControllerを登録します。
func (r *ProblemReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&labv1alpha1.Problem{}).
		Named("problem").
		Complete(r)
}
