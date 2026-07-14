// Package push implements outbound push mode: the controller dials a triage agent
// over an authenticated WebSocket and streams its Application inventory + deltas +
// Kubernetes Warning events + log-derived error/warn metrics, for closed clusters
// that triage cannot reach via the API (see docs/adr/0005-outbound-push-mode.md,
// docs/adr/0006-log-based-metrics.md; triage side ADR-029).
//
// The frame types here MUST stay wire-compatible with the triage receiver's
// internal/remoteagent/protocol.go — they are separate Go modules, so the types are
// duplicated, not imported. Both sides marshal *v1beta1.Application and *corev1.Event;
// their JSON field tags are identical across the relevant k8s/application versions.
package push

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

// ProtocolVersion is the wire protocol major version (must match the receiver).
const ProtocolVersion = 1

// Frame kinds (agent→hub unless noted).
const (
	KindHello       = "hello"
	KindSnapshot    = "snapshot"
	KindSnapshotEnd = "snapshot_end"
	KindAppDelta    = "app_delta"
	KindK8sEvent    = "k8s_event"
	KindLogMetrics  = "log_metrics"
	KindHeartbeat   = "heartbeat"
	KindPong        = "pong" // hub→agent
)

// MaxLogMetricsServices caps the number of ServiceLogMetrics entries in a single
// log_metrics frame. A scrape that qualifies more services than this is split into
// multiple frames (see logmetrics.Collector).
const MaxLogMetricsServices = 100

// MaxLogMetricsSampleLen caps ServiceLogMetrics.Sample; longer samples are
// truncated before send (see logmetrics.Collector).
const MaxLogMetricsSampleLen = 256

// Delta operations for KindAppDelta.
const (
	OpAdd    = "add"
	OpUpdate = "update"
	OpDelete = "delete"
)

// Frame is the on-the-wire envelope. Field names/tags mirror the receiver exactly.
type Frame struct {
	V       int    `json:"v"`
	Kind    string `json:"kind"`
	Cluster string `json:"cluster,omitempty"`

	Hello      *HelloPayload      `json:"hello,omitempty"`
	Snapshot   *SnapshotPayload   `json:"snapshot,omitempty"`
	AppDelta   *AppDeltaPayload   `json:"appDelta,omitempty"`
	K8sEvent   *K8sEventPayload   `json:"k8sEvent,omitempty"`
	LogMetrics *LogMetricsPayload `json:"logMetrics,omitempty"`
}

