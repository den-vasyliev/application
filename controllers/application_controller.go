// Copyright 2020 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/util/retry"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

// ApplicationReconciler reconciles a Application object
type ApplicationReconciler struct {
	client.Client
	Mapper              meta.RESTMapper
	Scheme              *runtime.Scheme
	StabilizationPeriod time.Duration

	// controller is the handle returned by builder.Build, used to register watches on
	// component kinds dynamically at runtime (see ensureComponentWatches).
	controller controller.Controller
	// cache feeds the dynamically registered component informers.
	cache cache.Cache
	// watchedKinds dedups dynamic watches so each component GVK is watched exactly once
	// for the lifetime of the process.
	watchedKinds sync.Map
}

// +kubebuilder:rbac:groups=app.k8s.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=app.k8s.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=*,resources=*,verbs=list;get;update;patch;watch

func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("application", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	var app appv1beta1.Application
	err := r.Get(ctx, req.NamespacedName, &app)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Application is in the process of being deleted, so no need to do anything.
	if app.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Register real-time watches for this Application's component kinds (idempotent per
	// GVK), so component status changes trigger a reconcile without waiting for the resync.
	r.ensureComponentWatches(ctx, &app)

	resources, errs := r.updateComponents(ctx, &app)
	newApplicationStatus := r.getNewApplicationStatus(ctx, &app, resources, &errs)

	newApplicationStatus.ObservedGeneration = app.Generation
	if equality.Semantic.DeepEqual(newApplicationStatus, &app.Status) {
		if len(errs) > 0 {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}

	// Stabilization: if transitioning from not-Ready to Ready, wait StabilizationPeriod
	// to confirm the healthy state persists before writing — prevents flapping on brief recoveries.
	// We use the lastTransitionTime of the current not-Ready condition as the start of the wait,
	// so the requeue only fires once rather than looping indefinitely.
	if r.StabilizationPeriod > 0 && isTransitioningToReady(newApplicationStatus, &app.Status) {
		notReadySince := notReadySince(&app.Status)
		waited := time.Since(notReadySince)
		if waited < r.StabilizationPeriod {
			return ctrl.Result{RequeueAfter: r.StabilizationPeriod - waited}, nil
		}
	}

	err = r.updateApplicationStatus(ctx, req.NamespacedName, newApplicationStatus)
	return ctrl.Result{}, err
}

func (r *ApplicationReconciler) updateComponents(ctx context.Context, app *appv1beta1.Application) ([]*unstructured.Unstructured, []error) {
	var errs []error
	resources := r.fetchComponentListResources(ctx, app.Spec.ComponentGroupKinds, app.Spec.Selector, app.Namespace, &errs)

	// Owner ref setting is disabled: the controller is a read-only aggregator.
	return resources, errs
}

func (r *ApplicationReconciler) getNewApplicationStatus(ctx context.Context, app *appv1beta1.Application, resources []*unstructured.Unstructured, errList *[]error) *appv1beta1.ApplicationStatus {
	objectStatuses := r.objectStatuses(ctx, resources, errList)
	errs := utilerrors.NewAggregate(*errList)

	aggReady, countWorkloadsReady, totalWorkloads := aggregateReady(objectStatuses)

	countAllReady := 0
	for _, os := range objectStatuses {
		if os.Status == StatusReady {
			countAllReady++
		}
	}

	newApplicationStatus := app.Status.DeepCopy()
	newApplicationStatus.ComponentList = appv1beta1.ComponentList{
		Objects: objectStatuses,
	}
	newApplicationStatus.ComponentsReady = fmt.Sprintf("%d/%d", countAllReady, len(objectStatuses))
	if errs != nil {
		setReadyUnknownCondition(newApplicationStatus, "ComponentsReadyUnknown", "failed to aggregate all components' statuses, check the Error condition for details")
	} else if aggReady {
		setReadyCondition(newApplicationStatus, "ComponentsReady", "all components ready")
	} else {
		setNotReadyCondition(newApplicationStatus, "ComponentsNotReady", fmt.Sprintf("%d/%d workloads not ready", countWorkloadsReady, totalWorkloads))
	}

	if errs != nil {
		setErrorCondition(newApplicationStatus, "ErrorSeen", errs.Error())
	} else {
		clearErrorCondition(newApplicationStatus)
	}

	return newApplicationStatus
}

func (r *ApplicationReconciler) fetchComponentListResources(ctx context.Context, groupKinds []metav1.GroupKind, selector *metav1.LabelSelector, namespace string, errs *[]error) []*unstructured.Unstructured {
	logger := log.FromContext(ctx)
	var resources []*unstructured.Unstructured

	if selector == nil {
		logger.Info("No selector is specified")
		return resources
	}

	for _, gk := range groupKinds {
		mapping, err := r.Mapper.RESTMapping(schema.GroupKind{
			Group: appv1beta1.StripVersion(gk.Group),
			Kind:  gk.Kind,
		})
		if err != nil {
			// Demoted to V(1): fires for every unmapped/misspelled component kind on every
			// reconcile, which floods the default log. Visible with --zap-log-level=debug.
			logger.V(1).Info("NoMappingForGK — skipping", "gk", gk.String(), "error", err)
			continue
		}

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(mapping.GroupVersionKind)
		if err = r.Client.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels(selector.MatchLabels)); err != nil {
			logger.Error(err, "unable to list resources for GVK", "gvk", mapping.GroupVersionKind)
			*errs = append(*errs, err)
			continue
		}

		for _, u := range list.Items {
			resource := u
			resources = append(resources, &resource)
		}
	}
	return resources
}

