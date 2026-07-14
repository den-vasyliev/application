package logmetrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/application/push"
)

// fakeSender records every SendLogMetrics call for assertions.
type fakeSender struct {
	mu    sync.Mutex
	calls [][]push.ServiceLogMetrics
}

func (f *fakeSender) SendLogMetrics(_ int64, _ int, services []push.ServiceLogMetrics) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]push.ServiceLogMetrics(nil), services...)
	f.calls = append(f.calls, cp)
}

func (f *fakeSender) allServices() []push.ServiceLogMetrics {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []push.ServiceLogMetrics
	for _, c := range f.calls {
		out = append(out, c...)
	}
	return out
}

func (f *fakeSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// metricsServer starts an httptest server that serves a mutable Prometheus text-format
// body, so a test can drive multiple scrapes with different counter values.
type metricsServer struct {
	mu   sync.Mutex
	body string
	srv  *httptest.Server
}

func newMetricsServer(t *testing.T, initial string) *metricsServer {
	t.Helper()
	m := &metricsServer{body: initial}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(m.body))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *metricsServer) set(body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.body = body
}

// hostPort returns the loopback host and port the test server listens on.
func (m *metricsServer) hostPort(t *testing.T) (string, int) {
	t.Helper()
	// m.srv.URL is "http://127.0.0.1:PORT"; Listener.Addr() gives it to us structured.
	addr := m.srv.Listener.Addr().String() // "127.0.0.1:PORT"
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		t.Fatalf("unexpected listener addr %q", addr)
	}
	port, err := strconv.Atoi(addr[idx+1:])
	if err != nil {
		t.Fatalf("parse port from %q: %v", addr, err)
	}
	return addr[:idx], port
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// newTestCollector builds a Collector wired to a fake client seeded with one
// Endpoints object — one subset per server, each carrying that server's own port, so
// discoverEndpoints (which prefers a subset's own single port) resolves correctly
// even when servers listen on different loopback ports. sender is a *fakeSender the
// test can inspect.
func newTestCollector(t *testing.T, servers []*metricsServer, opts Options) (*Collector, *fakeSender) {
	t.Helper()
	scheme := newTestScheme(t)

	var subsets []corev1.EndpointSubset
	for _, s := range servers {
		host, port := s.hostPort(t)
		subsets = append(subsets, corev1.EndpointSubset{
			Addresses: []corev1.EndpointAddress{{IP: host}},
			Ports:     []corev1.EndpointPort{{Port: int32(port)}},
		})
	}
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "triage-fluentbit", Namespace: "triageagent"},
		Subsets:    subsets,
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).Build()
	sender := &fakeSender{}

	opts.ServiceNamespace = "triageagent"
	opts.ServiceName = "triage-fluentbit"
	opts.setDefaults()

	c := &Collector{
		opts:   opts,
		client: fc,
		sender: sender,
		log:    logr.Discard(),
		prev:   make(map[string]endpointCounters),
	}
	return c, sender
}

const promErrCounter = `# HELP log_metric_counter_log_errors_total error log lines
# TYPE log_metric_counter_log_errors_total counter
log_metric_counter_log_errors_total{namespace="ops",service="api"} %d
`

const promErrWarnTotal = `# HELP log_metric_counter_log_errors_total error log lines
# TYPE log_metric_counter_log_errors_total counter
log_metric_counter_log_errors_total{namespace="ops",service="api"} %d
# HELP log_metric_counter_log_warns_total warn log lines
# TYPE log_metric_counter_log_warns_total counter
log_metric_counter_log_warns_total{namespace="ops",service="api"} %d
# HELP log_metric_counter_log_lines_total all log lines
# TYPE log_metric_counter_log_lines_total counter
log_metric_counter_log_lines_total{namespace="ops",service="api"} %d
`

