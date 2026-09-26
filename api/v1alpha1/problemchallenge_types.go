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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// このファイルを編集してProblemChallengeのAPIを定義します。
// フィールドを追加するときは、JSONに変換できるようJSONタグも指定してください。

// ProblemChallengeSpecは、ある出題機会に問題環境を展開する設定を表します。
type ProblemChallengeSpec struct {
	// ProblemRefは、出題するProblemの名前を指定します。
	// ProblemChallengeと同じnamespaceにあるProblemを参照します。
	// +required
	ProblemRef corev1.LocalObjectReference `json:"problemRef"`

	// Placementは、この出題機会に作成するすべてのVMの配置条件を指定します。
	// +required
	Placement ProblemChallengePlacementSpec `json:"placement"`

	// Teamsは、この出題機会に問題環境を作成するチームの一覧です。
	// チームごとに1つの問題環境を作成します。
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=id
	// +required
	Teams []ProblemChallengeTeamSpec `json:"teams"`
}

// ProblemChallengePlacementSpecは、チーム環境を実行するKubernetes Nodeの条件を表します。
type ProblemChallengePlacementSpec struct {
	// NodeSelectorは、KubeVirtのVMを実行するKubernetes Nodeをラベルで絞り込みます。
	// すべてのチーム環境を同じNodeに配置するには、対象Nodeだけに付けたラベルを指定してください。
	// +kubebuilder:validation:MinProperties=1
	// +required
	NodeSelector map[string]string `json:"nodeSelector"`
}

// ProblemChallengeTeamSpecは、問題環境を作成するチームを表します。
type ProblemChallengeTeamSpec struct {
	// IDは、チームを識別する一意なIDを指定します。
	// +kubebuilder:validation:MinLength=1
	// +required
	ID string `json:"id"`
}

// ProblemChallengeStatusは、ProblemChallengeの現在の状態を表します。
type ProblemChallengeStatus struct {
	// コントローラーが確認した状態を表すフィールドをここに追加します。
	// 型やマーカーを変更したときは、make manifestsとmake generateを実行してください。

	// Kubernetes APIの設計規約については、次を参照してください。
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// Conditionsは、ProblemChallengeの状態を種類ごとに表します。
	// 各条件は異なる側面の状態を示し、種類ごとに1つ設定します。
	//
	// よく使われる条件の種類には、次のものがあります。
	// - "Available": リソースが利用可能な状態
	// - "Progressing": リソースを作成または更新している状態
	// - "Degraded": リソースが期待した状態にならない、または維持できない状態
	//
	// 各条件の状態にはTrue、False、Unknownのいずれかを設定します。
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ProblemChallengeは、ある出題機会における全チーム分の問題環境を表すリソースです。
type ProblemChallenge struct {
	metav1.TypeMeta `json:",inline"`

	// metadataには、名前やNamespaceなどの情報が入ります。
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// specには、出題するProblem、配置条件、対象チームを指定します。
	// +required
	Spec ProblemChallengeSpec `json:"spec"`

	// statusには、コントローラーが確認した現在の状態が入ります。
	// +optional
	Status ProblemChallengeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ProblemChallengeListは、ProblemChallengeの一覧です。
type ProblemChallengeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ProblemChallenge `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ProblemChallenge{}, &ProblemChallengeList{})
		return nil
	})
}
