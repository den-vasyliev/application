package push

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

func TestFrameRoundTrip(t *testing.T) {
	app := &appv1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ops"},
	}
	app.Status.ComponentsReady = "1/1"
	app.Status.Conditions = []appv1beta1.Condition{{Type: "Ready", Status: corev1.ConditionTrue}}

	frames := []*Frame{
		newHello("owl", "newron", "1.3.8", []byte("test-key"), 1720000000, []string{"ops"}),
		newSnapshot("owl", []*appv1beta1.Application{app}),
		newSnapshotEnd("owl"),
		newAppDelta("owl", OpUpdate, app),
		newK8sEvent("owl", &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: "evt", Namespace: "ops"},
			Reason:     "BackOff",
			Type:       corev1.EventTypeWarning,
		}),
		newHeartbeat("owl"),
	}

	for _, f := range frames {
		data, err := encode(f)
		if err != nil {
			t.Fatalf("encode %s: %v", f.Kind, err)
		}
		got, err := decode(data)
		if err != nil {
			t.Fatalf("decode %s: %v", f.Kind, err)
		}
		if got.V != ProtocolVersion || got.Kind != f.Kind || got.Cluster != "owl" {
			t.Errorf("round-trip mismatch for %s: %+v", f.Kind, got)
		}
	}
}

// TestSnapshotPreservesStatus guards the wire-compat contract: the fields triage
// classifies on (ComponentsReady, Ready condition, component objects) survive
// marshal→unmarshal.
func TestSnapshotPreservesStatus(t *testing.T) {
	app := &appv1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ops", Labels: map[string]string{"app.kubernetes.io/name": "api"}},
	}
	app.Status.ComponentsReady = "0/2"
	app.Status.Conditions = []appv1beta1.Condition{{Type: "Ready", Status: corev1.ConditionFalse, Message: "crash loop"}}
	app.Status.Objects = []appv1beta1.ObjectStatus{
		{Kind: "Deployment", Name: "api", Status: "InProgress"},
	}

	data, err := encode(newSnapshot("owl", []*appv1beta1.Application{app}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	ra := got.Snapshot.Apps[0]
	if ra.Status.ComponentsReady != "0/2" {
		t.Errorf("componentsReady = %q", ra.Status.ComponentsReady)
	}
	if len(ra.Status.Conditions) != 1 || ra.Status.Conditions[0].Status != corev1.ConditionFalse {
		t.Errorf("conditions not preserved: %+v", ra.Status.Conditions)
	}
	if len(ra.Status.Objects) != 1 || ra.Status.Objects[0].Kind != "Deployment" {
		t.Errorf("component objects not preserved: %+v", ra.Status.Objects)
	}
}
