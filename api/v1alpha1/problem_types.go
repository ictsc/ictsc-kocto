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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// Problem APIで使う型を定義します。
// フィールドを追加するときはJSONタグも指定してください。

// ProblemSpecはProblemに指定する内容を表します。
// +kubebuilder:validation:XValidation:rule="has(self.storage) == has(oldSelf.storage)",message="storageはProblemの作成後に追加や削除ができません"
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

	// Storageは初回取り込み先を指定する。省略時はmanagerのStorageClassとFilesystemを使う。
	// 既存Problemへの後付けや削除は禁止し、作問済みのディスクを別の構成へ切り替えない。
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="storageは変更できません"
	// +optional
	Storage *ProblemStorageSpec `json:"storage,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	// +required
	Access []AccessEndpoint `json:"access"`
}

// ProblemStorageSpecは作問ディスクの保存先と接続方法を表す。アクセスモードはRWOを使う。
type ProblemStorageSpec struct {
	// StorageClassNameは初回取り込み先のStorageClass名を指定する。
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
	// +required
	StorageClassName string `json:"storageClassName"`

	// VolumeModeはFilesystemまたはBlockを指定する。作成後に変換しない。
	// +kubebuilder:validation:Enum=Filesystem;Block
	// +required
	VolumeMode corev1.PersistentVolumeMode `json:"volumeMode"`
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
// Memoryか、有効なMemoryBalloonの範囲指定が必須。両方の省略や有効時の二重指定は拒否する。
// 各フィールドのoptionalは単独で省略できるという意味で、以下のCELが組み合わせを検証する。
// MemoryBalloonを無効にする場合は固定量のMemoryが必要になる。
// +kubebuilder:validation:XValidation:rule="has(self.memoryBalloon) && self.memoryBalloon.enabled ? !has(self.memory) : has(self.memory)",message="memoryBalloonが有効な場合はmemoryを省略し、それ以外はmemoryが必須です"
type ProblemMachineSpec struct {
	// CPUはマシンのCPUコア数を指定する
	// +kubebuilder:validation:Minimum=1
	// +required
	CPU int32 `json:"cpu"`

	// Memoryは固定のメモリ量を指定する。balloonを明示的に有効にする場合は省略する。
	// 省略したmemoryBalloonは従来どおりゲスト量と要求量を揃え、デバイスの既定値も維持する。
	// +kubebuilder:validation:XValidation:rule="isQuantity(string(self)) && quantity(string(self)).isGreaterThan(quantity('0'))",message="machine.memoryは有効なQuantityで0より大きい値にしてください"
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`

	// MemoryBalloonはvirtio-balloonの有効/無効と、ゲスト量と配置要求量を指定する。
	// +optional
	MemoryBalloon *ProblemMemoryBalloonSpec `json:"memoryBalloon,omitempty"`

	// Diskは初回取り込み時のroot PVCの要求容量を指定する。
	// 作成後は変更できない。
	// +kubebuilder:validation:XValidation:rule="isQuantity(string(self)) && quantity(string(self)).isGreaterThan(quantity('0'))",message="machine.diskは有効なQuantityで0より大きい値にしてください"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machine.diskは変更できません"
	// +required
	Disk resource.Quantity `json:"disk"`

	// Imageは、初回取り込み元のイメージを指定する。
	// 作成後は変更できない。
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="machine.imageは変更できません"
	// +required
	Image ProblemMachineImageSpec `json:"image"`
}

// ProblemMemoryBalloonSpecのMinは配置要求量であり、実行中のballoonの下限ではない。
// 有効時は範囲を必須にし、無効時はmachine.memoryだけで固定量を指定する。
// +kubebuilder:validation:XValidation:rule="self.enabled ? has(self.min) && has(self.max) : !has(self.min) && !has(self.max)",message="memoryBalloonが有効な場合はminとmaxが必須で、無効な場合は指定できません"
// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.max) || !isQuantity(string(self.min)) || !isQuantity(string(self.max)) || !quantity(string(self.min)).isGreaterThan(quantity(string(self.max)))",message="memoryBalloon.minはmax以下にしてください"
type ProblemMemoryBalloonSpec struct {
	// Enabledはvirtio-balloonデバイスを接続するかを指定する。
	// +required
	Enabled bool `json:"enabled"`

	// MinはKubernetesへ要求するメモリ量。KubeVirtの追加分は別に加算される。
	// +kubebuilder:validation:XValidation:rule="isQuantity(string(self)) && quantity(string(self)).isGreaterThan(quantity('0'))",message="memoryBalloon.minは有効なQuantityで0より大きい値にしてください"
	// +optional
	Min *resource.Quantity `json:"min,omitempty"`

	// Maxはゲストに見せるメモリ量。memory.maxGuestによるhotplug上限ではない。
	// +kubebuilder:validation:XValidation:rule="isQuantity(string(self)) && quantity(string(self)).isGreaterThan(quantity('0'))",message="memoryBalloon.maxは有効なQuantityで0より大きい値にしてください"
	// +optional
	Max *resource.Quantity `json:"max,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.activeChallenge) || self.spec == oldSelf.spec",message="ProblemのspecはChallengeによるロック中は変更できません"

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
