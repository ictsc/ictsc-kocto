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
	"fmt"
	"slices"
	"time"

	labv1alpha1 "github.com/ictsc/ictsc-kocto/api/v1alpha1"
	"github.com/ictsc/ictsc-kocto/internal/bundle"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	virtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const bundleSourceAnnotation = "lab.icttoracon.net/bundle-source"

func problemBundleName(p *labv1alpha1.Problem) string {
	_, root := problemResourceNames(p)
	return root[:len(root)-len("root")] + "bundle"
}

func problemBundlePVC(p *labv1alpha1.Problem) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: problemBundleName(p), Namespace: p.Namespace, Labels: problemLabels(p), Annotations: map[string]string{bundleSourceAnnotation: p.Status.BundleRef.SourceHash}},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To(p.Status.BundleRef.StorageClassName), VolumeMode: ptr.To(corev1.PersistentVolumeFilesystem), AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("8Gi")}}},
	}
}

func problemBundleJob(p *labv1alpha1.Problem) *batchv1.Job {
	source, ref := p.Spec.Source, p.Status.BundleRef
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: problemBundleName(p), Namespace: p.Namespace, Labels: problemLabels(p), Annotations: map[string]string{bundleSourceAnnotation: ref.SourceHash}},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(2)), ActiveDeadlineSeconds: ptr.To(int64(1800)),
			// Keep the completed Job as durable build evidence. Never TTL/recreate it.
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: problemLabels(p)}, Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
				SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(107)), RunAsGroup: ptr.To(int64(107)), FSGroup: ptr.To(int64(107)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{{Name: "builder", Image: ref.BuilderImage, ImagePullPolicy: corev1.PullIfNotPresent,
					Command: []string{"/usr/local/bin/bundle-builder"}, Args: []string{"--repository=" + source.Repository, "--commit=" + source.CommitHash, "--directory=" + source.Directory, "--topology=" + source.TopologyFile},
					SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("20Gi")}},
					VolumeMounts:    []corev1.VolumeMount{{Name: "bundle", MountPath: "/disk"}, {Name: "work", MountPath: "/tmp"}, {Name: "work", MountPath: "/var/tmp"}},
				}},
				Volumes: []corev1.Volume{{Name: "bundle", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: problemBundleName(p)}}}, {Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("20Gi"))}}}},
			}},
		},
	}
	if ref.ImagePullSecret != "" {
		job.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: ref.ImagePullSecret}}
	}
	return job

}

// Compare complete VolumeSource values: a claim name alone must not authorize
// DataVolume/Secret/hostDisk sources, extra volumes, or writable bundle disks.
func problemVolumesMatch(p *labv1alpha1.Problem, vm *virtv1.VirtualMachine) bool {
	if vm.Spec.Template == nil {
		return false
	}
	expected := problemVirtualMachine(p).Spec.Template.Spec
	actual := vm.Spec.Template.Spec
	if !equality.Semantic.DeepEqual(actual.Volumes, expected.Volumes) {
		return false
	}
	// KubeVirt can default disk buses; for bundle Problems require the explicit
	// read-only bundle target, and no additional disks or filesystem devices.
	if p.Status.BundleRef != nil {
		if len(actual.Domain.Devices.Filesystems) != 0 || len(actual.Domain.Devices.Disks) != 2 {
			return false
		}
		for _, disk := range expected.Domain.Devices.Disks {
			index := slices.IndexFunc(actual.Domain.Devices.Disks, func(d virtv1.Disk) bool { return d.Name == disk.Name })
			if index < 0 || !equality.Semantic.DeepEqual(actual.Domain.Devices.Disks[index].DiskDevice, disk.DiskDevice) {
				return false
			}
		}
	}
	return true
}

func bundleJobMatches(p *labv1alpha1.Problem, job *batchv1.Job) bool {
	expected := problemBundleJob(p)
	if ptr.Deref(job.Spec.Parallelism, 1) != 1 || ptr.Deref(job.Spec.Completions, 1) != 1 || ptr.Deref(job.Spec.Suspend, false) || job.Spec.TTLSecondsAfterFinished != nil {
		return false
	}
	a, b := job.Spec.Template.Spec, expected.Spec.Template.Spec
	if len(a.Containers) != 1 || len(a.InitContainers) != 0 || a.HostNetwork || a.HostPID || a.HostIPC || a.ServiceAccountName != "" && a.ServiceAccountName != "default" {
		return false
	}
	ca, cb := a.Containers[0], b.Containers[0]
	return equality.Semantic.DeepEqual(a.ImagePullSecrets, b.ImagePullSecrets) && equality.Semantic.DeepEqual(a.Volumes, b.Volumes) && equality.Semantic.DeepEqual(a.SecurityContext, b.SecurityContext) &&
		equality.Semantic.DeepEqual(a.AutomountServiceAccountToken, b.AutomountServiceAccountToken) && a.RestartPolicy == b.RestartPolicy &&
		ca.Image == cb.Image && slices.Equal(ca.Command, cb.Command) && slices.Equal(ca.Args, cb.Args) && len(ca.Env) == 0 && len(ca.EnvFrom) == 0 &&
		equality.Semantic.DeepEqual(ca.Resources, cb.Resources) && equality.Semantic.DeepEqual(ca.VolumeMounts, cb.VolumeMounts) && equality.Semantic.DeepEqual(ca.SecurityContext, cb.SecurityContext) &&
		equality.Semantic.DeepEqual(job.Spec.BackoffLimit, expected.Spec.BackoffLimit) && equality.Semantic.DeepEqual(job.Spec.ActiveDeadlineSeconds, expected.Spec.ActiveDeadlineSeconds)
}

