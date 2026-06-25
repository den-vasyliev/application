// Copyright 2020 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func toUnstructured(obj runtime.Object) *unstructured.Unstructured {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	Expect(err).NotTo(HaveOccurred())
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(apps.SchemeGroupVersion.WithKind("Deployment"))
	return u
}

func int32p(i int32) *int32 { return &i }

var _ = Describe("deploymentStatus", func() {
	newDeployment := func(gen int64, spec, ready, available int32, conds []apps.DeploymentCondition) *apps.Deployment {
		return &apps.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Spec:       apps.DeploymentSpec{Replicas: int32p(spec)},
			Status: apps.DeploymentStatus{
				ObservedGeneration: gen,
				Replicas:           spec,
				ReadyReplicas:      ready,
				AvailableReplicas:  available,
				Conditions:         conds,
			},
		}
	}

	availableTrue := apps.DeploymentCondition{Type: apps.DeploymentAvailable, Status: core.ConditionTrue}
	availableFalse := apps.DeploymentCondition{Type: apps.DeploymentAvailable, Status: core.ConditionFalse}
	replicaFailureTrue := apps.DeploymentCondition{Type: apps.DeploymentReplicaFailure, Status: core.ConditionTrue}

	It("is Ready at full desired count when Available", func() {
		d := newDeployment(1, 3, 3, 3, []apps.DeploymentCondition{availableTrue})
		Expect(deploymentStatus(toUnstructured(d))).To(Equal(StatusReady))
	})

	It("stays Ready during an HPA scale-up (2->3) while Available — no false degraded", func() {
		// spec jumped to 3, but the new replica is still inside its readiness-probe
		// window: ready/available lag at 2. Available stays True because the existing
		// replicas keep serving, so the app is healthy.
		d := newDeployment(2, 3, 2, 2, []apps.DeploymentCondition{availableTrue})
		Expect(deploymentStatus(toUnstructured(d))).To(Equal(StatusReady))
	})

	It("stays Ready during a scale-up while observedGeneration still lags generation", func() {
		// The real prod flap: an HPA scale-up bumps metadata.generation to 3 immediately,
		// but the Deployment controller has not yet written status.observedGeneration (still
		// 2). Available is True the whole time. Gating Ready on generation == observedGeneration
		// would emit a false AppDegraded in this window. It must stay Ready.
		d := newDeployment(3, 3, 2, 2, []apps.DeploymentCondition{availableTrue})
		d.Status.ObservedGeneration = 2
		Expect(deploymentStatus(toUnstructured(d))).To(Equal(StatusReady))
	})

	It("is InProgress when the Deployment is not Available", func() {
		d := newDeployment(2, 3, 0, 0, []apps.DeploymentCondition{availableFalse})
		Expect(deploymentStatus(toUnstructured(d))).To(Equal(StatusInProgress))
	})

	It("is InProgress on ReplicaFailure even if Available", func() {
		d := newDeployment(2, 3, 2, 2, []apps.DeploymentCondition{availableTrue, replicaFailureTrue})
		Expect(deploymentStatus(toUnstructured(d))).To(Equal(StatusInProgress))
	})

	It("is InProgress when status lags generation AND is not Available", func() {
		// Generation skew alone must NOT degrade (see scale-up case above). A real problem
		// is signalled by Available=False / ReplicaFailure, which is what gates InProgress.
		d := newDeployment(3, 3, 0, 0, []apps.DeploymentCondition{availableFalse})
		d.Status.ObservedGeneration = 2
		Expect(deploymentStatus(toUnstructured(d))).To(Equal(StatusInProgress))
	})

	It("is Ready when scaled to zero", func() {
		d := newDeployment(1, 0, 0, 0, nil)
		Expect(deploymentStatus(toUnstructured(d))).To(Equal(StatusReady))
	})

	It("is InProgress with no conditions and a non-zero spec", func() {
		d := newDeployment(1, 3, 3, 3, nil)
		Expect(deploymentStatus(toUnstructured(d))).To(Equal(StatusInProgress))
	})
})

