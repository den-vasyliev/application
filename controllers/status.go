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
	case "Gateway.gateway.networking.k8s.io":
		return gatewayStatus(u)
	case "HTTPRoute.gateway.networking.k8s.io":
		return routeStatus(u)
	case "Agent.kagent.dev":
		return kagentStatus(u)
	case "ModelConfig.kagent.dev":
		return kagentStatus(u)
	default:
		return statusFromStandardConditions(u)
	}
}

// Status from standard conditions
func statusFromStandardConditions(u *unstructured.Unstructured) (string, error) {
	condition := StatusReady

	// Check Ready condition
	cs, found, err := getConditionOfType(u, StatusReady)
	if err != nil {
		return StatusUnknown, err
	}
	if found && cs == corev1.ConditionFalse {
		condition = StatusInProgress
	}

	// Check InProgress condition
	cs, found, err = getConditionOfType(u, StatusInProgress)
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
	//
	// We do NOT gate on ObservedGeneration == Generation: a scale-up bumps generation
	// immediately while the StatefulSet controller writes observedGeneration a moment later,
	// and gating on that skew would re-introduce the scale-up flap (see deploymentStatus).
	if scaledToZero || (sts.Status.CurrentReplicas > 0 && sts.Status.ReadyReplicas == sts.Status.CurrentReplicas) {
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

	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	scaledToZero := desired == 0 && deployment.Status.Replicas == 0

	// Ready when Kubernetes reports the Deployment Available (>= minAvailable replicas
	// serving) and there's no ReplicaFailure. We deliberately do NOT require
	// ReadyReplicas == spec.replicas: during an HPA scale-up (e.g. 2->3) the spec jumps
	// immediately while the new replica takes its readiness-probe window (up to 90s) to
	// become ready. The existing replicas keep serving and Available stays True, so the
	// app is healthy — gating on full desired count would flap Ready->NotReady on every
	// scale-up and page monitoring with a false "degraded" incident.
	//
	// We also do NOT gate Ready on ObservedGeneration == Generation. An HPA scale-up
	// changes spec.replicas, which bumps metadata.generation immediately; the Deployment
	// controller only writes status.observedGeneration a moment later. In that window
	// generation is ahead of observedGeneration while Available is still True — gating on
	// it would re-introduce the exact scale-up flap this fix exists to prevent. The
	// Available condition already reflects real serving capacity, so it is sufficient.
	if !replicaFailure && (scaledToZero || available) {
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
	//
	// We do NOT gate on ObservedGeneration == Generation: a scale-up bumps generation
	// immediately while observedGeneration lags a moment, and gating on that skew would
	// re-introduce the scale-up flap (see deploymentStatus).
	if !replicaFailure && (scaledToZero || rs.Status.AvailableReplicas > 0) {
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
	//
	// We do NOT gate on ObservedGeneration == Generation: a node join / spec change bumps
	// generation immediately while observedGeneration lags a moment, and gating on that
	// skew would re-introduce the scale-up flap (see deploymentStatus). NumberUnavailable
	// already respects maxUnavailable, so == 0 means coverage is healthy.
	if ds.Status.NumberUnavailable == 0 {
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

	// ClusterIP/NodePort/ExternalName have no external provisioning step — Ready as soon
	// as the object exists.
	if stype == corev1.ServiceTypeClusterIP || stype == corev1.ServiceTypeNodePort || stype == corev1.ServiceTypeExternalName {
		return StatusReady, nil
	}

	// LoadBalancer additionally requires the cloud provider to have provisioned the LB:
	// a ClusterIP assigned AND at least one ingress entry, all of them provisioned. An
	// ingress entry is provisioned when it carries either an IP (GCP/Azure/most CNI LBs)
	// or a Hostname (AWS ELB/NLB, which never populate IP) — requiring IP unconditionally
	// left hostname-only LoadBalancer Services stuck InProgress forever.
	if stype == corev1.ServiceTypeLoadBalancer {
		ingress := service.Status.LoadBalancer.Ingress
		if !isEmpty(service.Spec.ClusterIP) && len(ingress) > 0 && allIngressProvisioned(ingress) {
			return StatusReady, nil
		}
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
	//
	// We do NOT gate on ObservedGeneration == Generation: a scale-up bumps generation
	// immediately while observedGeneration lags a moment, and gating on that skew would
	// re-introduce the scale-up flap (see deploymentStatus).
	if scaledToZero || rc.Status.AvailableReplicas > 0 {
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

// Argo Rollout — uses status.phase rather than standard conditions.
func rolloutStatus(u *unstructured.Unstructured) (string, error) {
	// Scaled to zero -> Ready, regardless of phase. A Rollout with spec.replicas=0 runs
	// nothing, so a Degraded/InvalidSpec phase on it is noise nobody acts on (e.g. a
	// parked rollout with an incomplete strategy). The error only becomes real when it is
	// scaled back up, at which point the phase reflects it. Mirrors the scaled-to-zero ->
	// Ready treatment of the other workload kinds.
	replicas, found, err := unstructured.NestedInt64(u.Object, "spec", "replicas")
	if err != nil {
		return StatusUnknown, err
	}
	// Argo defaults an unset spec.replicas to 1.
	desired := int64(1)
	if found {
		desired = replicas
	}
	if desired == 0 {
		return StatusReady, nil
	}

	phase, phaseFound, err := unstructured.NestedString(u.Object, "status", "phase")
	if err != nil {
		return StatusUnknown, err
	}

	// A phase Argo itself calls a failure is authoritative — report degraded immediately,
	// don't second-guess it with replica counts.
	switch phase {
	case "Degraded", "Error":
		return StatusInProgress, nil
	}

	// For every other phase (Healthy/Inactive/Progressing/Paused/empty/unknown) the phase
	// alone is NOT sufficient to declare Ready. `Progressing` in particular covers BOTH a
	// healthy scale-up/canary step (existing pods keep serving) AND a rollout whose new
	// pods are crash-looping and never become available — Argo sits in `Progressing` for
	// the whole progressDeadlineSeconds window before flipping to `Degraded`. Trusting the
	// phase (the 1.2.0..1.3.x behavior) reported such a down rollout as Ready, so the
	// Application CR never transitioned and downstream monitoring never saw the outage.
	//
	// The discriminator — same one deploymentStatus uses — is actual serving capacity:
	// the `Available` condition and `availableReplicas`. A Progressing rollout that is
	// still serving its full desired complement is a healthy scale-up (Ready); one serving
	// fewer than desired is genuinely mid-degrade (InProgress).
	if hasAvailableConditionFalse(u) {
		return StatusInProgress, nil
	}
	available, _, err := unstructured.NestedInt64(u.Object, "status", "availableReplicas")
	if err != nil {
		return StatusUnknown, err
	}
	if available >= desired {
		return StatusReady, nil
	}
	// Serving fewer replicas than desired and not explicitly scaled to zero: the rollout is
	// not fully available. This catches the down/crash-looping rollout that stays in
	// `Progressing`. An empty/unknown phase with no availability also lands here (InProgress),
	// preserving the prior "missing phase -> InProgress" behavior.
	if !phaseFound || phase == "" {
		return StatusInProgress, nil
	}
	switch phase {
	case "Healthy", "Inactive", "Progressing", "Paused":
		return StatusInProgress, nil
	default:
		// Genuinely unrecognized phase with insufficient availability — surface as Unknown
		// so an unmapped Argo phase is visible rather than silently Ready.
		return StatusUnknown, nil
	}
}

// hasAvailableConditionFalse reports whether the object carries a status.conditions entry
// of type "Available" with status "False" — Argo Rollout mirrors the Deployment
// convention, writing an Available condition alongside the phase. An explicit False is an
// authoritative "not serving" signal regardless of phase or replica counters.
func hasAvailableConditionFalse(u *unstructured.Unstructured) bool {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(cond, "type"); t != "Available" {
			continue
		}
		s, _, _ := unstructured.NestedString(cond, "status")
		return s == "False"
	}
	return false
}

// Kubernetes Gateway API — Gateway (gateway.networking.k8s.io).
//
// Gateway API resources use positive-polarity conditions (Accepted/Programmed/
// ResolvedRefs), unlike the workload kinds' Ready/InProgress. A Gateway is serving
// once the controller has Accepted it and Programmed the data plane. We gate Ready
// on both being True.
//
// We do NOT gate on the "Ready" condition: Gateway API reserves "Ready" for future
// use and current implementations don't set it (Programmed is the health signal).
// We also don't require ResolvedRefs on the Gateway itself — a listener with an
// unresolved certificate ref is a per-listener problem the Programmed condition
// already reflects when it blocks the data plane. A Gateway that has not yet been
// reconciled (no conditions) is InProgress.
func gatewayStatus(u *unstructured.Unstructured) (string, error) {
	return readyFromPositiveConditions(u, "Accepted", "Programmed")
}

// Kubernetes Gateway API — HTTPRoute (gateway.networking.k8s.io).
//
// Route status is per-parent (status.parents[].conditions), not a top-level
// status.conditions like the Gateway. A route is serving when at least one parent
// has Accepted=True and ResolvedRefs=True: attached to a listener with all its
// backendRefs resolved. If any attached parent reports Accepted=False or
// ResolvedRefs=False (e.g. BackendNotFound), the route is InProgress. A route with
// no parents yet (not reconciled) is InProgress.
func routeStatus(u *unstructured.Unstructured) (string, error) {
	parents, found, err := unstructured.NestedSlice(u.Object, "status", "parents")
	if err != nil {
		return StatusUnknown, err
	}
	if !found || len(parents) == 0 {
		return StatusInProgress, nil
	}

	sawHealthyParent := false
	for _, p := range parents {
		parent, ok := p.(map[string]any)
		if !ok {
			continue
		}
		conds, found, err := unstructured.NestedSlice(parent, "conditions")
		if err != nil {
			return StatusUnknown, err
		}
		if !found {
			continue
		}
		accepted, resolved := conditionTrue(conds, "Accepted"), conditionTrue(conds, "ResolvedRefs")
		// A parent that explicitly rejected the route or failed to resolve its
		// backends is a genuine problem — surface it as InProgress.
		if conditionFalse(conds, "Accepted") || conditionFalse(conds, "ResolvedRefs") {
			return StatusInProgress, nil
		}
		if accepted && resolved {
			sawHealthyParent = true
		}
	}
	if sawHealthyParent {
		return StatusReady, nil
	}
	return StatusInProgress, nil
}

// kagent.dev — Agent and ModelConfig (kagent.dev/v1alpha2).
//
// kagent controllers report health via a positive-polarity Accepted condition on
// status.conditions (Kubernetes-conventional). Ready once Accepted=True: the
// controller validated the resource (e.g. resolved the ModelConfig secret / built
// the agent deployment). Accepted=False or not-yet-reconciled (no conditions) is
// InProgress.
func kagentStatus(u *unstructured.Unstructured) (string, error) {
	return readyFromPositiveConditions(u, "Accepted")
}

// readyFromPositiveConditions returns Ready only when every named condition is
// present on status.conditions with Status=True. Any missing or non-True named
// condition yields InProgress. Used by resources (Gateway API, kagent) that report
// health via positive-polarity conditions rather than the workload Ready/InProgress
// convention handled by statusFromStandardConditions.
func readyFromPositiveConditions(u *unstructured.Unstructured, required ...string) (string, error) {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil {
		return StatusUnknown, err
	}
	if !found {
		return StatusInProgress, nil
	}
	for _, name := range required {
		if !conditionTrue(conds, name) {
			return StatusInProgress, nil
		}
	}
	return StatusReady, nil
}

// conditionTrue reports whether the named condition exists in conds with Status=True.
func conditionTrue(conds []any, name string) bool {
	return conditionHasStatus(conds, name, "True")
}

// conditionFalse reports whether the named condition exists in conds with Status=False.
func conditionFalse(conds []any, name string) bool {
	return conditionHasStatus(conds, name, "False")
}

func conditionHasStatus(conds []any, name, status string) bool {
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		t, _ := cond["type"].(string)
		if t != name {
			continue
		}
		s, _ := cond["status"].(string)
		return s == status
	}
	return false
}

// allIngressProvisioned reports whether every LoadBalancer ingress entry carries an IP
// or a Hostname. AWS ELB/NLB only ever set Hostname, never IP.
func allIngressProvisioned(ingress []corev1.LoadBalancerIngress) bool {
	for _, i := range ingress {
		if isEmpty(i.IP) && isEmpty(i.Hostname) {
			return false
		}
	}
	return true
}

func isEmpty(s string) bool {
	return len(strings.TrimSpace(s)) == 0
}

// getConditionOfType returns the status of the named condition. It uses comma-ok type
// assertions throughout: many CRD schemas mark "reason" (and even "status") omitempty,
// and this function runs for every unhandled kind via status()'s default case — an
// unchecked assertion here would panic and crash-loop the controller on a perfectly
// valid but sparse condition entry.
func getConditionOfType(u *unstructured.Unstructured, conditionType string) (corev1.ConditionStatus, bool, error) {
	conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return corev1.ConditionFalse, false, err
	}

	for _, c := range conditions {
		condition, ok := c.(map[string]any)
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
			conditionStatus, _ := condition["status"].(string)
			return corev1.ConditionStatus(conditionStatus), true, nil
		}
	}
	return corev1.ConditionFalse, false, nil
}
