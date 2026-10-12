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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// このファイルを編集してProblemChallengeのAPIを定義します。
// フィールドを追加するときは、JSONに変換できるようJSONタグも指定してください。

// ProblemChallengeSpecは、ある出題機会に問題環境を展開する設定を表します。
// 作成後は変更できません。
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ProblemChallenge spec is immutable"
type ProblemChallengeSpec struct {
	// ProblemRefは、出題するProblemの名前を指定します。
	// ProblemChallengeと同じnamespaceにあるProblemを参照します。
	// +required
	ProblemRef ProblemReference `json:"problemRef"`

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

// ProblemReferenceは、同じnamespaceにあるProblemへの参照を表します。
type ProblemReference struct {
	// Nameは、参照するProblemの名前を指定します。
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
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

	// SourceProblemは、出題元のProblemを示します
	// +optional
	SourceProblem *SourceProblemReference `json:"sourceProblem,omitempty"`

	// SnapshotRefは、出題元のProblemのスナップショットを示します。
	// +optional
	SnapshotRef *ChallengeSnapshotReference `json:"snapshotRef,omitempty"`

	// Teamsは、各チームの問題環境の状態を表します。
	// +listType=map
	// +listMapKey=id
	// +optional
	Teams []ProblemChallengeTeamStatus `json:"teams,omitempty"`
}

// SourceProblemReferenceは、出題元のProblemへの参照を表します。
type SourceProblemReference struct {
	// Nameは、参照するProblemの名前を指定します。
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// UIDは、参照するProblemのUIDを指定します。
	// +required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	UID types.UID `json:"uid"`

	// Generationは、ロック取得時のProblemのgenerationを記録します。
	// +required
	Generation int64 `json:"generation"`
}

// ChallengeSnapshotReferenceは、出題元のProblemのスナップショットへの参照を表します。
type ChallengeSnapshotReference struct {
	// Nameは、参照するスナップショットの名前を指定します。
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// UIDは、参照するスナップショットのUIDを指定します。
	// +required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	UID types.UID `json:"uid"`

	// ContentRefは、保存したVM構成とディスク一覧を持つVirtualMachineSnapshotContentを示す。
	// 削除途中でcontrollerが再起動しても、同じ子リソースの片付けを続けるために記録する。
	// +optional
	ContentRef *TeamResourceReference `json:"contentRef,omitempty"`

	// VolumeSnapshotRefは、rootディスクのCSI VolumeSnapshotを示す。同じnamespaceにある。
	// +optional
	VolumeSnapshotRef *TeamResourceReference `json:"volumeSnapshotRef,omitempty"`

	// VolumeSnapshotContentRefは、rootディスクのCSI VolumeSnapshotContentを示す。
	// このリソースはクラスタ全体に属するため、namespaceを指定せずに参照する。
	// +optional
	VolumeSnapshotContentRef *TeamResourceReference `json:"volumeSnapshotContentRef,omitempty"`
}

// ProblemChallengeTeamStatusは、各チームの問題環境の状態を表します。
type ProblemChallengeTeamStatus struct {
	// IDは、チームの識別子を指定します。
	// +kubebuilder:validation:MinLength=1
	// +required
	ID string `json:"id"`

	// CloneRefは、チーム用のVirtualMachineCloneへの参照を示します。
	// +optional
	CloneRef *TeamResourceReference `json:"cloneRef,omitempty"`

	// VMRefは、復元したチームVMへの参照を示します。
	// +optional
	VMRef *TeamResourceReference `json:"vmRef,omitempty"`

	// PVCRefは、復元したチームのroot PVCへの参照を示します。
	// +optional
	PVCRef *TeamResourceReference `json:"pvcRef,omitempty"`

	// Conditionsは、チームの問題環境の状態を種類ごとに表します。
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// TeamResourceReferenceは、チーム環境を構成するリソースへの参照を表します。
type TeamResourceReference struct {
	// Nameは、参照するリソースの名前を記録します。
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// UIDは、参照するリソースのUIDを記録します。
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +required
	UID types.UID `json:"uid"`
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
