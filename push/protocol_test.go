package push

import (
	"fmt"
	"strings"
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
		newLogMetrics("owl", 1720000000, 60, []ServiceLogMetrics{
			{Namespace: "ops", Service: "api", ErrorCount: 42, WarnCount: 7, TotalCount: 1000, Sample: "panic: nil pointer"},
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

// TestLogMetricsFrame guards the log_metrics wire contract: field names/values
// (windowStart, windowSec, services[].{namespace,service,errorCount,warnCount,
// totalCount,sample}) must survive marshal→unmarshal exactly, and the JSON keys must
// match the receiver's LogMetricsPayload (internal/remoteagent/protocol.go).
func TestLogMetricsFrame(t *testing.T) {
	f := newLogMetrics("owl", 1720000000, 60, []ServiceLogMetrics{
		{Namespace: "ops", Service: "api", ErrorCount: 42, WarnCount: 7, TotalCount: 1000, Sample: "panic: nil pointer"},
		{Namespace: "ops", Service: "web", ErrorCount: 11},
	})
	data, err := encode(f)
	if err != nil {
		t.Fatal(err)
	}

	// Exact JSON field names, since these are the wire contract with a separate repo.
	raw := string(data)
	for _, want := range []string{
		`"kind":"log_metrics"`,
		`"windowStart":1720000000`,
		`"windowSec":60`,
		`"namespace":"ops"`,
		`"service":"api"`,
		`"errorCount":42`,
		`"warnCount":7`,
		`"totalCount":1000`,
		`"sample":"panic: nil pointer"`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("encoded frame missing %q; got %s", want, raw)
		}
	}
	// warnCount/totalCount are omitempty — the second service (zero warn/total) must
	// not carry them, so the receiver's zero-value defaults apply, not an explicit 0
	// that would look identical on decode but wastes wire bytes at scale.
	if strings.Contains(raw, `"service":"web","errorCount":11,"warnCount"`) {
		t.Errorf("omitempty fields present on zero-value service: %s", raw)
	}

	got, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.LogMetrics == nil {
		t.Fatal("LogMetrics payload missing after round-trip")
	}
	if got.LogMetrics.WindowStart != 1720000000 || got.LogMetrics.WindowSec != 60 {
		t.Errorf("window = (%d, %d), want (1720000000, 60)", got.LogMetrics.WindowStart, got.LogMetrics.WindowSec)
	}
	if len(got.LogMetrics.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(got.LogMetrics.Services))
	}
	s0 := got.LogMetrics.Services[0]
	if s0.Namespace != "ops" || s0.Service != "api" || s0.ErrorCount != 42 || s0.WarnCount != 7 || s0.TotalCount != 1000 || s0.Sample != "panic: nil pointer" {
		t.Errorf("service[0] round-trip mismatch: %+v", s0)
	}
}

func TestTruncateSample(t *testing.T) {
	short := "short sample"
	if got := TruncateSample(short); got != short {
		t.Errorf("TruncateSample(%q) = %q, want unchanged", short, got)
	}
	long := strings.Repeat("x", 300)
	got := TruncateSample(long)
	if len(got) != MaxLogMetricsSampleLen {
		t.Errorf("TruncateSample length = %d, want %d", len(got), MaxLogMetricsSampleLen)
	}
}

func TestChunkServices(t *testing.T) {
	if chunkServices(nil) != nil {
		t.Errorf("chunkServices(nil) should be nil")
	}
	services := make([]ServiceLogMetrics, 250)
	for i := range services {
		services[i] = ServiceLogMetrics{Namespace: "ops", Service: fmt.Sprintf("svc-%d", i)}
	}
	chunks := chunkServices(services)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (100+100+50)", len(chunks))
	}
	if len(chunks[0]) != MaxLogMetricsServices || len(chunks[1]) != MaxLogMetricsServices || len(chunks[2]) != 50 {
		t.Errorf("chunk sizes = %d, %d, %d, want 100, 100, 50", len(chunks[0]), len(chunks[1]), len(chunks[2]))
	}
	// every service must appear exactly once across chunks
	seen := make(map[string]bool)
	for _, c := range chunks {
		for _, s := range c {
			if seen[s.Service] {
				t.Errorf("service %q appears in more than one chunk", s.Service)
			}
			seen[s.Service] = true
		}
	}
	if len(seen) != 250 {
		t.Errorf("total unique services across chunks = %d, want 250", len(seen))
	}
}
