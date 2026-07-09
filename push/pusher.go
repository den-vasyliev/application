package push

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
)

// ValidateEndpoint checks the push endpoint scheme. wss:// (encrypted) is always
// accepted. ws:// (plaintext) sends the bearer token in the clear and is accepted
// only when allowPlaintext is true, so plaintext is a deliberate opt-in — never a
// silent default. This is independent of TLS certificate verification, which is a
// separate concern (see Options.InsecureTLS).
func ValidateEndpoint(endpoint string, allowPlaintext bool) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid --push-endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		if allowPlaintext {
			return nil
		}
		return fmt.Errorf("--push-endpoint uses plaintext ws:// which sends the bearer token in the clear; use wss:// or set --push-allow-plaintext to allow it")
	default:
		return fmt.Errorf("--push-endpoint must be a ws:// or wss:// URL, got scheme %q", u.Scheme)
	}
}

// ParseNamespaces splits a comma-separated namespace list into a clean slice,
// trimming surrounding whitespace and dropping empty entries. So "ops, dev",
// "ops,dev" and "ops, ,dev," all yield ["ops", "dev"]. An empty or all-blank
// input returns nil (meaning "all namespaces").
func ParseNamespaces(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if ns := strings.TrimSpace(part); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// Options configures the Pusher.
type Options struct {
	Endpoint      string        // wss://host/v1/cluster-agent/ws; empty disables push mode
	ClusterName   string        // stamped into every frame (triage source segment)
	Token         string        // Bearer token (literal); TokenFile takes precedence
	TokenFile     string        // path to a file containing the Bearer token
	Namespaces    []string      // namespaces to watch; empty = all
	AgentVersion  string        // reported in hello
	Heartbeat     time.Duration // heartbeat interval (default 20s)
	InsecureTLS   bool          // skip TLS verify (dev only)
	SnapshotChunk int           // apps per snapshot frame (default 50)
}

// Pusher streams Application inventory + Kubernetes Warning events to a triage agent
// over an outbound WebSocket. It is a manager.Runnable gated behind leader election
// so only one replica streams.
type Pusher struct {
	opts   Options
	cache  cache.Cache
	client client.Client
	log    logr.Logger

	// outbound queue of frames produced by informer handlers; drained by the writer.
	mu    sync.Mutex
	sendC chan *Frame

	// listAppsFn sources the snapshot. Defaults to the cache; overridable in tests.
	listAppsFn func(context.Context) ([]*appv1beta1.Application, error)
	// skipHandlers disables informer registration (tests that drive frames directly).
	skipHandlers bool
}

// New creates a Pusher. Returns nil if push mode is disabled (empty endpoint).
func New(opts Options, mgr manager.Manager, log logr.Logger) *Pusher {
	if opts.Endpoint == "" {
		return nil
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 20 * time.Second
	}
	if opts.SnapshotChunk <= 0 {
		opts.SnapshotChunk = 50
	}
	return &Pusher{
		opts:   opts,
		cache:  mgr.GetCache(),
		client: mgr.GetClient(),
		log:    log.WithName("push"),
	}
}

// NeedLeaderElection ensures only the leader replica streams.
func (p *Pusher) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It registers informer handlers once, then runs
// the connect→stream→reconnect loop until the context is cancelled.
func (p *Pusher) Start(ctx context.Context) error {
	if !p.cache.WaitForCacheSync(ctx) {
		p.log.Info("cache sync failed; push disabled")
		<-ctx.Done()
		return nil
	}

	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		connected, err := p.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		// A dropped or refused connection is a routine, self-healing condition (the
		// triage agent may be restarting or briefly unreachable). Log it at a low
		// level without a stack trace and keep retrying with capped backoff; only a
		// genuinely unexpected error is worth a louder line.
		if err != nil {
			p.log.V(1).Info("push connection ended; will reconnect",
				"reason", err.Error(), "backoff", backoff.String())
		}
		// A connection that actually established resets the backoff, so a long-lived
		// stream that finally drops reconnects promptly rather than at max backoff.
		if connected {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce dials, sends hello + snapshot, then streams deltas until the connection or
// context ends. A fresh send channel + informer registration per connection ensures a
// reconnect re-snapshots cleanly. It returns whether the connection was successfully
// established (used to reset reconnect backoff) alongside any error.
func (p *Pusher) runOnce(ctx context.Context) (connected bool, err error) {
	token, err := p.resolveToken()
	if err != nil {
		return false, err
	}

	dialer := websocket.DefaultDialer
	if p.opts.InsecureTLS {
		dialer = &websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)

	conn, _, err := dialer.DialContext(ctx, p.opts.Endpoint, hdr)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	connected = true

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	p.mu.Lock()
	p.sendC = make(chan *Frame, 256)
	sendC := p.sendC
	p.mu.Unlock()

	// Register informer handlers scoped to this connection.
	if !p.skipHandlers {
		regs, err := p.registerHandlers(connCtx)
		if err != nil {
			return connected, err
		}
		defer p.removeHandlers(regs)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); p.readLoop(connCtx, cancel, conn) }()

	// Hello + snapshot before streaming deltas.
	if err := p.writeFrame(conn, newHello(p.opts.ClusterName, p.opts.AgentVersion, p.opts.Namespaces)); err != nil {
		cancel()
		wg.Wait()
		return connected, err
	}
	if err := p.sendSnapshot(conn); err != nil {
		cancel()
		wg.Wait()
		return connected, err
	}

	err = p.writeLoop(connCtx, conn, sendC)
	cancel()
	wg.Wait()
	return connected, err
}

// writeLoop drains the send channel and emits heartbeats until the context ends.
func (p *Pusher) writeLoop(ctx context.Context, conn *websocket.Conn, sendC chan *Frame) error {
	ticker := time.NewTicker(p.opts.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-sendC:
			if err := p.writeFrame(conn, f); err != nil {
				return err
			}
		case <-ticker.C:
			if err := p.writeFrame(conn, newHeartbeat(p.opts.ClusterName)); err != nil {
				return err
			}
		}
	}
}

// readLoop consumes hub→agent frames (pong). Any read error ends the connection.
func (p *Pusher) readLoop(_ context.Context, cancel context.CancelFunc, conn *websocket.Conn) {
	defer cancel()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if _, err := decode(data); err != nil {
			p.log.V(2).Info("ignoring undecodable hub frame")
		}
	}
}