// HelloPayload is the first frame sent after the WebSocket upgrade. It is
// HMAC-signed: the agent signs (Tenant,ClusterName,Timestamp) with its tenant's key
// so the receiver can verify it holds that key. Tenant selects the service graph on
// the triage side; ClusterName is metadata for source-path stamping. Field names/tags
// MUST match the receiver's HelloPayload exactly.
type HelloPayload struct {
	AgentVersion string   `json:"agentVersion,omitempty"`
	ClusterName  string   `json:"clusterName"`
	Tenant       string   `json:"tenant"`
	Timestamp    int64    `json:"ts"`  // unix seconds; bounds replay
	Signature    string   `json:"sig"` // base64(HMAC-SHA256(key[tenant], canonical))
	Namespaces   []string `json:"namespaces,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// SnapshotPayload carries a (possibly partial) list of Application objects.
type SnapshotPayload struct {
	Apps []*appv1beta1.Application `json:"apps"`
}

// AppDeltaPayload carries a single live Application change.
type AppDeltaPayload struct {
	Op  string                  `json:"op"`
	App *appv1beta1.Application `json:"app"`
}

// K8sEventPayload carries a single Kubernetes event (Warning type expected).
type K8sEventPayload struct {
	Event *corev1.Event `json:"event"`
}

// LogMetricsPayload carries error/warn/total log-line counts, aggregated per
// (namespace, service) over one scrape window, for services whose error delta
// tripped the collector's gate (see logmetrics.Collector). Field names/tags MUST
// match the receiver's LogMetricsPayload exactly (internal/remoteagent/protocol.go).
type LogMetricsPayload struct {
	WindowStart int64               `json:"windowStart"` // unix seconds, window open
	WindowSec   int                 `json:"windowSec"`   // window length in seconds
	Services    []ServiceLogMetrics `json:"services"`
}

// ServiceLogMetrics is one (namespace, service) pair's counters for the window.
type ServiceLogMetrics struct {
	Namespace  string `json:"namespace"`
	Service    string `json:"service"`
	ErrorCount int64  `json:"errorCount"`
	WarnCount  int64  `json:"warnCount,omitempty"`
	TotalCount int64  `json:"totalCount,omitempty"`
	Sample     string `json:"sample,omitempty"` // ≤256 chars, truncated before send
}

// newHello builds a signed hello frame. tenant + key sign (tenant,cluster,ts) so the
// receiver can verify the agent holds the tenant's HMAC key; ts is unix seconds.
// capabilities lets the caller advertise optional features (e.g. "log_metrics") that
// are actually active on this connection, in addition to the always-on baseline.
func newHello(cluster, tenant, agentVersion string, key []byte, ts int64, namespaces []string, extraCapabilities ...string) *Frame {
	caps := append([]string{"applications", "k8s_events"}, extraCapabilities...)
	return &Frame{V: ProtocolVersion, Kind: KindHello, Cluster: cluster, Hello: &HelloPayload{
		AgentVersion: agentVersion,
		ClusterName:  cluster,
		Tenant:       tenant,
		Timestamp:    ts,
		Signature:    signHandshake(key, tenant, cluster, ts),
		Namespaces:   namespaces,
		Capabilities: caps,
	}}
}

// newSnapshot builds a snapshot frame for a chunk of apps.
func newSnapshot(cluster string, apps []*appv1beta1.Application) *Frame {
	return &Frame{V: ProtocolVersion, Kind: KindSnapshot, Cluster: cluster, Snapshot: &SnapshotPayload{Apps: apps}}
}

func newSnapshotEnd(cluster string) *Frame {
	return &Frame{V: ProtocolVersion, Kind: KindSnapshotEnd, Cluster: cluster}
}

func newAppDelta(cluster, op string, app *appv1beta1.Application) *Frame {
	return &Frame{V: ProtocolVersion, Kind: KindAppDelta, Cluster: cluster, AppDelta: &AppDeltaPayload{Op: op, App: app}}
}

func newK8sEvent(cluster string, e *corev1.Event) *Frame {
	return &Frame{V: ProtocolVersion, Kind: KindK8sEvent, Cluster: cluster, K8sEvent: &K8sEventPayload{Event: e}}
}

// newLogMetrics builds a log_metrics frame for one window. Samples are defensively
// re-truncated here (callers, e.g. logmetrics.Collector, are expected to have already
// truncated via TruncateSample, but this is the last line of defense before the wire).
func newLogMetrics(cluster string, windowStart int64, windowSec int, services []ServiceLogMetrics) *Frame {
	for i := range services {
		services[i].Sample = TruncateSample(services[i].Sample)
	}
	return &Frame{V: ProtocolVersion, Kind: KindLogMetrics, Cluster: cluster, LogMetrics: &LogMetricsPayload{
		WindowStart: windowStart,
		WindowSec:   windowSec,
		Services:    services,
	}}
}

// TruncateSample clips s to MaxLogMetricsSampleLen, matching the receiver's field
// limit so it never has to reject/trim a sample itself.
func TruncateSample(s string) string {
	if len(s) <= MaxLogMetricsSampleLen {
		return s
	}
	return s[:MaxLogMetricsSampleLen]
}

// chunkServices splits services into groups of at most MaxLogMetricsServices, so a
// scrape that qualifies more services than the per-frame cap is sent as multiple
// log_metrics frames for the same window.
func chunkServices(services []ServiceLogMetrics) [][]ServiceLogMetrics {
	if len(services) == 0 {
		return nil
	}
	var out [][]ServiceLogMetrics
	for i := 0; i < len(services); i += MaxLogMetricsServices {
		end := i + MaxLogMetricsServices
		if end > len(services) {
			end = len(services)
		}
		out = append(out, services[i:end])
	}
	return out
}

func newHeartbeat(cluster string) *Frame {
	return &Frame{V: ProtocolVersion, Kind: KindHeartbeat, Cluster: cluster}
}

// encode marshals a frame to its wire bytes.
func encode(f *Frame) ([]byte, error) {
	return json.Marshal(f)
}

// decode parses wire bytes into a Frame (used to read hub→agent pong frames).
func decode(data []byte) (*Frame, error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	return &f, nil
}