func (r *ApplicationReconciler) setOwnerRefForResources(ctx context.Context, ownerRef metav1.OwnerReference, resources []*unstructured.Unstructured) error {
	logger := log.FromContext(ctx)
	for _, resource := range resources {
		ownerRefs := resource.GetOwnerReferences()
		ownerRefFound := false
		for i, refs := range ownerRefs {
			if ownerRef.Kind == refs.Kind &&
				ownerRef.APIVersion == refs.APIVersion &&
				ownerRef.Name == refs.Name {
				ownerRefFound = true
				if ownerRef.UID != refs.UID {
					ownerRefs[i] = ownerRef
				}
			}
		}

		if !ownerRefFound {
			ownerRefs = append(ownerRefs, ownerRef)
		}
		resource.SetOwnerReferences(ownerRefs)
		err := r.Client.Update(ctx, resource)
		if err != nil {
			// We log this error, but we continue and try to set the ownerRefs on the other resources.
			logger.Error(err, "ErrorSettingOwnerRef", "gvk", resource.GroupVersionKind().String(),
				"namespace", resource.GetNamespace(), "name", resource.GetName())
		}
	}
	return nil
}

func (r *ApplicationReconciler) objectStatuses(ctx context.Context, resources []*unstructured.Unstructured, errs *[]error) []appv1beta1.ObjectStatus {
	logger := log.FromContext(ctx)
	var objectStatuses []appv1beta1.ObjectStatus
	for _, resource := range resources {
		os := appv1beta1.ObjectStatus{
			Group: resource.GroupVersionKind().Group,
			Kind:  resource.GetKind(),
			Name:  resource.GetName(),
			Link:  resource.GetSelfLink(),
		}
		s, err := status(resource)
		if err != nil {
			logger.Error(err, "unable to compute status for resource", "gvk", resource.GroupVersionKind().String(),
				"namespace", resource.GetNamespace(), "name", resource.GetName())
			*errs = append(*errs, err)
		}
		os.Status = s
		objectStatuses = append(objectStatuses, os)
	}
	sort.Slice(objectStatuses, func(i, j int) bool {
		ki := objectStatuses[i].Group + "/" + objectStatuses[i].Kind + "/" + objectStatuses[i].Name
		kj := objectStatuses[j].Group + "/" + objectStatuses[j].Kind + "/" + objectStatuses[j].Name
		return ki < kj
	})
	return objectStatuses
}

// workloadKinds are the resource kinds that determine Application readiness.
// Infrastructure resources (Service, Ingress, etc.) are tracked in ComponentList
// for visibility but do not affect the Ready condition.
var workloadKinds = map[string]bool{
	"Deployment":            true,
	"StatefulSet":           true,
	"DaemonSet":             true,
	"ReplicaSet":            true,
	"ReplicationController": true,
	"Job":                   true,
	"CronJob":               true,
	"Rollout":               true,
}

