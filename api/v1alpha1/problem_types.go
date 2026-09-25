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
)

// Problem APIの型を定義します。
// フィールドを追加するときはJSONタグも指定してください。

// ProblemSpecはProblemに指定する内容を表します。
type ProblemSpec struct {
	// 作問者が指定するフィールドをここに追加します。
	// 型を変更したらmake manifestsとmake generateを実行してください。
	// Kubebuilderのマーカーを使うと入力値を検証できます。

	// Source
	// 問題を定義するリポジトリ/ディレクトリの情報を指定する
	// +required
	Source ProblemSourceSpec `json:"source"`

	// Machine
	// マシンスペック
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

type ProblemMachineSpec struct {
	// CPUはマシンのCPUコア数を指定する
	// +kubebuilder:validation:Minimum=1
	// +required
	CPU int32 `json:"cpu"`

	// Memoryはマシンのメモリ容量を指定する
	// +required
	Memory resource.Quantity `json:"memory"`

	// Diskはマシンのディスク容量を指定する
	// +required
	Disk resource.Quantity `json:"disk"`
}

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
	// Controllerが確認した状態を表すフィールドをここに追加します。
	// 型を変更したらmake manifestsとmake generateを実行してください。

	// ConditionsはProblemの状態を表します。
	// typeごとに状態を分け、True、False、Unknownで結果を示します。
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Problemは問題環境を表すリソースです。
type Problem struct {
	metav1.TypeMeta `json:",inline"`

	// metadataには名前やnamespaceなどの情報が入ります。
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// specには作りたい問題環境の内容を指定します。
	// +required
	Spec ProblemSpec `json:"spec"`

	// statusにはControllerが確認した現在の状態が入ります。
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