// TestCollector_DeltaAcrossTwoScrapes verifies the second scrape's delta is
// (cur-prev), not the raw cumulative value, and that it reaches the Sender once it
// crosses the error threshold.
func TestCollector_DeltaAcrossTwoScrapes(t *testing.T) {
	ms := newMetricsServer(t, fmt.Sprintf(promErrCounter, 5))
	c, sender := newTestCollector(t, []*metricsServer{ms}, Options{ErrorThreshold: 10})

	ctx := context.Background()
	c.runOnce(ctx) // first scrape: cumulative=5, delta=5 (below threshold) -> no send
	if sender.callCount() != 0 {
		t.Fatalf("after first scrape (delta=5 < threshold=10): callCount = %d, want 0", sender.callCount())
	}

	ms.set(fmt.Sprintf(promErrCounter, 20)) // cumulative 5 -> 20, delta = 15
	c.runOnce(ctx)
	svcs := sender.allServices()
	if len(svcs) != 1 {
		t.Fatalf("services sent = %d, want 1", len(svcs))
	}
	if svcs[0].Namespace != "ops" || svcs[0].Service != "api" || svcs[0].ErrorCount != 15 {
		t.Errorf("service = %+v, want {ops api errorCount=15}", svcs[0])
	}
}

// TestCollector_CounterReset verifies a Fluent Bit pod restart (counter drops instead
// of increasing) is treated as "current value is the delta" rather than a negative
// delta or a dropped update.
func TestCollector_CounterReset(t *testing.T) {
	ms := newMetricsServer(t, fmt.Sprintf(promErrCounter, 100))
	c, sender := newTestCollector(t, []*metricsServer{ms}, Options{ErrorThreshold: 10})

	ctx := context.Background()
	c.runOnce(ctx) // cumulative=100, delta=100 -> sent (>= threshold 10)
	if sender.callCount() != 1 {
		t.Fatalf("after first scrape: callCount = %d, want 1", sender.callCount())
	}

	ms.set(fmt.Sprintf(promErrCounter, 12)) // pod restarted: counter reset to 12 (< 100)
	c.runOnce(ctx)
	svcs := sender.allServices()
	last := svcs[len(svcs)-1]
	if last.ErrorCount != 12 {
		t.Errorf("post-reset delta = %d, want 12 (treat current value as the delta, not cur-prev=-88)", last.ErrorCount)
	}
}

// TestCollector_GateBelowThresholdNoSend verifies a service whose error delta never
// crosses the threshold produces no frame at all — not a frame with 0 services, no
// frame.
// TestCollector_SkipsEmptyServiceEntries verifies error series from pods without
// any service label (service and fallback both empty) are excluded from the frame
// even when they cross the threshold — the receiver rejects a whole frame over one
// empty-service entry, and an unattributable service can't resolve in the hub's
// graph anyway. Attributed services in the same window must still be sent.
func TestCollector_SkipsEmptyServiceEntries(t *testing.T) {
	body := `# HELP log_metric_counter_log_errors_total error log lines
# TYPE log_metric_counter_log_errors_total counter
log_metric_counter_log_errors_total{namespace="ops",service="api"} 25
log_metric_counter_log_errors_total{namespace="ops",service="",service_fallback=""} 40
`
	ms := newMetricsServer(t, body)
	c, sender := newTestCollector(t, []*metricsServer{ms}, Options{ErrorThreshold: 10})

	c.runOnce(context.Background())
	svcs := sender.allServices()
	if len(svcs) != 1 {
		t.Fatalf("services = %d, want 1 (empty-service entry must be skipped)", len(svcs))
	}
	if svcs[0].Service != "api" || svcs[0].ErrorCount != 25 {
		t.Errorf("service = %+v, want api/25", svcs[0])
	}
}

func TestCollector_GateBelowThresholdNoSend(t *testing.T) {
	ms := newMetricsServer(t, fmt.Sprintf(promErrCounter, 3))
	c, sender := newTestCollector(t, []*metricsServer{ms}, Options{ErrorThreshold: 10})

	c.runOnce(context.Background())
	if sender.callCount() != 0 {
		t.Fatalf("callCount = %d, want 0 (delta=3 < threshold=10)", sender.callCount())
	}
}

