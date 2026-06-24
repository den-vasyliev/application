// Copyright 2020 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Constants defining labels
const (
	StatusReady      = "Ready"
	StatusInProgress = "InProgress"
	StatusUnknown    = "Unknown"
	StatusDisabled   = "Disabled"
)

func status(u *unstructured.Unstructured) (string, error) {
	gk := u.GroupVersionKind().GroupKind()
	switch gk.String() {
	case "StatefulSet.apps":
		return stsStatus(u)
	case "Deployment.apps":
		return deploymentStatus(u)
	case "ReplicaSet.apps":
		return replicasetStatus(u)
	case "DaemonSet.apps":
		return daemonsetStatus(u)
	case "PersistentVolumeClaim":
		return pvcStatus(u)
	case "Service":
		return serviceStatus(u)
	case "Pod":
		return podStatus(u)
	case "PodDisruptionBudget.policy":
		return pdbStatus(u)
	case "ReplicationController":
		return replicationControllerStatus(u)
	case "Job.batch":
		return jobStatus(u)
	case "CronJob.batch":
		return cronJobStatus(u)
	case "Rollout.argoproj.io":
		return rolloutStatus(u)
	default:
		return statusFromStandardConditions(u)
	}
}

// Status from standard conditions
func statusFromStandardConditions(u *unstructured.Unstructured) (string, error) {
	condition := StatusReady

	// Check Ready condition
	_, cs, found, err := getConditionOfType(u, StatusReady)
	if err != nil {
		return StatusUnknown, err
	}
	if found && cs == corev1.ConditionFalse {
		condition = StatusInProgress
	}

	// Check InProgress condition
	_, cs, found, err = getConditionOfType(u, StatusInProgress)
	if err != nil {
		return StatusUnknown, err
	}
	if found && cs == corev1.ConditionTrue {
		condition = StatusInProgress
	}

	return condition, nil
}

// Statefulset
func stsStatus(u *unstructured.Unstructured) (string, error) {
	sts := &appsv1.StatefulSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, sts); err != nil {
		return StatusUnknown, err
	}

	desired := int32(1)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	scaledToZero := desired == 0 && sts.Status.Replicas == 0

	// Ready when every replica the controller is currently managing is ready, rather than
	// requiring ReadyReplicas == desired. During an HPA scale-up (e.g. 2->3) the new pod
	// takes its readiness-probe window to come up while the existing replicas keep serving;
	// gating on the full desired count would flap Ready->NotReady on every scale-up and page
	// monitoring with a false "degraded" incident. CurrentReplicas reflects what's actually
	// rolled, so ReadyReplicas == CurrentReplicas means "all running pods are healthy".
	if sts.Status.ObservedGeneration == sts.Generation &&
		(scaledToZero || (sts.Status.CurrentReplicas > 0 && sts.Status.ReadyReplicas == sts.Status.CurrentReplicas)) {
		return StatusReady, nil
	}
	return StatusInProgress, nil
}

// Deployment
func deploymentStatus(u *unstructured.Unstructured) (string, error) {
	deployment := &appsv1.Deployment{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, deployment); err != nil {
		return StatusUnknown, err
	}

	replicaFailure := false
	available := false

	for _, condition := range deployment.Status.Conditions {
		switch condition.Type {
		case appsv1.DeploymentAvailable:
			if condition.Status == corev1.ConditionTrue {
				available = true
			}
		case appsv1.DeploymentReplicaFailure:
			if condition.Status == corev1.ConditionTrue {
				replicaFailure = true
			}
		}
	}

	scaledToZero := *deployment.Spec.Replicas == 0 && deployment.Status.Replicas == 0

	// Ready when Kubernetes reports the Deployment Available (>= minAvailable replicas
	// serving) and there's no ReplicaFailure. We deliberately do NOT require
	// ReadyReplicas == spec.replicas: during an HPA scale-up (e.g. 2->3) the spec jumps
	// immediately while the new replica takes its readiness-probe window (up to 90s) to
	// become ready. The existing replicas keep serving and Available stays True, so the
	// app is healthy — gating on full desired count would flap Ready->NotReady on every
	// scale-up and page monitoring with a false "degraded" incident.
	if deployment.Status.ObservedGeneration == deployment.Generation &&
		!replicaFailure &&
		(scaledToZero || available) {
		return StatusReady, nil
	}
	return StatusInProgress, nil
}

// Replicaset
func replicasetStatus(u *unstructured.Unstructured) (string, error) {
	rs := &appsv1.ReplicaSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, rs); err != nil {
		return StatusUnknown, err
	}

	replicaFailure := false
	for _, condition := range rs.Status.Conditions {
		if condition.Type == appsv1.ReplicaSetReplicaFailure && condition.Status == corev1.ConditionTrue {
			replicaFailure = true
		}
	}

	desired := int32(1)
	if rs.Spec.Replicas != nil {
		desired = *rs.Spec.Replicas
	}
	scaledToZero := desired == 0 && rs.Status.Replicas == 0

	// Ready when at least one replica is available (serving) and there's no ReplicaFailure,
	// rather than requiring AvailableReplicas == desired. A scale-up leaves the new replica
	// pending for its readiness-probe window while existing ones serve; gating on the full
	// desired count would flap Ready->NotReady on every scale-up and false-page monitoring.
	if rs.Status.ObservedGeneration == rs.Generation && !replicaFailure &&
		(scaledToZero || rs.Status.AvailableReplicas > 0) {
		return StatusReady, nil
	}
	return StatusInProgress, nil
}

