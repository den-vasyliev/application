// Copyright 2024 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
	"sigs.k8s.io/application/controllers"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	cfg        *rest.Config
	k8sClient  client.Client
	testEnv    *envtest.Environment
	ctx        context.Context
	cancelFunc context.CancelFunc
)

func TestE2e(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Application E2E Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("bootstrapping test environment")
	useExisting := false
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd", "bases"),
			"testdata",
		},
		UseExistingCluster: &useExisting,
	}

	var err error
	err = appv1beta1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	cfg, err = testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(cfg).ToNot(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).ToNot(HaveOccurred())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	err = (&controllers.ApplicationReconciler{
		Client: mgr.GetClient(),
		Mapper: mgr.GetRESTMapper(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	ctx, cancelFunc = context.WithCancel(context.Background())
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).NotTo(HaveOccurred())
	}()
})

var _ = AfterSuite(func() {
	cancelFunc()
	By("tearing down the test environment")
	Expect(testEnv.Stop()).NotTo(HaveOccurred())
})

var _ = Describe("Application with custom CRD components", func() {
	const (
		testNamespace = "default"
		timeout       = 30 * time.Second
		interval      = time.Second
	)

	var testCRDGVR = schema.GroupVersionResource{
		Group:    "test.crd.com",
		Version:  "v1",
		Resource: "testcrds",
	}

	It("should track a custom CRD instance in Application status", func() {
		labels := map[string]string{"app": "test-custom"}

		// Create a TestCRD instance
		testCR := &unstructured.Unstructured{}
		testCR.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   testCRDGVR.Group,
			Version: testCRDGVR.Version,
			Kind:    "TestCRD",
		})
		testCR.SetName("my-testcrd")
		testCR.SetNamespace(testNamespace)
		testCR.SetLabels(labels)
		Expect(k8sClient.Create(ctx, testCR)).NotTo(HaveOccurred())

		// Patch TestCRD status with a Ready=True condition so statusFromStandardConditions returns Ready
		statusPatch := map[string]interface{}{
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   "Ready",
						"status": "True",
						"reason": "TestReady",
					},
				},
			},
		}
		patchBytes, err := json.Marshal(statusPatch)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Status().Patch(
			ctx, testCR,
			client.RawPatch(types.MergePatchType, patchBytes),
		)).NotTo(HaveOccurred())

		// Create an Application referencing TestCRD components
		app := &appv1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-with-testcrd",
				Namespace: testNamespace,
			},
			Spec: appv1beta1.ApplicationSpec{
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				ComponentGroupKinds: []metav1.GroupKind{
					{Group: "test.crd.com", Kind: "TestCRD"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, app)).NotTo(HaveOccurred())

		// Wait until the Application status lists the TestCRD component
		appKey := types.NamespacedName{Name: app.Name, Namespace: testNamespace}
		err = wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
			if err := k8sClient.Get(ctx, appKey, app); err != nil {
				return false, nil
			}
			if len(app.Status.ComponentList.Objects) == 0 {
				return false, nil
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(app.Status.ComponentList.Objects).To(HaveLen(1))
		Expect(app.Status.ComponentList.Objects[0].Kind).To(Equal("TestCRD"))
		Expect(app.Status.ComponentList.Objects[0].Name).To(Equal("my-testcrd"))
		Expect(app.Status.ComponentList.Objects[0].Status).To(Equal(controllers.StatusReady))
		Expect(app.Status.ComponentsReady).To(Equal(fmt.Sprintf("1/1")))
	})
})
