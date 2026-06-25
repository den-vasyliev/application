// Copyright 2020 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Exhaustive matrix over EVERY status shape a Deployment passes through during a 2->3
// scale-up. The point: stop inventing one input and asserting it. Enumerate all of them
// and FAIL if any "still serving" shape returns InProgress. This surfaces the bug
// mechanically instead of by guessing.
var _ = Describe("deploymentStatus matrix (2->3 scale-up, exhaustive)", func() {
	toU := func(d *apps.Deployment) *unstructured.Unstructured {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(d)
		Expect(err).NotTo(HaveOccurred())
		u := &unstructured.Unstructured{Object: m}
		u.SetGroupVersionKind(apps.SchemeGroupVersion.WithKind("Deployment"))
		return u
	}

	availTrue := apps.DeploymentCondition{Type: apps.DeploymentAvailable, Status: core.ConditionTrue}
	availFalse := apps.DeploymentCondition{Type: apps.DeploymentAvailable, Status: core.ConditionFalse}
	progressing := apps.DeploymentCondition{Type: apps.DeploymentProgressing, Status: core.ConditionTrue, Reason: "ReplicaSetUpdated"}

	// Each row: a status a real 2->3 scale-up Deployment can have, with the EXPECTED verdict.
	// "serving" = at least 2 replicas available, existing pods up -> must be Ready.
	type row struct {
		desc   string
		gen    int64
		obsGen int64
		spec   int32
		repl   int32
		ready  int32
		avail  int32
		conds  []apps.DeploymentCondition
		want   string
	}
	rows := []row{
		{"steady 2/2, Available+Progressing", 1, 1, 2, 2, 2, 2, []apps.DeploymentCondition{availTrue, progressing}, StatusReady},
		{"scale issued: gen=2 obsGen=1, still 2 avail, Available=True", 2, 1, 3, 2, 2, 2, []apps.DeploymentCondition{availTrue}, StatusReady},
		{"scale issued: gen=2 obsGen=1, still 2 avail, NO conditions yet", 2, 1, 3, 2, 2, 2, nil, StatusReady},
		{"obsGen caught up: replicas=3 ready=2 avail=2, Available+Progressing", 2, 2, 3, 3, 2, 2, []apps.DeploymentCondition{availTrue, progressing}, StatusReady},
		{"new pod pending: replicas=3 ready=2 avail=2, only Progressing (no Available yet)", 2, 2, 3, 3, 2, 2, []apps.DeploymentCondition{progressing}, StatusReady},
		{"new pod pending: replicas=3 ready=2 avail=2, NO conditions", 2, 2, 3, 3, 2, 2, nil, StatusReady},
		{"new pod ready: 3/3, Available+Progressing", 2, 2, 3, 3, 3, 3, []apps.DeploymentCondition{availTrue, progressing}, StatusReady},
		// Genuine failures during/around scale — MUST be InProgress:
		{"all gone: replicas=3 ready=0 avail=0, Available=False", 2, 2, 3, 3, 0, 0, []apps.DeploymentCondition{availFalse}, StatusInProgress},
		{"all gone: replicas=3 ready=0 avail=0, NO conditions", 2, 2, 3, 3, 0, 0, nil, StatusInProgress},
	}

	for _, r := range rows {
		r := r
		It("scale-up shape: "+r.desc, func() {
			specRepl := r.spec
			d := &apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: r.gen},
				Spec:       apps.DeploymentSpec{Replicas: &specRepl},
				Status: apps.DeploymentStatus{
					ObservedGeneration: r.obsGen,
					Replicas:           r.repl,
					ReadyReplicas:      r.ready,
					AvailableReplicas:  r.avail,
					Conditions:         r.conds,
				},
			}
			got, err := deploymentStatus(toU(d))
			Expect(err).NotTo(HaveOccurred())
			By(fmt.Sprintf("[%s] spec=%d repl=%d ready=%d avail=%d conds=%d => got=%s want=%s",
				r.desc, r.spec, r.repl, r.ready, r.avail, len(r.conds), got, r.want))
			Expect(got).To(Equal(r.want), "WRONG VERDICT for: "+r.desc)
		})
	}
})

// Exhaustive matrix over the status shapes an Argo Rollout passes through during a
// scale-up. prod is Rollout-heavy; rolloutStatus reads status.phase, which LAGS during a
// scale (replica counters update first, phase recomputed later). Any "still serving"
// shape that returns InProgress is the flap.
var _ = Describe("rolloutStatus matrix (scale-up, exhaustive)", func() {
	mk := func(specReplicas int64, phase string, available int64) *unstructured.Unstructured {
		obj := map[string]interface{}{
			"spec":   map[string]interface{}{"replicas": specReplicas},
			"status": map[string]interface{}{"availableReplicas": available},
		}
		if phase != "" {
			obj["status"].(map[string]interface{})["phase"] = phase
		}
		u := &unstructured.Unstructured{Object: obj}
		u.SetAPIVersion("argoproj.io/v1alpha1")
		u.SetKind("Rollout")
		return u
	}

	type rrow struct {
		desc  string
		spec  int64
		phase string
		avail int64
		want  string
	}
	rows := []rrow{
		{"steady Healthy 2/2", 2, "Healthy", 2, StatusReady},
		{"scale-up Progressing, 2 still serving", 3, "Progressing", 2, StatusReady},
		{"scale-up: phase EMPTY (not recomputed yet), 2 serving", 3, "", 2, StatusReady},
		{"scale-up Paused, 2 serving", 3, "Paused", 2, StatusReady},
		{"completed: Healthy 3/3", 3, "Healthy", 3, StatusReady},
		{"scaled to zero, phase Degraded (parked)", 0, "Degraded", 0, StatusReady},
		// genuine failure at non-zero replicas:
		{"Degraded, nothing available", 3, "Degraded", 0, StatusInProgress},
	}
	for _, r := range rows {
		r := r
		It("rollout shape: "+r.desc, func() {
			got, err := rolloutStatus(mk(r.spec, r.phase, r.avail))
			Expect(err).NotTo(HaveOccurred())
			By(fmt.Sprintf("[%s] spec=%d phase=%q avail=%d => got=%s want=%s",
				r.desc, r.spec, r.phase, r.avail, got, r.want))
			Expect(got).To(Equal(r.want), "WRONG VERDICT for: "+r.desc)
		})
	}
})