// Daemonset
func daemonsetStatus(u *unstructured.Unstructured) (string, error) {
	ds := &appsv1.DaemonSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ds); err != nil {
		return StatusUnknown, err
	}

	// Ready when no scheduled pod is currently unavailable, rather than requiring
	// NumberReady == DesiredNumberScheduled exactly. When a node joins the cluster the new
	// daemon pod is briefly not-yet-ready; DesiredNumberScheduled jumps immediately while
	// NumberReady lags, which would flap the app to NotReady on every node scale-up. The DS
	// controller's own NumberUnavailable counter respects maxUnavailable, so == 0 means the
	// rollout/coverage is healthy. A DS that schedules onto zero nodes (e.g. nodeSelector
	// matches nothing) is also Ready.
	if ds.Status.ObservedGeneration == ds.Generation &&
		ds.Status.NumberUnavailable == 0 {
		return StatusReady, nil
	}
	return StatusInProgress, nil
}

// PVC
func pvcStatus(u *unstructured.Unstructured) (string, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, pvc); err != nil {
		return StatusUnknown, err
	}

	if pvc.Status.Phase == corev1.ClaimBound {
		return StatusReady, nil
	}
	return StatusInProgress, nil
}

// Service
func serviceStatus(u *unstructured.Unstructured) (string, error) {
	service := &corev1.Service{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, service); err != nil {
		return StatusUnknown, err
	}
	stype := service.Spec.Type

	if stype == corev1.ServiceTypeClusterIP || stype == corev1.ServiceTypeNodePort || stype == corev1.ServiceTypeExternalName ||
		stype == corev1.ServiceTypeLoadBalancer && !isEmpty(service.Spec.ClusterIP) &&
			len(service.Status.LoadBalancer.Ingress) > 0 && !hasEmptyIngressIP(service.Status.LoadBalancer.Ingress) {
		return StatusReady, nil
	}
	return StatusInProgress, nil
}

// Pod
func podStatus(u *unstructured.Unstructured) (string, error) {
	pod := &corev1.Pod{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, pod); err != nil {
		return StatusUnknown, err
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && (condition.Reason == "PodCompleted" || condition.Status == corev1.ConditionTrue) {
			return StatusReady, nil
		}
	}
	return StatusInProgress, nil
}

// PodDisruptionBudget
func pdbStatus(u *unstructured.Unstructured) (string, error) {
	pdb := &policyv1.PodDisruptionBudget{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, pdb); err != nil {
		return StatusUnknown, err
	}

	if pdb.Status.ObservedGeneration == pdb.Generation &&
		pdb.Status.CurrentHealthy >= pdb.Status.DesiredHealthy {
		return StatusReady, nil
	}
	return StatusInProgress, nil
}

func replicationControllerStatus(u *unstructured.Unstructured) (string, error) {
	rc := &corev1.ReplicationController{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, rc); err != nil {
		return StatusUnknown, err
	}

	desired := int32(1)
	if rc.Spec.Replicas != nil {
		desired = *rc.Spec.Replicas
	}
	scaledToZero := desired == 0 && rc.Status.Replicas == 0

	// Ready when at least one replica is available (serving), rather than requiring
	// AvailableReplicas == desired — see replicasetStatus: a scale-up must not flap the app
	// to NotReady while the new replica comes up and existing ones keep serving.
	if rc.Status.ObservedGeneration == rc.Generation &&
		(scaledToZero || rc.Status.AvailableReplicas > 0) {
		return StatusReady, nil
	}
	return StatusInProgress, nil
}

func jobStatus(u *unstructured.Unstructured) (string, error) {
	job := &batchv1.Job{}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, job); err != nil {
		return StatusUnknown, err
	}

	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return StatusReady, nil
		case batchv1.JobFailed:
			return StatusInProgress, nil
		}
	}

	return StatusInProgress, nil
}

func cronJobStatus(u *unstructured.Unstructured) (string, error) {
	cj := &batchv1.CronJob{}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, cj); err != nil {
		return StatusUnknown, err
	}

	// Only InProgress when explicitly suspended; schedule/success history doesn't reflect app health.
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		return StatusInProgress, nil
	}

	return StatusReady, nil
}

// Argo Rollout — uses status.phase rather than standard conditions
func rolloutStatus(u *unstructured.Unstructured) (string, error) {
	phase, found, err := unstructured.NestedString(u.Object, "status", "phase")
	if err != nil {
		return StatusUnknown, err
	}
	if !found || phase == "" {
		return StatusInProgress, nil
	}
	switch phase {
	case "Healthy", "Inactive":
		return StatusReady, nil
	case "Degraded", "Progressing", "Paused", "Error", "Unknown":
		return StatusInProgress, nil
	default:
		return StatusUnknown, nil
	}
}

func hasEmptyIngressIP(ingress []corev1.LoadBalancerIngress) bool {
	for _, i := range ingress {
		if isEmpty(i.IP) {
			return true
		}
	}
	return false
}

func isEmpty(s string) bool {
	return len(strings.TrimSpace(s)) == 0
}

func getConditionOfType(u *unstructured.Unstructured, conditionType string) (string, corev1.ConditionStatus, bool, error) {
	conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return "", corev1.ConditionFalse, false, err
	}

	for _, c := range conditions {
		condition, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, found := condition["type"]
		if !found {
			continue
		}
		condType, ok := t.(string)
		if !ok {
			continue
		}
		if condType == conditionType {
			reason := condition["reason"].(string)
			conditionStatus := condition["status"].(string)
			return reason, corev1.ConditionStatus(conditionStatus), true, nil
		}
	}
	return "", corev1.ConditionFalse, false, nil
}
