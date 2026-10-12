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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clonev1 "kubevirt.io/api/clone/v1beta1"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"
)

const (
	challengeCleanupFinalizer        = "lab.icttoracon.net/challenge-cleanup"
	challengeUIDLabel                = "lab.icttoracon.net/challenge-uid"
	challengeTeamLabel               = "lab.icttoracon.net/team-key"
	challengeSnapshotAnnotation      = "lab.icttoracon.net/source-snapshot-uid"
	challengeStartAttemptAnnotation  = "lab.icttoracon.net/start-attempted"
	challengeStartAcceptedAnnotation = "lab.icttoracon.net/start-accepted"
	challengeVMIAnnotation           = "lab.icttoracon.net/initial-vmi-uid"
	challengeProblemIndex            = "spec.problemRef.name"
)

// UIDとチームIDから名前を固定する。チームIDの文字種や長さをKubernetes名の制限へ持ち込まない。
func challengeSnapshotName(c *labv1alpha1.ProblemChallenge) string {
	hash := sha256.Sum256([]byte(c.UID))
	return fmt.Sprintf("challenge-%x-snapshot", hash[:10])
}

// KubeVirt v1.9.0のSnapshotContent名はSnapshotのUIDから決まる。ContentにはownerReferenceが付かない。
func challengeSnapshotContentName(uid types.UID) string { return "vmsnapshot-content-" + string(uid) }

func challengeTeamNames(c *labv1alpha1.ProblemChallenge, id string) (string, string, string) {
	challengeHash := sha256.Sum256([]byte(c.UID))
	teamHash := sha256.Sum256([]byte(id))
	prefix := fmt.Sprintf("challenge-%x-team-%x", challengeHash[:10], teamHash[:6])
	return prefix + "-clone", prefix + "-vm", prefix + "-vm-root"
}

func challengeLabels(c *labv1alpha1.ProblemChallenge, id string) map[string]string {
	labels := map[string]string{challengeUIDLabel: string(c.UID)}
	if id != "" {
		hash := sha256.Sum256([]byte(id))
		labels[challengeTeamLabel] = fmt.Sprintf("%x", hash[:6])
	}
	return labels
}

func challengeOwner(c *labv1alpha1.ProblemChallenge) metav1.OwnerReference {
	return *metav1.NewControllerRef(c, labv1alpha1.GroupVersion.WithKind("ProblemChallenge"))
}

// SnapshotはChallengeにつき1つだけ作る。失敗後に別時点のディスクで保存し直さない。
func challengeSnapshot(c *labv1alpha1.ProblemChallenge, vmName string) *snapshotv1.VirtualMachineSnapshot {
	group := "kubevirt.io"
	policy := snapshotv1.VirtualMachineSnapshotContentDelete
	return &snapshotv1.VirtualMachineSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: challengeSnapshotName(c), Namespace: c.Namespace, Labels: challengeLabels(c, ""), OwnerReferences: []metav1.OwnerReference{challengeOwner(c)}},
		Spec:       snapshotv1.VirtualMachineSnapshotSpec{Source: corev1.TypedLocalObjectReference{APIGroup: &group, Kind: "VirtualMachine", Name: vmName}, DeletionPolicy: &policy, FailureDeadline: &metav1.Duration{Duration: 5 * time.Minute}},
	}
}

// 全チームで同じSnapshotを使う。元Problemのowner/ラベルを引き継がず、チーム用の配置へ置き換える。
// 復元中にVMを起動させないためManualを指定し、PVCの確認後に初回のstart要求を別途送る。
func challengeClone(c *labv1alpha1.ProblemChallenge, id string) (*clonev1.VirtualMachineClone, error) {
	name, vmName, _ := challengeTeamNames(c, id)
	sourceGroup, targetGroup := "snapshot.kubevirt.io", "kubevirt.io"
	policy := clonev1.VolumeNamePolicyPrefixTargetName
	object := &clonev1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: challengeLabels(c, id), OwnerReferences: []metav1.OwnerReference{challengeOwner(c)}},
		Spec: clonev1.VirtualMachineCloneSpec{
			Source:       &corev1.TypedLocalObjectReference{APIGroup: &sourceGroup, Kind: "VirtualMachineSnapshot", Name: c.Status.SnapshotRef.Name},
			Target:       &corev1.TypedLocalObjectReference{APIGroup: &targetGroup, Kind: "VirtualMachine", Name: vmName},
			LabelFilters: []string{"!*"}, AnnotationFilters: []string{"!*"},
			Template:         clonev1.VirtualMachineCloneTemplateFilters{LabelFilters: []string{"!*"}, AnnotationFilters: []string{"!*"}},
			VolumeNamePolicy: &policy,
		},
	}
	// v1.9.0のRestore webhookは/spec以下の単純なパッチだけを受け付ける。
	// metadataと配置は、復元元を確認した後に通常のVM更新で設定する。
	patches := []struct {
		path  string
		value any
	}{{"/spec/runStrategy", "Manual"}}
	for _, patch := range patches {
		data, err := json.Marshal(map[string]any{"op": "add", "path": patch.path, "value": patch.value})
		if err != nil {
			return nil, err
		}
		object.Spec.Patches = append(object.Spec.Patches, string(data))
	}
	return object, nil
}

// 名前だけでは削除対象を判定せず、APIの所有参照と保存したUIDを使う。
func hasOwner(object metav1.Object, kind string, uid types.UID) bool {
	for _, owner := range object.GetOwnerReferences() {
		if owner.Kind == kind && owner.UID == uid {
			return true
		}
	}
	return false
}

func ownedByChallenge(object metav1.Object, c *labv1alpha1.ProblemChallenge) bool {
	return hasOwner(object, "ProblemChallenge", c.UID)
}
