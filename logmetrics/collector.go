// Package logmetrics implements the sending side of the log-based metrics pipeline
// (docs/adr/0006-log-based-metrics.md): a periodic loop that discovers the Fluent Bit
// DaemonSet's exporter endpoints (chart-templated, "triage-fluentbit"), scrapes each
// pod's Prometheus text-format /metrics, sums error/warn/total log-line counters per
// (namespace, service) label pair, computes a delta against the previous scrape, and
// sends a log_metrics frame (push/protocol.go) through the existing Pusher for
// services whose error delta trips a threshold. Off by default; a no-op when push
// mode itself is disabled, since the frame has nowhere to go.
package logmetrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/application/push"
)

// Sender is the subset of *push.Pusher the collector needs. A small interface (rather
// than importing *push.Pusher directly into every call site) so tests can fake it, and
// so a nil *push.Pusher (push mode disabled) satisfies it via a plain nil interface
// check in New.
type Sender interface {
	SendLogMetrics(windowStart int64, windowSec int, services []push.ServiceLogMetrics)
}

// Options configures the Collector. All fields have defaults applied in New except
// ServiceNamespace/ServiceName, which are required.
type Options struct {
	// ServiceNamespace/ServiceName identify the Service fronting the Fluent Bit
	// DaemonSet pods (chart default: "triage-fluentbit" in the release namespace).
	// The collector reads its Endpoints (not the Service) to get pod IPs directly.
	ServiceNamespace string
	ServiceName      string
	// Port is the Fluent Bit prometheus_exporter port on each pod. Default 2021.
	Port int
	// Interval is how often the collector scrapes + evaluates the gate. Default 60s.
	Interval time.Duration
	// ScrapeTimeout bounds each per-pod HTTP GET. Default 5s.
	ScrapeTimeout time.Duration
	// ErrorThreshold is the minimum errorDelta for a service to qualify for the
	// frame. Default 10.
	ErrorThreshold int64
	// Metric family names as emitted by Fluent Bit's log_to_metrics filter — these
	// default to the *_counter_* names produced by metric_namespace=log_metric,
	// metric_mode=counter (see charts/app-controller's fluentbit ConfigMap and
	// docs/adr/0006-log-based-metrics.md for how the emitted name is derived).
	// ErrorMetric is required; Warn/TotalMetric are optional — an absent family in a
	// scrape is simply skipped, not an error.
	ErrorMetric string
	WarnMetric  string
	TotalMetric string
	// Label keys the collector reads off each Prometheus sample to identify the
	// service. Defaults "namespace" and "service".
	NamespaceLabel string
	ServiceLabel   string
	// ServiceLabelFallback, if set, is a second label key consulted when
	// ServiceLabel is absent on a sample (e.g. the chart's fluentbit pipeline emits
	// both "service" from app.kubernetes.io/name and "service_fallback" from the
	// plain "app" label, for pods that only set the older convention). Default
	// "service_fallback"; set empty to disable the fallback lookup entirely.
	ServiceLabelFallback string
	// SampleLabel, if set, is the label key holding a representative log line
	// (Fluent Bit does not emit this by default; reserved for a future filter that
	// does). Empty means no Sample is populated.
	SampleLabel string

	// httpClient is injectable in tests. Defaults to a client with ScrapeTimeout.
	httpClient *http.Client
	// now is injectable in tests. Defaults to time.Now.
	now func() time.Time
}

func (o *Options) setDefaults() {
	if o.Port <= 0 {
		o.Port = 2021
	}
	if o.Interval <= 0 {
		o.Interval = 60 * time.Second
	}
	if o.ScrapeTimeout <= 0 {
		o.ScrapeTimeout = 5 * time.Second
	}
	if o.ErrorThreshold <= 0 {
		o.ErrorThreshold = 10
	}
	if o.ErrorMetric == "" {
		o.ErrorMetric = "log_metric_counter_log_errors_total"
	}
	if o.WarnMetric == "" {
		o.WarnMetric = "log_metric_counter_log_warns_total"
	}
	if o.TotalMetric == "" {
		o.TotalMetric = "log_metric_counter_log_lines_total"
	}
	if o.NamespaceLabel == "" {
		o.NamespaceLabel = "namespace"
	}
	if o.ServiceLabel == "" {
		o.ServiceLabel = "service"
	}
	if o.ServiceLabelFallback == "" {
		o.ServiceLabelFallback = "service_fallback"
	}
	if o.httpClient == nil {
		o.httpClient = &http.Client{Timeout: o.ScrapeTimeout}
	}
	if o.now == nil {
		o.now = time.Now
	}
}

// Collector is a manager.Runnable that periodically scrapes Fluent Bit exporter
// endpoints and forwards qualifying services to a Sender. Gated behind leader
// election so only one replica scrapes + sends.
type Collector struct {
	opts   Options
	client client.Reader
	sender Sender
	log    logr.Logger

	// prev holds the last-seen cumulative counters per (endpoint, servicePairKey),
	// so a delta survives individual pod restarts/scaling independently — see
	// computeDeltas.
	prev map[string]endpointCounters
}