// sendSnapshot lists all in-scope Applications from the cache and sends them in
// chunks, terminated by snapshot_end.
func (p *Pusher) sendSnapshot(conn *websocket.Conn) error {
	listFn := p.listAppsFn
	if listFn == nil {
		listFn = p.listApps
	}
	apps, err := listFn(context.Background())
	if err != nil {
		return err
	}
	for i := 0; i < len(apps); i += p.opts.SnapshotChunk {
		end := i + p.opts.SnapshotChunk
		if end > len(apps) {
			end = len(apps)
		}
		if err := p.writeFrame(conn, newSnapshot(p.opts.ClusterName, apps[i:end])); err != nil {
			return err
		}
	}
	p.log.Info("sent snapshot", "apps", len(apps), "cluster", p.opts.ClusterName)
	return p.writeFrame(conn, newSnapshotEnd(p.opts.ClusterName))
}

// listApps returns all Applications in the configured namespaces (or all namespaces).
func (p *Pusher) listApps(ctx context.Context) ([]*appv1beta1.Application, error) {
	var out []*appv1beta1.Application
	namespaces := p.opts.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""} // all namespaces
	}
	for _, ns := range namespaces {
		list := &appv1beta1.ApplicationList{}
		opts := []client.ListOption{}
		if ns != "" {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := p.cache.List(ctx, list, opts...); err != nil {
			return nil, err
		}
		for i := range list.Items {
			out = append(out, &list.Items[i])
		}
	}
	return out, nil
}

