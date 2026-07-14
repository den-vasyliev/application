package logmetrics

import dto "github.com/prometheus/client_model/go"

// delta is one (namespace, service) pair's counter movement for one endpoint's scrape,
// relative to that same endpoint's previous scrape.
type delta struct {
	errorDelta int64
	warnDelta  int64
	totalDelta int64
	sample     string // opts.SampleLabel's value on the error sample, if configured
}

// computeDeltas extracts counters for opts.ErrorMetric/WarnMetric/TotalMetric from a
// single endpoint's parsed families, aggregates them per (namespace, service) label
// pair, and returns the delta against that same endpoint's previous scrape.
//
// Deltas are computed per endpoint (keyed by addr), not globally, so that:
//   - a pod restart resetting its counters to 0 produces a negative raw delta, which
//     is treated as "current value is the delta" (the counter's lifetime-since-restart
//     total), instead of silently going negative or being dropped;
//   - one endpoint scaling in/out never perturbs another endpoint's baseline.
//
// The current scrape's absolute values replace the stored snapshot unconditionally
// (whether this was a reset or a normal increment), so the next call always diffs
// against exactly this scrape.
func (c *Collector) computeDeltas(addr string, fams map[string]*dto.MetricFamily) map[servicePair]delta {
	cur := endpointCounters{
		c.opts.ErrorMetric: c.sumByServicePair(fams[c.opts.ErrorMetric]),
	}
	if c.opts.WarnMetric != "" {
		cur[c.opts.WarnMetric] = c.sumByServicePair(fams[c.opts.WarnMetric])
	}
	if c.opts.TotalMetric != "" {
		cur[c.opts.TotalMetric] = c.sumByServicePair(fams[c.opts.TotalMetric])
	}

	prev := c.prev[addr]
	out := make(map[servicePair]delta)
	mergeMetric := func(metricName string, assign func(*delta, int64)) {
		for pair, v := range cur[metricName] {
			d := out[pair]
			assign(&d, counterDelta(prev[metricName][pair], v))
			out[pair] = d
		}
	}
	mergeMetric(c.opts.ErrorMetric, func(d *delta, v int64) { d.errorDelta = v })
	if c.opts.WarnMetric != "" {
		mergeMetric(c.opts.WarnMetric, func(d *delta, v int64) { d.warnDelta = v })
	}
	if c.opts.TotalMetric != "" {
		mergeMetric(c.opts.TotalMetric, func(d *delta, v int64) { d.totalDelta = v })
	}

	if c.opts.SampleLabel != "" {
		for pair, sample := range c.sampleByServicePair(fams[c.opts.ErrorMetric]) {
			d := out[pair]
			d.sample = sample
			out[pair] = d
		}
	}

	c.prev[addr] = cur
	return out
}

// sampleByServicePair returns, per (namespace, service) pair, the first non-empty
// opts.SampleLabel value found on the error family's samples for that pair. Fluent
// Bit's log_to_metrics filter does not emit such a label by default (see
// docs/adr/0006-log-based-metrics.md) — this only produces output when SampleLabel is
// explicitly configured to a label a custom pipeline adds.
func (c *Collector) sampleByServicePair(fam *dto.MetricFamily) map[servicePair]string {
	out := make(map[servicePair]string)
	if fam == nil {
		return out
	}
	for _, m := range fam.GetMetric() {
		pair := servicePair{
			Namespace: labelValue(m, c.opts.NamespaceLabel),
			Service:   c.serviceOf(m),
		}
		if _, seen := out[pair]; seen {
			continue
		}
		if v := labelValue(m, c.opts.SampleLabel); v != "" {
			out[pair] = v
		}
	}
	return out
}

// serviceOf returns the sample's service identity: opts.ServiceLabel if present,
// otherwise opts.ServiceLabelFallback (e.g. the chart's fluentbit pipeline emits
// "service" from app.kubernetes.io/name and "service_fallback" from the plain "app"
// label — pods only setting the older convention still get attributed correctly).
func (c *Collector) serviceOf(m *dto.Metric) string {
	if v := labelValue(m, c.opts.ServiceLabel); v != "" {
		return v
	}
	if c.opts.ServiceLabelFallback != "" {
		return labelValue(m, c.opts.ServiceLabelFallback)
	}
	return ""
}

// counterDelta returns cur-prev, except when that would be negative (a counter reset
// — Fluent Bit pod restart, or the series is new this scrape with prev==0 already
// handled by the zero value) in which case cur itself is treated as the delta: the
// counter has been accumulating since the reset, so its current value IS the count of
// events since then, none of which were reported yet.
func counterDelta(prev, cur float64) int64 {
	d := cur - prev
	if d < 0 {
		d = cur
	}
	return int64(d)
}

// sumByServicePair sums every sample in a metric family by its (namespace, service)
// label pair, using the collector's configured label keys. A family may be nil (the
// metric name wasn't present in this scrape — e.g. WarnMetric/TotalMetric are
// optional), in which case an empty map is returned.
func (c *Collector) sumByServicePair(fam *dto.MetricFamily) map[servicePair]float64 {
	out := make(map[servicePair]float64)
	if fam == nil {
		return out
	}
	for _, m := range fam.GetMetric() {
		pair := servicePair{
			Namespace: labelValue(m, c.opts.NamespaceLabel),
			Service:   c.serviceOf(m),
		}
		if pair.Namespace == "" && pair.Service == "" {
			continue // sample carries neither identifying label; nothing to attribute it to
		}
		out[pair] += sampleValue(m)
	}
	return out
}

// labelValue returns the value of the named label on a sample, or "" if absent.
func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// sampleValue extracts the numeric value regardless of the family's declared type —
// log_to_metrics in counter mode emits Counter samples, but this stays defensive
// against a family configured as a Gauge upstream (e.g. a hand-edited pipeline).
func sampleValue(m *dto.Metric) float64 {
	switch {
	case m.Counter != nil:
		return m.GetCounter().GetValue()
	case m.Gauge != nil:
		return m.GetGauge().GetValue()
	case m.Untyped != nil:
		return m.GetUntyped().GetValue()
	default:
		return 0
	}
}