// TestCollector_WarnAndTotalOptional verifies warn/total counters are aggregated
// alongside errors when present, and that an absent family (warn/total metric name
// not in the scrape) does not error — it's simply zero.
func TestCollector_WarnAndTotalOptional(t *testing.T) {
	ms := newMetricsServer(t, fmt.Sprintf(promErrWarnTotal, 20, 8, 500))
	c, sender := newTestCollector(t, []*metricsServer{ms}, Options{ErrorThreshold: 10})

	c.runOnce(context.Background())
	svcs := sender.allServices()
	if len(svcs) != 1 {
		t.Fatalf("services = %d, want 1", len(svcs))
	}
	if svcs[0].ErrorCount != 20 || svcs[0].WarnCount != 8 || svcs[0].TotalCount != 500 {
		t.Errorf("service = %+v, want errorCount=20 warnCount=8 totalCount=500", svcs[0])
	}
}

// TestCollector_AggregatesAcrossEndpoints verifies two Fluent Bit pods (two
// endpoints) reporting the same (namespace, service) label pair are summed into one
// entry in the frame, not sent as two separate entries.
func TestCollector_AggregatesAcrossEndpoints(t *testing.T) {
	ms1 := newMetricsServer(t, fmt.Sprintf(promErrCounter, 6))
	ms2 := newMetricsServer(t, fmt.Sprintf(promErrCounter, 7))
	c, sender := newTestCollector(t, []*metricsServer{ms1, ms2}, Options{ErrorThreshold: 10})

	c.runOnce(context.Background())
	svcs := sender.allServices()
	if len(svcs) != 1 {
		t.Fatalf("services = %d, want 1 (aggregated across 2 endpoints)", len(svcs))
	}
	if svcs[0].ErrorCount != 13 { // 6 + 7, both endpoints' first-scrape cumulative == delta
		t.Errorf("aggregated errorCount = %d, want 13 (6 from endpoint 1 + 7 from endpoint 2)", svcs[0].ErrorCount)
	}
}

// TestCollector_PerEndpointBaseline verifies deltas are computed per endpoint, not
// globally: one endpoint's counter climbing does not get diffed against a different
// endpoint's previous value.
func TestCollector_PerEndpointBaseline(t *testing.T) {
	ms1 := newMetricsServer(t, fmt.Sprintf(promErrCounter, 50)) // high baseline
	ms2 := newMetricsServer(t, fmt.Sprintf(promErrCounter, 2))  // low baseline
	c, sender := newTestCollector(t, []*metricsServer{ms1, ms2}, Options{ErrorThreshold: 10})

	c.runOnce(context.Background()) // seeds both baselines; first scrape sends (50+2=52 >= 10)
	sender.mu.Lock()
	sender.calls = nil // reset for the assertion below
	sender.mu.Unlock()

	ms1.set(fmt.Sprintf(promErrCounter, 55)) // +5
	ms2.set(fmt.Sprintf(promErrCounter, 3))  // +1
	c.runOnce(context.Background())

	svcs := sender.allServices()
	if len(svcs) != 0 {
		// combined delta 5+1=6 < threshold 10: correctly gated out. If endpoint
		// baselines were mixed up (e.g. globally keyed), the delta could be
		// miscomputed as negative-then-reset and wrongly appear large.
		t.Fatalf("services sent = %d, want 0 (combined delta 6 < threshold 10); got %+v", len(svcs), svcs)
	}
}

const promErrCounterFallbackLabel = `# HELP log_metric_counter_log_errors_total error log lines
# TYPE log_metric_counter_log_errors_total counter
log_metric_counter_log_errors_total{namespace="ops",service_fallback="legacy-api"} %d
`