// Save the build reference before creating children; source updates and races
// with Challenge locks use the same resourceVersion protection as VM status.
func (r *ProblemReconciler) saveBundle(ctx context.Context, observed *labv1alpha1.Problem, ref *labv1alpha1.ProblemBundleReference) error {
	current := &labv1alpha1.Problem{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(observed), current); err != nil {
		return err
	}
	if current.UID != observed.UID || current.Generation != observed.Generation || !current.DeletionTimestamp.IsZero() || current.Status.ActiveChallenge != nil {
		return apierrors.NewConflict(labv1alpha1.GroupVersion.WithResource("problems").GroupResource(), observed.Name, fmt.Errorf("problem changed during bundle reconciliation"))
	}
	if !equality.Semantic.DeepEqual(current.Status.BundleRef, observed.Status.BundleRef) {
		return apierrors.NewConflict(labv1alpha1.GroupVersion.WithResource("problems").GroupResource(), observed.Name, fmt.Errorf("bundle reference changed"))
	}
	current.Status.BundleRef = ref
	apiMeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "BundleReady", Status: metav1.ConditionFalse, Reason: "BundleBuildPending", Message: "Waiting for the offline bundle builder", ObservedGeneration: current.Generation})
	if ref.Ready {
		apiMeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "BundleReady", Status: metav1.ConditionTrue, Reason: "BundleBuilt", Message: "Bundle disk is ready; guest deployment is not observed", ObservedGeneration: current.Generation})
	}
	return r.Status().Update(ctx, current)
}