func isTransitioningToReady(newStatus, curStatus *appv1beta1.ApplicationStatus) bool {
	newReady := false
	for _, c := range newStatus.Conditions {
		if c.Type == appv1beta1.Ready && c.Status == corev1.ConditionTrue {
			newReady = true
			break
		}
	}
	if !newReady {
		return false
	}
	for _, c := range curStatus.Conditions {
		if c.Type == appv1beta1.Ready && c.Status != corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// notReadySince returns the lastTransitionTime of the Ready=False condition,
// or time.Now() if not found (so stabilization fires immediately for unknown state).
func notReadySince(status *appv1beta1.ApplicationStatus) time.Time {
	for _, c := range status.Conditions {
		if c.Type == appv1beta1.Ready && c.Status != corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Now()
}

func aggregateReady(objectStatuses []appv1beta1.ObjectStatus) (bool, int, int) {
	total, countReady := 0, 0
	for _, os := range objectStatuses {
		if !workloadKinds[os.Kind] {
			continue
		}
		total++
		if os.Status == StatusReady {
			countReady++
		}
	}
	if total == 0 {
		return true, 0, 0
	}
	return countReady == total, countReady, total
}

func (r *ApplicationReconciler) updateApplicationStatus(ctx context.Context, nn types.NamespacedName, status *appv1beta1.ApplicationStatus) error {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		original := &appv1beta1.Application{}
		if err := r.Get(ctx, nn, original); err != nil {
			return err
		}
		original.Status = *status
		if err := r.Client.Status().Update(ctx, original); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to update status of Application %s/%s: %v", nn.Namespace, nn.Name, err)
	}
	return nil
}

func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Build (not Complete) so we keep the Controller handle. We register watches on the
	// Application here; component-kind watches are added lazily at runtime in Reconcile
	// (see ensureComponentWatches), because the set of component kinds is not known until
	// an Application declares them in spec.componentKinds.
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&appv1beta1.Application{}).
		Build(r)
	if err != nil {
		return err
	}
	r.controller = c
	r.cache = mgr.GetCache()
	return nil
}

// ensureComponentWatches registers a dynamic watch for each of the Application's
// component kinds, so a change to any matched component (e.g. a Deployment's status
// updating during a scale-up) enqueues a reconcile of the owning Application in real
// time, rather than waiting for the cache resync (--sync-period, default 120s).
//
// Each GVK is watched exactly once for the process lifetime (deduped via watchedKinds):
// the watch is namespace-agnostic and the map function re-evaluates selectors per event,
// so one watch per kind serves every Application that uses that kind.
func (r *ApplicationReconciler) ensureComponentWatches(ctx context.Context, app *appv1beta1.Application) {
	// No-op when the dynamic-watch machinery isn't wired (e.g. a reconciler constructed
	// without SetupWithManager, as some tests do). Status aggregation still works; only the
	// real-time trigger is skipped, falling back to the cache resync.
	if r.controller == nil || r.cache == nil {
		return
	}
	logger := log.FromContext(ctx)
	for _, gk := range app.Spec.ComponentGroupKinds {
		mapping, err := r.Mapper.RESTMapping(schema.GroupKind{
			Group: appv1beta1.StripVersion(gk.Group),
			Kind:  gk.Kind,
		})
		if err != nil {
			// Kind not installed in the cluster — skip; fetchComponentListResources logs it too.
			continue
		}
		gvk := mapping.GroupVersionKind
		if _, loaded := r.watchedKinds.LoadOrStore(gvk, true); loaded {
			continue // already watching this kind
		}

		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		src := source.Kind(r.cache, client.Object(u),
			handler.EnqueueRequestsFromMapFunc(r.applicationsForComponent))
		if err := r.controller.Watch(src); err != nil {
			// Roll back the dedup entry so a later reconcile can retry the watch.
			r.watchedKinds.Delete(gvk)
			logger.Error(err, "failed to register dynamic component watch", "gvk", gvk.String())
			continue
		}
		// V(1): one line per kind at first registration. Useful but noisy on startup —
		// visible with --zap-log-level=debug, not at default INFO.
		logger.V(1).Info("registered dynamic component watch", "gvk", gvk.String())
	}
}

// applicationsForComponent maps a changed component object to reconcile requests for
// every Application in the component's namespace whose selector matches the component's
// labels. This is the component -> Application fan-out that drives real-time status
// updates.
func (r *ApplicationReconciler) applicationsForComponent(ctx context.Context, obj client.Object) []reconcile.Request {
	var apps appv1beta1.ApplicationList
	if err := r.List(ctx, &apps, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	compLabels := labels.Set(obj.GetLabels())
	var reqs []reconcile.Request
	for i := range apps.Items {
		app := &apps.Items[i]
		if app.Spec.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(app.Spec.Selector)
		if err != nil {
			continue
		}
		if sel.Matches(compLabels) {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name},
			})
		}
	}
	return reqs
}