var _ = Describe("stsStatus", func() {
	newSTS := func(gen int64, spec, current, ready, replicas int32) *apps.StatefulSet {
		return &apps.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Spec:       apps.StatefulSetSpec{Replicas: int32p(spec)},
			Status: apps.StatefulSetStatus{
				ObservedGeneration: gen,
				Replicas:           replicas,
				CurrentReplicas:    current,
				ReadyReplicas:      ready,
			},
		}
	}
	statusOf := func(s *apps.StatefulSet) string {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s)
		Expect(err).NotTo(HaveOccurred())
		res, err := stsStatus(&unstructured.Unstructured{Object: m})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	It("is Ready at full count", func() {
		Expect(statusOf(newSTS(1, 3, 3, 3, 3))).To(Equal(StatusReady))
	})
	It("stays Ready during scale-up while managed pods are all ready", func() {
		// spec=3, but controller has only rolled 2 (current=2) and both are ready.
		Expect(statusOf(newSTS(2, 3, 2, 2, 2))).To(Equal(StatusReady))
	})
	It("is InProgress when a managed pod is not ready", func() {
		Expect(statusOf(newSTS(2, 3, 3, 2, 3))).To(Equal(StatusInProgress))
	})
	It("is Ready when scaled to zero", func() {
		Expect(statusOf(newSTS(1, 0, 0, 0, 0))).To(Equal(StatusReady))
	})
	It("stays Ready during a scale-up while observedGeneration still lags generation", func() {
		// generation bumped to 3 by the scale; controller has rolled 2 current pods, both
		// ready, and not yet written observedGeneration (still 2). Must stay Ready.
		s := newSTS(3, 3, 2, 2, 2)
		s.Status.ObservedGeneration = 2
		Expect(statusOf(s)).To(Equal(StatusReady))
	})
	It("is InProgress when a managed pod is not ready even if generation lags", func() {
		s := newSTS(3, 3, 3, 2, 3)
		s.Status.ObservedGeneration = 2
		Expect(statusOf(s)).To(Equal(StatusInProgress))
	})
})

var _ = Describe("replicasetStatus", func() {
	statusOf := func(rs *apps.ReplicaSet) string {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(rs)
		Expect(err).NotTo(HaveOccurred())
		res, err := replicasetStatus(&unstructured.Unstructured{Object: m})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	newRS := func(gen int64, spec, available int32, conds []apps.ReplicaSetCondition) *apps.ReplicaSet {
		return &apps.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Spec:       apps.ReplicaSetSpec{Replicas: int32p(spec)},
			Status: apps.ReplicaSetStatus{
				ObservedGeneration: gen,
				Replicas:           spec,
				AvailableReplicas:  available,
				Conditions:         conds,
			},
		}
	}
	rsFailure := []apps.ReplicaSetCondition{{Type: apps.ReplicaSetReplicaFailure, Status: core.ConditionTrue}}

	It("is Ready at full count", func() {
		Expect(statusOf(newRS(1, 3, 3, nil))).To(Equal(StatusReady))
	})
	It("stays Ready during scale-up (some replicas available)", func() {
		Expect(statusOf(newRS(2, 3, 2, nil))).To(Equal(StatusReady))
	})
	It("stays Ready during a scale-up while observedGeneration lags generation", func() {
		rs := newRS(3, 3, 2, nil)
		rs.Status.ObservedGeneration = 2
		Expect(statusOf(rs)).To(Equal(StatusReady))
	})
	It("is InProgress when nothing is available", func() {
		Expect(statusOf(newRS(2, 3, 0, nil))).To(Equal(StatusInProgress))
	})
	It("is InProgress on ReplicaFailure", func() {
		Expect(statusOf(newRS(2, 3, 2, rsFailure))).To(Equal(StatusInProgress))
	})
	It("is Ready when scaled to zero", func() {
		Expect(statusOf(newRS(1, 0, 0, nil))).To(Equal(StatusReady))
	})
})

var _ = Describe("replicationControllerStatus", func() {
	statusOf := func(rc *core.ReplicationController) string {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(rc)
		Expect(err).NotTo(HaveOccurred())
		res, err := replicationControllerStatus(&unstructured.Unstructured{Object: m})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	newRC := func(gen int64, spec, available int32) *core.ReplicationController {
		return &core.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Spec:       core.ReplicationControllerSpec{Replicas: int32p(spec)},
			Status: core.ReplicationControllerStatus{
				ObservedGeneration: gen,
				Replicas:           spec,
				AvailableReplicas:  available,
			},
		}
	}
	It("is Ready at full count", func() {
		Expect(statusOf(newRC(1, 3, 3))).To(Equal(StatusReady))
	})
	It("stays Ready during scale-up", func() {
		Expect(statusOf(newRC(2, 3, 2))).To(Equal(StatusReady))
	})
	It("stays Ready during a scale-up while observedGeneration lags generation", func() {
		rc := newRC(3, 3, 2)
		rc.Status.ObservedGeneration = 2
		Expect(statusOf(rc)).To(Equal(StatusReady))
	})
	It("is InProgress when nothing is available", func() {
		Expect(statusOf(newRC(2, 3, 0))).To(Equal(StatusInProgress))
	})
	It("is Ready when scaled to zero", func() {
		Expect(statusOf(newRC(1, 0, 0))).To(Equal(StatusReady))
	})
})

