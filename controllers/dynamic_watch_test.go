// Copyright 2020 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// This suite proves the DYNAMIC COMPONENT WATCH fires: a change to a matched component
// (a Deployment's status) triggers a reconcile of the owning Application in real time,
// instead of waiting for the cache resync. To rule out the resync as the trigger, we set
// the manager SyncPeriod to an hour — so any reconcile within seconds must have come from
// the component watch, not a periodic resync.
var _ = Describe("dynamic component watch", func() {
	var (
		stopMgr    context.CancelFunc
		mgrStopped *sync.WaitGroup
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		hour := time.Hour
		mgr, err := manager.New(cfg, manager.Options{
			Cache: cache.Options{SyncPeriod: &hour}, // disable resync as a trigger source
		})
		Expect(err).NotTo(HaveOccurred())
		c = mgr.GetClient()

		r := &ApplicationReconciler{
			Client: mgr.GetClient(),
			Mapper: mgr.GetRESTMapper(),
		}
		// Use the REAL SetupWithManager so the dynamic-watch wiring is active.
		Expect(r.SetupWithManager(mgr)).NotTo(HaveOccurred())

		stopMgr, mgrStopped = StartTestManager(mgr)
	})

	AfterEach(func() {
		stopMgr()
		mgrStopped.Wait()
	})

	It("reconciles the Application within seconds when a matched Deployment's status changes (resync disabled)", func() {
		labelSet := map[string]string{"watch-test": uuid.New().String()}
		ns := metav1.NamespaceDefault

		dep := createDeployment(labelSet, ns)
		Expect(c.Create(ctx, dep)).NotTo(HaveOccurred())

		app := &appv1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "watch-app-" + uuid.New().String()[:8], Namespace: ns, Labels: labelSet},
			Spec: appv1beta1.ApplicationSpec{
				Selector:            &metav1.LabelSelector{MatchLabels: labelSet},
				ComponentGroupKinds: []metav1.GroupKind{{Group: "apps", Kind: "Deployment"}},
			},
		}
		Expect(c.Create(ctx, app)).NotTo(HaveOccurred())
		appKey := types.NamespacedName{Name: app.Name, Namespace: ns}

		// Wait for the first reconcile (from the Application create) to populate the
		// component list and register the Deployment watch.
		Eventually(func() int {
			got := &appv1beta1.Application{}
			if err := c.Get(ctx, appKey, got); err != nil {
				return 0
			}
			return len(got.Status.ComponentList.Objects)
		}, timeout, 200*time.Millisecond).Should(Equal(1), "component should be tracked after initial reconcile")

		// Record the Application's resourceVersion, then mutate ONLY the Deployment's
		// status. If the watch fires, the controller re-reconciles and rewrites the
		// Application status (bumping its resourceVersion) within seconds — with no resync.
		before := &appv1beta1.Application{}
		Expect(c.Get(ctx, appKey, before)).NotTo(HaveOccurred())

		// Set the Deployment Available=True so its component status flips Ready -> proving
		// the new status was recomputed from the changed Deployment.
		fetched := &apps.Deployment{}
		Expect(c.Get(ctx, types.NamespacedName{Name: dep.Name, Namespace: ns}, fetched)).NotTo(HaveOccurred())
		fetched.Status = apps.DeploymentStatus{
			Replicas:           1,
			ReadyReplicas:      1,
			AvailableReplicas:  1,
			UpdatedReplicas:    1,
			ObservedGeneration: fetched.Generation,
			Conditions: []apps.DeploymentCondition{
				{Type: apps.DeploymentAvailable, Status: core.ConditionTrue, Reason: "MinimumReplicasAvailable"},
			},
		}
		Expect(c.Status().Update(ctx, fetched)).NotTo(HaveOccurred())

		// The component's status in the Application must become Ready, and it must happen
		// fast (well under the 1h resync) — proving the Deployment watch triggered it.
		Eventually(func() string {
			got := &appv1beta1.Application{}
			if err := c.Get(ctx, appKey, got); err != nil {
				return ""
			}
			for _, o := range got.Status.ComponentList.Objects {
				if o.Kind == "Deployment" {
					return o.Status
				}
			}
			return ""
		}, 15*time.Second, 200*time.Millisecond).Should(Equal(StatusReady),
			"component watch must trigger a reconcile that recomputes the Deployment status within seconds")
	})
})