// endpointCounters is one endpoint's last scrape: family -> servicePairKey -> value.
type endpointCounters map[string]map[servicePair]float64

type servicePair struct {
	Namespace string
	Service   string
}

// New creates a Collector. Returns nil if opts.ServiceName is empty (collector
// disabled) or sender is nil (push mode disabled — nothing to send to).
func New(opts Options, mgr manager.Manager, sender Sender, log logr.Logger) *Collector {
	if opts.ServiceName == "" || sender == nil {
		return nil
	}
	opts.setDefaults()
	return &Collector{
		opts:   opts,
		client: mgr.GetAPIReader(),
		sender: sender,
		log:    log.WithName("logmetrics"),
		prev:   make(map[string]endpointCounters),
	}
}

// NeedLeaderElection ensures only the leader replica scrapes + sends.
func (c *Collector) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable: scrape on a fixed interval until ctx ends.
func (c *Collector) Start(ctx context.Context) error {
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

// runOnce discovers endpoints, scrapes them, aggregates deltas, applies the gate, and
// sends. Errors from individual endpoint scrapes are logged and skipped — one dead
// Fluent Bit pod must not block the others' counters from being reported.
func (c *Collector) runOnce(ctx context.Context) {
	windowStart := c.opts.now()
	targets, err := c.discoverEndpoints(ctx)
	if err != nil {
		c.log.Error(err, "discover fluent-bit endpoints failed")
		return
	}
	if len(targets) == 0 {
		c.log.V(1).Info("no fluent-bit endpoints found; skipping scrape",
			"service", c.opts.ServiceNamespace+"/"+c.opts.ServiceName)
		return
	}

	totals := make(map[servicePair]*push.ServiceLogMetrics)
	for _, addr := range targets {
		fams, err := c.scrape(ctx, addr)
		if err != nil {
			c.log.V(1).Info("scrape failed; skipping endpoint", "addr", addr, "reason", err.Error())
			continue
		}
		deltas := c.computeDeltas(addr, fams)
		for pair, d := range deltas {
			agg := totals[pair]
			if agg == nil {
				agg = &push.ServiceLogMetrics{Namespace: pair.Namespace, Service: pair.Service}
				totals[pair] = agg
			}
			agg.ErrorCount += d.errorDelta
			agg.WarnCount += d.warnDelta
			agg.TotalCount += d.totalDelta
			if d.sample != "" && agg.Sample == "" {
				agg.Sample = push.TruncateSample(d.sample)
			}
		}
	}

	var qualifying []push.ServiceLogMetrics
	unattributed := int64(0)
	for _, m := range totals {
		if m.ErrorCount < c.opts.ErrorThreshold {
			continue
		}
		// Pods with neither service label leave Service empty. Skip them: the
		// receiver rejects the whole frame over one empty entry, and an
		// unattributable service could never resolve in the hub's graph anyway.
		if m.Service == "" {
			unattributed += m.ErrorCount
			continue
		}
		qualifying = append(qualifying, *m)
	}
	if unattributed > 0 {
		c.log.Info("skipped unattributable error logs (pods without a service label)",
			"errorCount", unattributed)
	}
	if len(qualifying) == 0 {
		c.log.V(1).Info("no services crossed the error threshold; no frame sent",
			"threshold", c.opts.ErrorThreshold, "servicesScraped", len(totals))
		return
	}

	windowSec := int(c.opts.Interval / time.Second)
	c.sender.SendLogMetrics(windowStart.Unix(), windowSec, qualifying)
	c.log.Info("sent log_metrics frame", "services", len(qualifying), "windowSec", windowSec)
}

// discoverEndpoints lists the configured Service's Endpoints and returns
// "ip:port" targets for every ready address across all subsets. Each subset carries
// its own Ports list (from the Service spec); we use the subset's port when the
// subset exposes exactly one, and otherwise fall back to opts.Port — the DaemonSet's
// Service in the chart always exposes exactly one (the exporter) port, so the normal
// path is subset-provided, and opts.Port exists mainly as a safety net / test seam.
func (c *Collector) discoverEndpoints(ctx context.Context) ([]string, error) {
	var ep corev1.Endpoints
	key := types.NamespacedName{Namespace: c.opts.ServiceNamespace, Name: c.opts.ServiceName}
	if err := c.client.Get(ctx, key, &ep); err != nil {
		return nil, fmt.Errorf("get endpoints %s: %w", key, err)
	}
	var out []string
	for _, subset := range ep.Subsets {
		port := c.opts.Port
		if len(subset.Ports) == 1 {
			port = int(subset.Ports[0].Port)
		}
		for _, addr := range subset.Addresses {
			out = append(out, net.JoinHostPort(addr.IP, strconv.Itoa(port)))
		}
	}
	return out, nil
}

// scrape fetches and parses one endpoint's Prometheus text-format /metrics.
func (c *Collector) scrape(ctx context.Context, addr string) (map[string]*dto.MetricFamily, error) {
	url := fmt.Sprintf("http://%s/metrics", addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.opts.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	fams, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}
	return fams, nil
}
