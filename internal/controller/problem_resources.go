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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
)

const (
	problemUIDLabel         = "lab.icttoracon.net/problem-uid"
	problemCleanupFinalizer = "lab.icttoracon.net/problem-cleanup"
)

func problemResourceNames(problem *labv1alpha1.Problem) (string, string) {
	hash := sha256.Sum256([]byte(problem.UID))
	prefix := fmt.Sprintf("problem-%x", hash[:10])
	return prefix + "-vm", prefix + "-root"
}

func problemLabels(problem *labv1alpha1.Problem) map[string]string {
	return map[string]string{problemUIDLabel: string(problem.UID)}
}

func problemDataVolume(problem *labv1alpha1.Problem, storageClass string) *cdiv1.DataVolume {
	_, name := problemResourceNames(problem)
	url := "docker://" + problem.Spec.Machine.Image.ContainerDisk
	mode := corev1.PersistentVolumeFilesystem
	return &cdiv1.DataVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: problem.Namespace, Labels: problemLabels(problem)},
		Spec: cdiv1.DataVolumeSpec{
			Source:      &cdiv1.DataVolumeSource{Registry: &cdiv1.DataVolumeSourceRegistry{URL: &url}},
			ContentType: cdiv1.DataVolumeKubeVirt,
			Storage: &cdiv1.StorageSpec{
				StorageClassName: &storageClass, VolumeMode: &mode,
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: problem.Spec.Machine.Disk.DeepCopy(),
				}},
			},
		},
	}
}

func problemVirtualMachine(problem *labv1alpha1.Problem) *virtv1.VirtualMachine {
	name, disk := problemResourceNames(problem)
	run := virtv1.RunStrategyAlways
	if problem.Status.ActiveChallenge != nil {
		run = virtv1.RunStrategyHalted
	}
	nic := false
	grace := int64(300)
	memory := problem.Spec.Machine.Memory.DeepCopy()
	return &virtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: problem.Namespace, Labels: problemLabels(problem)},
		Spec: virtv1.VirtualMachineSpec{
			RunStrategy: &run,
			Template: &virtv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: problemLabels(problem)},
				Spec: virtv1.VirtualMachineInstanceSpec{
					TerminationGracePeriodSeconds: &grace,
					Domain: virtv1.DomainSpec{
						CPU:       &virtv1.CPU{Cores: uint32(problem.Spec.Machine.CPU)},
						Memory:    &virtv1.Memory{Guest: &memory},
						Resources: virtv1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: memory}},
						Devices: virtv1.Devices{AutoattachPodInterface: &nic, Disks: []virtv1.Disk{{
							Name: "root", DiskDevice: virtv1.DiskDevice{Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio}},
						}}},
					},
					Volumes: []virtv1.Volume{{Name: "root", VolumeSource: virtv1.VolumeSource{
						PersistentVolumeClaim: &virtv1.PersistentVolumeClaimVolumeSource{PersistentVolumeClaimVolumeSource: corev1.PersistentVolumeClaimVolumeSource{ClaimName: disk}},
					}}},
				},
			},
		},
	}
}