func (r *ProblemReconciler) ensureProblemBundle(ctx context.Context, p *labv1alpha1.Problem, vmExists bool) (bool, ctrl.Result, error) {
	state := func(reason, message string) (bool, ctrl.Result, error) {
		result, err := r.problemState(ctx, p, reason, message, false, false, nil, nil)
		return false, result, err
	}
	if p.Status.BundleRef == nil {
		// Existing disks are never migrated, even if a previous manager crashed
		// before recording their UID. Preserve the old root-only lifecycle.
		if r.BundleBuilderImage == "" || p.Status.DiskRef != nil || p.Status.VMRef != nil || vmExists {
			return true, ctrl.Result{}, nil
		}
		_, name := problemResourceNames(p)
		for _, object := range []client.Object{&cdiv1.DataVolume{}, &corev1.PersistentVolumeClaim{}} {
			err := r.reader().Get(ctx, types.NamespacedName{Name: name, Namespace: p.Namespace}, object)
			if err == nil {
				return true, ctrl.Result{}, nil
			}
			if !apierrors.IsNotFound(err) {
				return false, ctrl.Result{}, err
			}
		}
		if err := bundle.Validate(p.Spec.Source); err != nil {
			return state("InvalidBundleSource", err.Error())
		}
		if p.Spec.Machine.Disk.Cmp(resource.MustParse("14Gi")) < 0 {
			return state("BundleDiskTooSmall", "Flatcar requires machine.disk of at least 14Gi")
		}
		storage := r.StorageClass
		if p.Spec.Storage != nil {
			storage = p.Spec.Storage.StorageClassName
		}
		if storage == "" {
			return state("StorageClassRequired", "Specify storage before provisioning the bundle")
		}
		ref := &labv1alpha1.ProblemBundleReference{SourceHash: bundle.SourceHash(p.Spec.Source), BuilderImage: r.BundleBuilderImage, ImagePullSecret: r.BundleBuilderPullSecret, StorageClassName: storage}
		return false, ctrl.Result{Requeue: true}, r.saveBundle(ctx, p, ref)
	}
	ref := p.Status.BundleRef.DeepCopy()
	if ref.SourceHash != bundle.SourceHash(p.Spec.Source) {
		return state("BundleSourceChanged", "Bundle source updates are unsupported; preserve the author disks")
	}
	name := problemBundleName(p)
	key := types.NamespacedName{Name: name, Namespace: p.Namespace}
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.reader().Get(ctx, key, pvc)
	if apierrors.IsNotFound(err) {
		if ref.PVCUID != "" || ref.JobUID != "" || ref.Ready {
			return state("BundleMissing", "Recorded bundle PVC is missing; do not rebuild automatically")
		}
		pvc = problemBundlePVC(p)
		if err := controllerutil.SetControllerReference(p, pvc, r.Scheme); err != nil {
			return false, ctrl.Result{}, err
		}
		if err := r.Create(ctx, pvc); err != nil {
			return false, ctrl.Result{}, err
		}
	} else if err != nil {
		return false, ctrl.Result{}, err
	}
	expectedPVC := problemBundlePVC(p)
	if !ownedByProblem(pvc, p) || pvc.Annotations[bundleSourceAnnotation] != ref.SourceHash || ref.PVCUID != "" && ref.PVCUID != pvc.UID || !pvc.DeletionTimestamp.IsZero() || !equality.Semantic.DeepEqual(pvc.Spec.StorageClassName, expectedPVC.Spec.StorageClassName) || pvcVolumeMode(pvc.Spec) != corev1.PersistentVolumeFilesystem || !slices.Equal(pvc.Spec.AccessModes, expectedPVC.Spec.AccessModes) || pvc.Spec.Resources.Requests.Storage().Cmp(*expectedPVC.Spec.Resources.Requests.Storage()) != 0 {
		return state("ResourceConflict", "Bundle PVC identity or storage does not match the initial build")
	}
	if ref.PVCUID == "" {
		ref.PVCUID = pvc.UID
		return false, ctrl.Result{Requeue: true}, r.saveBundle(ctx, p, ref)
	}
	job := &batchv1.Job{}
	err = r.reader().Get(ctx, key, job)
	if apierrors.IsNotFound(err) {
		if ref.JobUID != "" || ref.Ready {
			return state("BundleJobMissing", "Recorded bundle Job is missing; do not rebuild automatically")
		}
		job = problemBundleJob(p)
		if err := controllerutil.SetControllerReference(p, job, r.Scheme); err != nil {
			return false, ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil {
			return false, ctrl.Result{}, err
		}
	} else if err != nil {
		return false, ctrl.Result{}, err
	}
	if !ownedByProblem(job, p) || job.Annotations[bundleSourceAnnotation] != ref.SourceHash || ref.JobUID != "" && ref.JobUID != job.UID || !job.DeletionTimestamp.IsZero() || !bundleJobMatches(p, job) {
		return state("ResourceConflict", "Bundle Job identity or build inputs do not match")
	}
	if ref.JobUID == "" {
		ref.JobUID = job.UID
		return false, ctrl.Result{Requeue: true}, r.saveBundle(ctx, p, ref)
	}
	complete := false
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return state("BundleBuildFailed", "Bundle Job failed after bounded retries; inspect Job/Pod logs")
		}
		complete = complete || c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue
	}
	if !complete || pvc.Status.Phase != corev1.ClaimBound {
		return state("BundleBuildPending", "Waiting for the builder Job and a bound bundle PVC")
	}
	pods := &corev1.PodList{}
	if err := r.reader().List(ctx, pods, client.InNamespace(p.Namespace), client.MatchingLabels{"batch.kubernetes.io/controller-uid": string(job.UID)}); err != nil {
		return false, ctrl.Result{}, err
	}
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.UID != job.UID || owner.Kind != "Job" || owner.APIVersion != batchv1.SchemeGroupVersion.String() {
			return state("ResourceConflict", "Bundle writer Pod does not belong to the recorded Job")
		}
		if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			continue
		}
		// A completed Job does not recreate its Pods. Delete terminal writer Pods
		// and observe their disappearance before handing the RWO volume to a VM.
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, &pod, client.Preconditions{UID: &pod.UID}); err != nil {
				return false, ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
	}
	if len(pods.Items) != 0 {
		result, err := r.problemState(ctx, p, "BundleWriterStopping", "Waiting for bundle writer Pod removal before using the PVC", false, false, nil, nil)
		result.RequeueAfter = 5 * time.Second
		return false, result, err
	}
	if !ref.Ready {
		ref.Ready = true
		return false, ctrl.Result{Requeue: true}, r.saveBundle(ctx, p, ref)
	}
	return true, ctrl.Result{}, nil
}

// Called only after VM/VMI/launcher and root storage cleanup. Stop the writer
// before deleting its PVC, using owner and recorded UID on every delete.
func (r *ProblemReconciler) cleanupProblemBundle(ctx context.Context, p *labv1alpha1.Problem) (bool, error) {
	ref := p.Status.BundleRef
	if ref == nil {
		return true, nil
	}
	key := types.NamespacedName{Name: problemBundleName(p), Namespace: p.Namespace}
	job := &batchv1.Job{}
	err := r.reader().Get(ctx, key, job)
	if err == nil {
		if !ownedByProblem(job, p) || ref.JobUID != "" && ref.JobUID != job.UID {
			return false, fmt.Errorf("bundle Job ownership or UID changed; refusing deletion")
		}
		return false, client.IgnoreNotFound(r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &job.UID}))
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := r.reader().List(ctx, pods, client.InNamespace(p.Namespace), client.MatchingLabels{problemUIDLabel: string(p.UID)}); err != nil {
		return false, err
	}
	if len(pods.Items) != 0 {
		return false, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err = r.reader().Get(ctx, key, pvc)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !ownedByProblem(pvc, p) || ref.PVCUID != "" && ref.PVCUID != pvc.UID {
		return false, fmt.Errorf("bundle PVC ownership or UID changed; refusing deletion")
	}
	return false, client.IgnoreNotFound(r.Delete(ctx, pvc, client.Preconditions{UID: &pvc.UID}))
}
