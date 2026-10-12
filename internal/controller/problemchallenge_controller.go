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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
)

// ProblemChallengeReconcilerは、競技用VMの展開と状態の管理を行う。処理はまだ未実装。
// 競技の公平性のため、競技用VMはHA対象にしない。障害や消失後の自動再起動、
// 別ノードへの移動、スナップショットからの自動再複製は行わず、障害を報告する。
// controller自体の復旧と処理の再開は許可するが、それを競技用VMの復旧契機にはしない。
type ProblemChallengeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problemchallenges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problemchallenges/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lab.icttoracon.net,resources=problemchallenges/finalizers,verbs=update

// Reconcileは、今後ロック取得、作問VMの停止確認、Snapshot保存、チーム展開を進める入口。
// 現在は未実装なので、リソースの作成やstatus更新を行わずに終了する。
func (r *ProblemChallengeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// TODO: 同じ確定済みSnapshotから全チームを展開し、初回起動と障害後の復旧を区別する。
	// 作問VM用のAlwaysを競技用VMへ適用しない。Manualなどで初回のみ起動し、
	// controllerの再起動や反復Reconcileでも失敗した競技用VMを再作成や再起動しない。
	// 展開完了後はチームVMとPVCの状態を確認し、作問VMのReadyには依存させない。

	return ctrl.Result{}, nil
}

// SetupWithManagerは、ProblemChallengeの変更を受けてReconcileが呼ばれるように監視を登録する。
func (r *ProblemChallengeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&labv1alpha1.ProblemChallenge{}).
		Named("problemchallenge").
		Complete(r)
}