// registerHandlers wires Application + Event informer callbacks that enqueue frames.
func (p *Pusher) registerHandlers(ctx context.Context) ([]handlerReg, error) {
	appInf, err := p.cache.GetInformer(ctx, &appv1beta1.Application{})
	if err != nil {
		return nil, err
	}
	appReg, err := appInf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { p.onApp(OpAdd, obj) },
		UpdateFunc: func(_, obj any) { p.onApp(OpUpdate, obj) },
		DeleteFunc: func(obj any) { p.onApp(OpDelete, obj) },
	})
	if err != nil {
		return nil, err
	}
	regs := []handlerReg{{inf: appInf, reg: appReg}}

	evtInf, err := p.cache.GetInformer(ctx, &corev1.Event{})
	if err != nil {
		p.removeHandlers(regs)
		return nil, err
	}
	evtReg, err := evtInf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { p.onEvent(obj) },
		UpdateFunc: func(_, obj any) { p.onEvent(obj) },
	})
	if err != nil {
		p.removeHandlers(regs)
		return nil, err
	}
	p.log.Info("registered Application + Event informer handlers")
	return append(regs, handlerReg{inf: evtInf, reg: evtReg}), nil
}

type handlerReg struct {
	inf cache.Informer
	reg toolscache.ResourceEventHandlerRegistration
}

func (p *Pusher) removeHandlers(regs []handlerReg) {
	for _, r := range regs {
		_ = r.inf.RemoveEventHandler(r.reg)
	}
}

func (p *Pusher) onApp(op string, obj any) {
	app, ok := obj.(*appv1beta1.Application)
	if !ok {
		return
	}
	if !p.inScope(app.Namespace) {
		return
	}
	// Trace (V(1), enabled via --zap-log-level=1|debug): one line per pushed
	// Application delta so the exact stream to triage is observable on demand.
	p.log.V(1).Info("push app delta",
		"op", op, "namespace", app.Namespace, "name", app.Name,
		"ready", app.Status.ComponentsReady)
	p.enqueue(newAppDelta(p.opts.ClusterName, op, app))
}

func (p *Pusher) onEvent(obj any) {
	e, ok := obj.(*corev1.Event)
	if !ok || e.Type != corev1.EventTypeWarning {
		return
	}
	if !p.inScope(e.Namespace) {
		return
	}
	// Trace (V(1)): one line per pushed Kubernetes Warning event.
	p.log.V(1).Info("push k8s event",
		"namespace", e.Namespace, "reason", e.Reason,
		"kind", e.InvolvedObject.Kind, "object", e.InvolvedObject.Name)
	p.enqueue(newK8sEvent(p.opts.ClusterName, e))
}

func (p *Pusher) inScope(ns string) bool {
	if len(p.opts.Namespaces) == 0 {
		return true
	}
	for _, want := range p.opts.Namespaces {
		if want == ns {
			return true
		}
	}
	return false
}

// enqueue offers a frame to the current connection's send channel, dropping (with a
// warn) if the socket is backed up — never blocking an informer handler.
func (p *Pusher) enqueue(f *Frame) {
	p.mu.Lock()
	sendC := p.sendC
	p.mu.Unlock()
	if sendC == nil {
		return
	}
	select {
	case sendC <- f:
	default:
		p.log.Info("send queue full; dropping frame", "kind", f.Kind)
	}
}

func (p *Pusher) writeFrame(conn *websocket.Conn, f *Frame) error {
	data, err := encode(f)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// resolveToken reads the token from TokenFile (preferred) or the literal Token.
func (p *Pusher) resolveToken() (string, error) {
	if p.opts.TokenFile != "" {
		b, err := os.ReadFile(p.opts.TokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return p.opts.Token, nil
}
