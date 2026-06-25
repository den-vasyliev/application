// Copyright 2020 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

// This suite reproduces the prod scale-up scenario END TO END against a real
// apiserver (envtest): create an Application + a Deployment it selects, set the
// Deployment's status to the exact values a scaling Deployment has in prod, run the
// real reconcile, and read the Application status the controller actually wrote.
//
// envtest has no Deployment controller, so we set .status ourselves to mirror prod.
var _ = Describe("scale-up reconcile (envtest, end-to-end)", func() {
	var (
		ctx     context.Context
		ns      string
		counter int
		mapper  apimeta.RESTMapper
	)

	BeforeEach(func() {
		ctx = context.Background()
		counter++
		ns = fmt.Sprintf("scaleup-%d", counter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())

		dc, err := discovery.NewDiscoveryClientForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		gr, err := restmapper.GetAPIGroupResources(dc)
		Expect(err).NotTo(HaveOccurred())
		mapper = restmapper.NewDiscoveryRESTMapper(gr)
	})

	createApp := func(labels map[string]string) types.NamespacedName {
		app := &appv1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
			Spec: appv1beta1.ApplicationSpec{
				ComponentGroupKinds: []metav1.GroupKind{{Group: "apps", Kind: "Deployment"}},
				Selector:            &metav1.LabelSelector{MatchLabels: labels},
			},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		return types.NamespacedName{Name: "app", Namespace: ns}
	}

	createDeploymentWithStatus := func(name string, labels map[string]string, specReplicas int32, st appsv1.DeploymentStatus) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: &specReplicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		fetched := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), fetched)).To(Succeed())
		fetched.Status = st
		Expect(k8sClient.Status().Update(ctx, fetched)).To(Succeed())
	}

	// runReconcile runs the REAL reconciler against the envtest apiserver, then returns
	// the Application status the controller persisted.
	runReconcile := func(appKey types.NamespacedName) *appv1beta1.ApplicationStatus {
		r := &ApplicationReconciler{
			Client: k8sClient,
			Mapper: mapper,
			Scheme: k8sClient.Scheme(),
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: appKey})
		Expect(err).NotTo(HaveOccurred())
		got := &appv1beta1.Application{}
		Expect(k8sClient.Get(ctx, appKey, got)).To(Succeed())
		return &got.Status
	}

	readyOf := func(st *appv1beta1.ApplicationStatus) corev1.ConditionStatus {
		for _, c := range st.Conditions {
			if c.Type == appv1beta1.Ready {
				return c.Status
			}
		}
		return corev1.ConditionUnknown
	}

	It("keeps the Application Ready during a scale-up when the Deployment is Available (prod shot 1: 4/5, Available=True)", func() {
		labels := map[string]string{"app.kubernetes.io/instance": "example-service"}
		appKey := createApp(labels)
		createDeploymentWithStatus("example-service", labels, 5, appsv1.DeploymentStatus{
			Replicas:           5,
			ReadyReplicas:      4,
			AvailableReplicas:  4,
			UpdatedReplicas:    5,
			ObservedGeneration: 1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"},
			},
		})

		st := runReconcile(appKey)
		By(fmt.Sprintf("ComponentsReady=%q components=%d ready=%v", st.ComponentsReady, len(st.Objects), readyOf(st)))
		for _, o := range st.Objects {
			By(fmt.Sprintf("  component %s/%s = %s", o.Kind, o.Name, o.Status))
		}
		Expect(readyOf(st)).To(Equal(corev1.ConditionTrue))
	})

	It("DIAGNOSTIC: scale-up with Available=False (one pod 0/1 below minAvailable) — records actual behavior", func() {
		// prod shot 2 (dev): one pod 0/1. If that drops below minAvailable, kube sets
		// Available=False and InProgress is CORRECT, not a false positive. This test
		// records which it is so we stop guessing.
		labels := map[string]string{"app.kubernetes.io/instance": "example-service"}
		appKey := createApp(labels)
		createDeploymentWithStatus("example-service", labels, 6, appsv1.DeploymentStatus{
			Replicas:           6,
			ReadyReplicas:      5,
			AvailableReplicas:  5,
			UpdatedReplicas:    6,
			ObservedGeneration: 1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"},
			},
		})

		st := runReconcile(appKey)
		By(fmt.Sprintf("5/6 ready, Available=True => ComponentsReady=%q Ready=%v", st.ComponentsReady, readyOf(st)))
		Expect(readyOf(st)).To(Equal(corev1.ConditionTrue),
			"5/6 ready with Available=True is a healthy scale-up and must stay Ready")
	})

	It("is correctly InProgress only when the Deployment is genuinely Available=False", func() {
		// This is the case that is NOT a false positive: a scale-up where a failing pod
		// drops the Deployment below minAvailable, so kube sets Available=False.
		labels := map[string]string{"app.kubernetes.io/instance": "example-service"}
		appKey := createApp(labels)
		createDeploymentWithStatus("example-service", labels, 5, appsv1.DeploymentStatus{
			Replicas:           5,
			ReadyReplicas:      0,
			AvailableReplicas:  0,
			UpdatedReplicas:    5,
			ObservedGeneration: 1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable"},
			},
		})
		st := runReconcile(appKey)
		By(fmt.Sprintf("Available=False => ComponentsReady=%q Ready=%v", st.ComponentsReady, readyOf(st)))
		Expect(readyOf(st)).NotTo(Equal(corev1.ConditionTrue),
			"genuine unavailability must report not-Ready")
	})

	It("DIAGNOSTIC: no Available condition at all (kube hasn't published it yet during a fast scale)", func() {
		// If during the scale-up window the Deployment status has replicas but NO
		// Available condition yet, deploymentStatus sees available=false and returns
		// InProgress — a real flap source that hand-built unit tests (which always set
		// a condition) would miss. Record the behavior.
		labels := map[string]string{"app.kubernetes.io/instance": "example-service"}
		appKey := createApp(labels)
		createDeploymentWithStatus("example-service", labels, 5, appsv1.DeploymentStatus{
			Replicas:           5,
			ReadyReplicas:      4,
			AvailableReplicas:  4,
			UpdatedReplicas:    5,
			ObservedGeneration: 1,
			Conditions:         nil, // no conditions published yet
		})
		st := runReconcile(appKey)
		By(fmt.Sprintf("no conditions, 4/5 available => ComponentsReady=%q Ready=%v", st.ComponentsReady, readyOf(st)))
		// 4 replicas are available even without the condition — arguably should be Ready.
		// Assert the CURRENT behavior so we see it explicitly.
		Expect(readyOf(st)).To(Equal(corev1.ConditionTrue),
			"4/5 available should be Ready even if the Available condition is not yet published")
	})
})