var _ = Describe("daemonsetStatus", func() {
	statusOf := func(ds *apps.DaemonSet) string {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ds)
		Expect(err).NotTo(HaveOccurred())
		res, err := daemonsetStatus(&unstructured.Unstructured{Object: m})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	newDS := func(gen int64, desired, ready, unavailable int32) *apps.DaemonSet {
		return &apps.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Status: apps.DaemonSetStatus{
				ObservedGeneration:     gen,
				DesiredNumberScheduled: desired,
				NumberReady:            ready,
				NumberAvailable:        ready,
				NumberUnavailable:      unavailable,
			},
		}
	}
	It("is Ready when fully covered", func() {
		Expect(statusOf(newDS(1, 3, 3, 0))).To(Equal(StatusReady))
	})
	It("stays Ready when a node joins (desired jumps, none unavailable yet)", func() {
		// New node scheduled but its pod not counted unavailable; existing pods serve.
		Expect(statusOf(newDS(2, 4, 3, 0))).To(Equal(StatusReady))
	})
	It("stays Ready when a node joins while observedGeneration lags generation", func() {
		ds := newDS(3, 4, 3, 0)
		ds.Status.ObservedGeneration = 2
		Expect(statusOf(ds)).To(Equal(StatusReady))
	})
	It("is InProgress when a scheduled pod is unavailable", func() {
		Expect(statusOf(newDS(2, 4, 3, 1))).To(Equal(StatusInProgress))
	})
	It("is Ready when scheduled onto zero nodes", func() {
		Expect(statusOf(newDS(1, 0, 0, 0))).To(Equal(StatusReady))
	})
})

var _ = Describe("rolloutStatus", func() {
	// Rollout is a CRD with no typed dep in this repo, so build the unstructured directly.
	newRollout := func(replicas *int64, phase string) *unstructured.Unstructured {
		obj := map[string]interface{}{
			"spec":   map[string]interface{}{},
			"status": map[string]interface{}{},
		}
		if replicas != nil {
			obj["spec"].(map[string]interface{})["replicas"] = *replicas
		}
		if phase != "" {
			obj["status"].(map[string]interface{})["phase"] = phase
		}
		u := &unstructured.Unstructured{Object: obj}
		u.SetAPIVersion("argoproj.io/v1alpha1")
		u.SetKind("Rollout")
		return u
	}
	int64p := func(i int64) *int64 { return &i }
	statusOf := func(replicas *int64, phase string) string {
		res, err := rolloutStatus(newRollout(replicas, phase))
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	It("is Ready when Healthy", func() {
		Expect(statusOf(int64p(4), "Healthy")).To(Equal(StatusReady))
	})
	It("is Ready when Inactive", func() {
		Expect(statusOf(int64p(4), "Inactive")).To(Equal(StatusReady))
	})
	It("stays Ready while Progressing (scale-up / canary step, still serving)", func() {
		// The Rollout equivalent of the Deployment 2->3 flap: Progressing during an HPA
		// scale-up must not degrade the Application while existing pods keep serving.
		Expect(statusOf(int64p(5), "Progressing")).To(Equal(StatusReady))
	})
	It("stays Ready while Paused (blue/green or canary pause, serving)", func() {
		Expect(statusOf(int64p(4), "Paused")).To(Equal(StatusReady))
	})
	It("is InProgress when Degraded", func() {
		Expect(statusOf(int64p(4), "Degraded")).To(Equal(StatusInProgress))
	})
	It("is InProgress when Error", func() {
		Expect(statusOf(int64p(4), "Error")).To(Equal(StatusInProgress))
	})
	It("is Ready when scaled to zero, ignoring a Degraded phase", func() {
		// example-parked-rollout: spec.replicas=0, phase=Degraded (InvalidSpec). Nothing runs,
		// so nobody acts on the error — it only becomes real once scaled back up.
		Expect(statusOf(int64p(0), "Degraded")).To(Equal(StatusReady))
	})
	It("is Ready when scaled to zero with no phase", func() {
		Expect(statusOf(int64p(0), "")).To(Equal(StatusReady))
	})
	It("is InProgress when phase is missing and not scaled to zero", func() {
		Expect(statusOf(int64p(4), "")).To(Equal(StatusInProgress))
	})
	It("is Unknown on an unrecognized phase", func() {
		Expect(statusOf(int64p(4), "SomethingNew")).To(Equal(StatusUnknown))
	})
})
