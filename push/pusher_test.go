package push

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

// fakeHub is a WS server that records received frames and can send a pong.
type fakeHub struct {
	mu     sync.Mutex
	frames []*Frame
	gotTok string
}

func (h *fakeHub) record(f *Frame) {
	h.mu.Lock()
	h.frames = append(h.frames, f)
	h.mu.Unlock()
}

func (h *fakeHub) kinds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.frames))
	for i, f := range h.frames {
		out[i] = f.Kind
	}
	return out
}

var testUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func newFakeHub(t *testing.T) (*fakeHub, *httptest.Server) {
	t.Helper()
	h := &fakeHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.gotTok = r.Header.Get("Authorization")
		h.mu.Unlock()
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f, err := decode(data)
			if err != nil {
				continue
			}
			h.record(f)
			if f.Kind == KindHeartbeat {
				pong, _ := encode(&Frame{V: 1, Kind: KindPong, Cluster: f.Cluster})
				_ = conn.WriteMessage(websocket.TextMessage, pong)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

func newTestPusher(endpoint string, apps []*appv1beta1.Application) *Pusher {
	p := &Pusher{
		opts: Options{
			Endpoint:      endpoint,
			ClusterName:   "owl",
			Token:         "tok",
			Heartbeat:     150 * time.Millisecond,
			SnapshotChunk: 50,
		},
		log:          logr.Discard(),
		skipHandlers: true,
		listAppsFn: func(context.Context) ([]*appv1beta1.Application, error) {
			return apps, nil
		},
	}
	return p
}

func mkApp(name string, ready bool) *appv1beta1.Application {
	a := &appv1beta1.Application{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ops"}}
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	a.Status.ComponentsReady = "1/1"
	a.Status.Conditions = []appv1beta1.Condition{{Type: "Ready", Status: st}}
	return a
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestPusher_HelloSnapshotHeartbeat drives one connection and asserts the frame
// sequence the receiver expects: hello → snapshot → snapshot_end → heartbeat,
// with the Bearer token on the upgrade.
func TestPusher_HelloSnapshotHeartbeat(t *testing.T) {
	hub, srv := newFakeHub(t)
	p := newTestPusher(wsURL(srv), []*appv1beta1.Application{mkApp("api", true), mkApp("web", false)})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = p.runOnce(ctx) }()

	// Wait until we have at least hello, snapshot, snapshot_end, and one heartbeat.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		k := hub.kinds()
		if len(k) >= 4 && contains(k, KindHeartbeat) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	k := hub.kinds()
	if len(k) < 3 || k[0] != KindHello || k[1] != KindSnapshot || k[2] != KindSnapshotEnd {
		t.Fatalf("frame sequence = %v, want hello, snapshot, snapshot_end, ...", k)
	}
	if !contains(k, KindHeartbeat) {
		t.Errorf("no heartbeat sent; frames = %v", k)
	}
	hub.mu.Lock()
	tok := hub.gotTok
	hub.mu.Unlock()
	if tok != "Bearer tok" {
		t.Errorf("auth header = %q, want 'Bearer tok'", tok)
	}
}

// TestPusher_DeltaEnqueued verifies onApp enqueues an app_delta that gets sent.
func TestPusher_DeltaEnqueued(t *testing.T) {
	hub, srv := newFakeHub(t)
	p := newTestPusher(wsURL(srv), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = p.runOnce(ctx) }()

	// Wait for the connection to establish (snapshot_end seen), then enqueue a delta.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if contains(hub.kinds(), KindSnapshotEnd) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.onApp(OpUpdate, mkApp("api", false))

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if contains(hub.kinds(), KindAppDelta) {
			cancel()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatalf("app_delta not received; frames = %v", hub.kinds())
}

func TestValidateEndpoint(t *testing.T) {
	tests := []struct {
		endpoint       string
		allowPlaintext bool
		wantErr        bool
	}{
		{"wss://triage/v1/cluster-agent/ws", false, false}, // secure → ok
		{"wss://triage/v1/cluster-agent/ws", true, false},  // secure, plaintext allowed → ok
		{"ws://triage/v1/cluster-agent/ws", false, true},   // plaintext, not opted in → reject
		{"ws://triage/v1/cluster-agent/ws", true, false},   // plaintext, explicitly allowed → ok
		{"http://triage/foo", false, true},                 // wrong scheme → reject
		{"https://triage/foo", false, true},                // wrong scheme → reject
		{"triage:8080", false, true},                       // no ws scheme → reject
		{"://bad", false, true},                            // unparseable → reject
	}
	for _, tt := range tests {
		err := ValidateEndpoint(tt.endpoint, tt.allowPlaintext)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateEndpoint(%q, allowPlaintext=%v) err=%v, wantErr=%v", tt.endpoint, tt.allowPlaintext, err, tt.wantErr)
		}
	}
}

func TestParseNamespaces(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"ops", []string{"ops"}},
		{"ops,dev", []string{"ops", "dev"}},
		{"ops, dev", []string{"ops", "dev"}},    // spaces after comma
		{" ops , dev ", []string{"ops", "dev"}}, // spaces everywhere
		{"ops, ,dev,", []string{"ops", "dev"}},  // empty entries dropped
		{"  ", nil},                             // all blank → nil
		{",,", nil},                             // only separators → nil
	}
	for _, tt := range tests {
		got := ParseNamespaces(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("ParseNamespaces(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("ParseNamespaces(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
			}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