// TestCollector_ServiceLabelFallback verifies a sample missing the primary
// ServiceLabel ("service") but carrying ServiceLabelFallback ("service_fallback")
// still gets attributed to that service, matching pods that only set the older
// "app" label convention (see fluentbit.pipeline.serviceLabelFallback in the chart).
func TestCollector_ServiceLabelFallback(t *testing.T) {
	ms := newMetricsServer(t, fmt.Sprintf(promErrCounterFallbackLabel, 15))
	c, sender := newTestCollector(t, []*metricsServer{ms}, Options{ErrorThreshold: 10})

	c.runOnce(context.Background())
	svcs := sender.allServices()
	if len(svcs) != 1 {
		t.Fatalf("services = %d, want 1", len(svcs))
	}
	if svcs[0].Service != "legacy-api" {
		t.Errorf("service = %q, want %q (from the fallback label)", svcs[0].Service, "legacy-api")
	}
}

// TestCollector_NoEndpointsSkipsScrape verifies an empty Endpoints object (Service
// exists, no ready pods yet) is handled gracefully — no panic, no send.
func TestCollector_NoEndpointsSkipsScrape(t *testing.T) {
	scheme := newTestScheme(t)
	ep := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "triage-fluentbit", Namespace: "triageagent"}}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ep).Build()
	sender := &fakeSender{}
	opts := Options{ServiceNamespace: "triageagent", ServiceName: "triage-fluentbit", ErrorThreshold: 10}
	opts.setDefaults()
	c := &Collector{opts: opts, client: fc, sender: sender, log: logr.Discard(), prev: make(map[string]endpointCounters)}

	c.runOnce(context.Background())
	if sender.callCount() != 0 {
		t.Errorf("callCount = %d, want 0 (no endpoints)", sender.callCount())
	}
}

// TestNew_DisabledWithoutServiceName verifies New returns nil (collector disabled)
// when ServiceName is unset, so main.go's unconditional wiring is a no-op by default.
func TestNew_DisabledWithoutServiceName(t *testing.T) {
	if c := New(Options{}, nil, &fakeSender{}, logr.Discard()); c != nil {
		t.Errorf("New with empty ServiceName = %v, want nil", c)
	}
}

// TestNew_DisabledWithoutSender verifies New returns nil when push mode itself is
// disabled (sender is nil, e.g. because *push.Pusher was nil).
func TestNew_DisabledWithoutSender(t *testing.T) {
	if c := New(Options{ServiceName: "triage-fluentbit"}, nil, nil, logr.Discard()); c != nil {
		t.Errorf("New with nil sender = %v, want nil", c)
	}
}

// TestTypedNilPusherIsNotNilInterface is a regression guard for Go's typed-nil-
// interface trap, which main.go must route around: a nil *push.Pusher (push.New
// returns nil when --push-endpoint is empty) stored DIRECTLY into a Sender interface
// variable does NOT compare equal to nil, because the interface value still carries
// the concrete type *push.Pusher — only the pointer inside it is nil. If New relied on
// `sender == nil` alone to detect "push mode disabled," a directly-passed nil
// *push.Pusher would slip through and the collector would start scraping with
// nowhere to send. main.go avoids this with an explicit
// `var sender logmetrics.Sender; if pusher != nil { sender = pusher }` guard rather
// than passing the pusher variable straight through. This test exists so that guard
// doesn't get "simplified" away by someone who assumes sender == nil is sufficient.
func TestTypedNilPusherIsNotNilInterface(t *testing.T) {
	var nilPusher *push.Pusher // exactly what push.New returns when disabled
	var sender Sender = nilPusher
	if sender == nil {
		t.Fatal("a *push.Pusher stored in a Sender interface compared equal to nil — " +
			"if this now passes, Go's typed-nil-interface behavior changed and main.go's " +
			"explicit-nil guard before calling logmetrics.New may no longer be necessary")
	}
}
