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

package utils

import (
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// ExternalTestCRDsはcontrollerが読み書きする外部APIだけをテストへ用意する。
// spec/statusを保持し、KubernetesのUID/resourceVersion/statusの動作を検証する。
// 外部operatorの入力検証やVMの起動を再現するための定義ではない。
func ExternalTestCRDs() []*apiextensionsv1.CustomResourceDefinition {
	definitions := []struct {
		group, version, kind, plural string
		scope                        apiextensionsv1.ResourceScope
	}{
		{"kubevirt.io", "v1", "VirtualMachine", "virtualmachines", apiextensionsv1.NamespaceScoped},
		{"kubevirt.io", "v1", "VirtualMachineInstance", "virtualmachineinstances", apiextensionsv1.NamespaceScoped},
		{"cdi.kubevirt.io", "v1beta1", "DataVolume", "datavolumes", apiextensionsv1.NamespaceScoped},
		{"clone.kubevirt.io", "v1beta1", "VirtualMachineClone", "virtualmachineclones", apiextensionsv1.NamespaceScoped},
		{"snapshot.kubevirt.io", "v1beta1", "VirtualMachineSnapshot", "virtualmachinesnapshots", apiextensionsv1.NamespaceScoped},
		{"snapshot.kubevirt.io", "v1beta1", "VirtualMachineSnapshotContent", "virtualmachinesnapshotcontents", apiextensionsv1.NamespaceScoped},
		{"snapshot.kubevirt.io", "v1beta1", "VirtualMachineRestore", "virtualmachinerestores", apiextensionsv1.NamespaceScoped},
		{"snapshot.storage.k8s.io", "v1", "VolumeSnapshot", "volumesnapshots", apiextensionsv1.NamespaceScoped},
		{"snapshot.storage.k8s.io", "v1", "VolumeSnapshotContent", "volumesnapshotcontents", apiextensionsv1.ClusterScoped},
		{"snapshot.storage.k8s.io", "v1", "VolumeSnapshotClass", "volumesnapshotclasses", apiextensionsv1.ClusterScoped},
	}
	crds := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(definitions))
	for _, d := range definitions {
		crds = append(crds, &apiextensionsv1.CustomResourceDefinition{
			TypeMeta: metav1.TypeMeta{APIVersion: apiextensionsv1.SchemeGroupVersion.String(), Kind: "CustomResourceDefinition"},
			ObjectMeta: metav1.ObjectMeta{Name: d.plural + "." + d.group,
				Annotations: map[string]string{"api-approved.kubernetes.io": "unapproved, controller test API"}},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: d.group, Scope: d.scope,
				Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: d.kind, ListKind: d.kind + "List", Plural: d.plural, Singular: strings.ToLower(d.kind)},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name: d.version, Served: true, Storage: true,
					Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
					Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object", XPreserveUnknownFields: ptr.To(true), Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec":   {Type: "object", XPreserveUnknownFields: ptr.To(true)},
							"status": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
						},
					}},
				}},
			},
		})
	}
	return crds
}
