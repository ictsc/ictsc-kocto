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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// Problem APIで使う型を定義します。
// フィールドを追加するときはJSONタグも指定してください。

// ProblemSpecはProblemに指定する内容を表します。
type ProblemSpec struct {
	// 作問者が指定するフィールドをここに追加します。
	// 型を変更したらmake manifestsとmake generateを実行してください。
	// Kubebuilderのマーカーを使うと入力値を検証できます。

	// Sourceは、問題を定義するGitリポジトリやディレクトリの情報を指定します。
	// +required
	Source ProblemSourceSpec `json:"source"`

	// Machineは、問題環境を動かすVMのスペックを指定します。
	// +required
	Machine ProblemMachineSpec `json:"machine"`

	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	// +required
	Access []AccessEndpoint `json:"access"`
}

// ProblemSourceSpecは問題のソース情報を表す
type ProblemSourceSpec struct {
	// RepositoryはGitリポジトリのURLを指定する
	// +required
	Repository string `json:"repository"`

	// CommitHashはGitリポジトリのコミットハッシュを指定する
	// +required
	CommitHash string `json:"commitHash"`

	// DirectoryはGitリポジトリ内のディレクトリを指定する
	// 空の場合はリポジトリのルートディレクトリを指定する
	// +optional
	Directory string `json:"directory,omitempty"`

	// TopologyFileは問題環境のトポロジーファイルを指定する
	// Directoryからの相対パスを指定する
	// +required
	TopologyFile string `json:"topologyFile"`

	// AnsiblePlaybookは問題環境の構築に使用するAnsibleプレイブックを指定する
	// Directoryからの相対パスを指定する
	// +optional
	AnsiblePlaybook *string `json:"ansiblePlaybook,omitempty"`
}

// ProblemMachineSpecは、問題環境を動かすVMのスペックを表します。
type ProblemMachineSpec struct {
	// CPUはマシンのCPUコア数を指定する
	// +kubebuilder:validation:Minimum=1
	// +required
	CPU int32 `json:"cpu"`

	// Memoryはマシンのメモリ容量を指定する
	// +kubebuilder:validation:XValidation:rule="isQuantity(string(self)) && quantity(string(self)).isGreaterThan(quantity('0'))",message="machine.memory must be a valid quantity greater than zero"
	// +required
	Memory resource.Quantity `json:"memory"`

	// Diskは初回取り込み時のroot PVCの要求容量を指定する。
	// 作成後は変更できない。
	// +kubebuilder:validation:XValidation:rule="isQuantity(string(self)) && quantity(string(self)).isGreaterThan(quantity('0'))",message="machine.disk must be a valid quantity greater than zero"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machine.disk is immutable"
	// +required
	Disk resource.Quantity `json:"disk"`

	// Imageは、初回取り込み元のイメージを指定する。
	// 作成後は変更できない。
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machine.image is immutable"
	// +required
	Image ProblemMachineImageSpec `json:"image"`
}

// ProblemMachineImageSpecは、問題環境VMの起動元を表します。
type ProblemMachineImageSpec struct {
	// ContainerDiskは、VMの起動元として使うコンテナディスクイメージを指定する
	// +kubebuilder:validation:MinLength=1
	// +required
	ContainerDisk string `json:"containerDisk"`
}

// AccessEndpointは、参加者が問題環境へ接続するためのエンドポイントを表します。
type AccessEndpoint struct {
	// Nameはアクセスエンドポイントの名前を指定する
	// +required
	Name string `json:"name"`

	// Nodeはアクセスエンドポイントが接続されるノードを指定する
	// +required
	Node string `json:"node"`

	// Interfaceはアクセスエンドポイントが接続されるインターフェースを指定する
	// +required
	Interface string `json:"interface"`
}

// ProblemStatusはProblemの現在の状態を表します。
type ProblemStatus struct {
	// コントローラーが確認した状態を表すフィールドをここに追加します。
	// 型を変更したらmake manifestsとmake generateを実行してください。

	// Conditionsは、Problemの現在の状態を表します。
	// 条件の種類ごとに状態を分け、True、False、Unknownで結果を示します。
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// VMRefは、管理する作問VMの名前とUIDを記録します。
	// +optional
	VMRef *ProblemVMReference `json:"vmRef,omitempty"`

	// DiskRefは、初回取り込みを管理するDataVolumeの名前とUIDを記録します。
	// +optional
	DiskRef *ProblemDiskReference `json:"diskRef,omitempty"`

	// ActiveChallengeは、ロックを所有するChallengeの情報を記録します。
	// +optional
	ActiveChallenge *ActiveChallengeStatus `json:"activeChallenge,omitempty"`
}

type ProblemVMReference struct {
	// Nameは、管理する作問VMの名前を指定します。
	// +kubebuilder:validation:MinLength=1
	// +required
	VMName string `json:"vmName"`

	// UIDは、管理する作問VMのUIDを指定します。
	// +required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	UID types.UID `json:"uid"`
}

type ProblemDiskReference struct {
	// Nameは、初回取り込みを管理するDataVolumeの名前を指定します。
	// +kubebuilder:validation:MinLength=1
	// +required
	DiskName string `json:"diskName"`

	// UIDは、初回取り込みを管理するDataVolumeのUIDを指定します。
	// +required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	UID types.UID `json:"uid"`
}
type ActiveChallengeStatus struct {
	// ActiveChallengeNameは、ロックを所有するChallengeの情報を示します。
	// +kubebuilder:validation:MinLength=1
	// +required
	ActiveChallengeName string `json:"activeChallengeName"`

	// UIDは、現在アクティブなChallengeのUIDを指定します。
	// +required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	UID types.UID `json:"uid"`

	// ProblemGenerationは、ロック取得時のProblemの世代を指定します。
	// +required
	ProblemGeneration int64 `json:"problemGeneration"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.activeChallenge) || self.spec == oldSelf.spec",message="Problem spec cannot change while locked by a Challenge"

// Problemは問題環境を表すリソースです。
type Problem struct {
	metav1.TypeMeta `json:",inline"`

	// metadataには、名前やNamespaceなどの情報が入ります。
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// specには、作成する問題環境の内容を指定します。
	// +required
	Spec ProblemSpec `json:"spec"`

	// statusには、コントローラーが確認した現在の状態が入ります。
	// +optional
	Status ProblemStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ProblemListはProblemの一覧です。
type ProblemList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Problem `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Problem{}, &ProblemList{})
		return nil
	})
}
