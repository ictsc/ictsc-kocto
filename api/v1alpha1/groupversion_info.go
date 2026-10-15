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

// Package v1alpha1はlabグループのv1alpha1 APIを定義します。
// +kubebuilder:object:generate=true
// +groupName=lab.icttoracon.net
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersionは、型の登録に使うグループとバージョンです。
	// applyconfigurationの生成処理もこの名前を参照します。
	SchemeGroupVersion = schema.GroupVersion{Group: "lab.icttoracon.net", Version: "v1alpha1"}

	// GroupVersionは互換性のために残すSchemeGroupVersionの別名です。
	GroupVersion = SchemeGroupVersion

	// SchemeBuilderはGoの型をGroupVersionKindのSchemeへ登録します。
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})

	// AddToSchemeは、このグループとバージョンの型を指定のSchemeへ登録します。
	AddToScheme = SchemeBuilder.AddToScheme
)
