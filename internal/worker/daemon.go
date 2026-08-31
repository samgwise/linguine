package worker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"go.nanomsg.org/mangos/v3"

	"github.com/samgw/linguine/internal/catalog"
	"github.com/samgw/linguine/internal/engine"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/protocol"
)

// DefaultHeartbeatInterval is the gap between worker heartbeats once
// registered with the router. Until the first ack arrives the daemon ticks
// faster (registrationTick) so registration converges quickly.
const DefaultHeartbeatInterval = 5 * time.Second

// registrationTick is the heartbeat period used while the worker has never
// been acknowledged, so a fresh worker registers within a second of the
// socket coming up instead of waiting a full heartbeat interval.
const registrationTick = time.Second

// degradedAfter returns how long the worker tolerates hearing nothing from
// the router (no acks of any kind) while registered before it declares the
// connection degraded: three heartbeat intervals.
func degradedAfter(d *Daemon) time.Duration { return 3 * d.heartbeatInterval }

// Daemon is the worker: it dials the router outbound, heartbeats to
// authenticate and stay live, and proxies dispatched requests to a local
// OpenAI-compatible engine, streaming tokens back over the NNG mesh.
// Connection truth is driven by router acks: the daemon only considers
// itself registered once a heartbeat has been acknowledged.
type Daemon struct {
	mesh              *mesh.Worker
	engine            engine.Engine
	probe             *catalog.Probe
	routerAddr        string
	nodeID            string
	enrollmentToken   string
	activeModel       string
	heartbeatInterval time.Duration
	activeRequests    atomic.Int64
	tlsConfig         *tls.Config // nil for ws:// or inproc://
	proxyURL          string      // empty -> HTTP_PROXY/HTTPS_PROXY env

	mu sync.Mutex
	// state is the worker's view of its own connection (ConnState*).
	state string
	// lastAck is the time of the most recent ack of any kind (accept or
	// reject). Zero until the first ack arrives.
	lastAck time.Time
	// rejectReason is the reason from the most recent rejection ack, cleared
	// by an acceptance.
	rejectReason string
	// warnCount counts consecutive rejections to throttle repeated warnings.
	warnCount int
}

// State returns the worker's current connection state (ConnState*).
func (d *Daemon) State() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// lastRejectReason returns the reason from the most recent rejection ack
// (empty when the worker is accepted). Used by tests.
func (d *Daemon) lastRejectReason() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rejectReason
}

// Option configures a Daemon.
type Option func(*Daemon)

// WithHeartbeatInterval sets the heartbeat period.
func WithHeartbeatInterval(d time.Duration) Option {
	return func(dn *Daemon) { dn.heartbeatInterval = d }
}

// WithActiveModel sets the model label advertised in heartbeats.
func WithActiveModel(m string) Option {
	return func(dn *Daemon) { dn.activeModel = m }
}

// WithProbe attaches a catalog probe so heartbeats advertise the engine's
// current /v1/models catalog. nil leaves the catalog empty.
func WithProbe(p *catalog.Probe) Option {
	return func(dn *Daemon) { dn.probe = p }
}

// WithTLSConfig sets the client TLS config used when dialling a wss:// router
// (CA file, fingerprint pin, or system roots). nil leaves the dial plaintext.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(dn *Daemon) { dn.tlsConfig = cfg }
}

// WithProxyURL tunnels the worker's mesh dial through an HTTP CONNECT proxy.
// An empty string means fall back to the standard HTTP_PROXY/HTTPS_PROXY/NO_PROXY
// environment variables.
func WithProxyURL(u string) Option {
	return func(dn *Daemon) { dn.proxyURL = u }
}

// NewDaemon creates a worker daemon. The NNG socket is created here.
func NewDaemon(routerAddr, nodeID, enrollmentToken string, eng engine.Engine, opts ...Option) (*Daemon, error) {
	sock, err := mesh.NewWorker()
	if err != nil {
		return nil, fmt.Errorf("worker: create mesh socket: %w", err)
	}
	d := &Daemon{
		mesh:              sock,
		engine:            eng,
		routerAddr:        routerAddr,
		nodeID:            nodeID,
		enrollmentToken:   enrollmentToken,
		heartbeatInterval: DefaultHeartbeatInterval,
		state:             protocol.ConnStateConnecting,
	}
	for _, o := range opts {
		o(d)
	}
	return d, nil
}

// Run dials the router and serves jobs until ctx is cancelled or the socket
// is closed. The mangos dialer establishes (and re-establishes) the
// connection asynchronously; registration is signaled by the first
// heartbeat ack, not by this function returning.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.mesh.Dial(d.routerAddr, d.tlsConfig, d.proxyURL); err != nil {
		return fmt.Errorf("worker: dial router: %w", err)
	}
	go d.heartbeatLoop(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msg, err := d.mesh.Recv()
		if err != nil {
			if errors.Is(err, mangos.ErrClosed) {
				return nil
			}
			if errors.Is(err, mangos.ErrRecvTimeout) {
				continue // idle: loop and re-check ctx
			}
			log.Printf("[worker] recv error: %v", err)
			continue
		}
		backtrace := append([]byte{}, msg.Header...)
		env, err := protocol.DecodeEnvelope(msg.Body)
		msg.Free()
		if err != nil {
			log.Printf("[worker] decode envelope: %v", err)
			continue
		}
		switch env.ReqID {
		case protocol.HeartbeatAckReqID:
			d.handleAck(env.Payload)
		case protocol.HeartbeatReqID:
			// Heartbeats flow worker→router only; ignore a stray echo rather
			// than proxying it into the engine.
		default:
			go d.handleJob(ctx, backtrace, env)
		}
	}
}

// Close releases the mesh socket.
func (d *Daemon) Close() error { return d.mesh.Close() }

// heartbeatLoop sends an immediate heartbeat on connect, then ticks at
// registrationTick until the router has acknowledged (fast convergence),
// settling at d.heartbeatInterval once registered. While registered it also
// watches for ack silence and demotes the connection to degraded.
func (d *Daemon) heartbeatLoop(ctx context.Context) {
	d.sendHeartbeat() // announce immediately on connect
	ticker := time.NewTicker(registrationTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.checkDegraded()
			d.sendHeartbeat()
			if d.State() == protocol.ConnStateRegistered {
				ticker.Reset(d.heartbeatInterval)
			}
		}
	}
}

// checkDegraded demotes a registered connection to degraded when no ack has
// arrived within the degradation window (three heartbeat intervals), and
// logs the transition once.
func (d *Daemon) checkDegraded() {
	after := degradedAfter(d)
	d.mu.Lock()
	if d.state != protocol.ConnStateRegistered || d.lastAck.IsZero() || time.Since(d.lastAck) <= after {
		d.mu.Unlock()
		return
	}
	d.state = protocol.ConnStateDegraded
	d.mu.Unlock()
	log.Printf("[worker] no router ack for %s — connection degraded, still heartbeating", after)
}

func (d *Daemon) sendHeartbeat() {
	hb := protocol.Heartbeat{
		NodeID:          d.nodeID,
		EnrollmentToken: d.enrollmentToken,
		ConnectionState: d.State(),
		ActiveModel:     d.activeModel,
		ActiveRequests:  int(d.activeRequests.Load()),
		// VRAM/TPS are best-effort: proxy engines don't expose them
		// consistently, so 1a leaves them zero. Phase 1b/2 populates them
		// from a managed engine's telemetry.
	}
	if d.probe != nil {
		if catalog, _ := d.probe.Current(); catalog != nil {
			hb.Catalog = catalog
			if hb.ActiveModel == "" && len(catalog) > 0 {
				hb.ActiveModel = catalog[0]
			}
		}
	}
	payload, err := json.Marshal(hb)
	if err != nil {
		return
	}
	env := &protocol.Envelope{ReqID: protocol.HeartbeatReqID, Payload: payload}
	if err := d.mesh.Send(env.Encode()); err != nil {
		log.Printf("[worker] heartbeat send: %v", err)
	}
}

// handleAck applies a router heartbeat ack to the worker's connection state.
// Transitions are logged; sustained rejections are throttled so a persistent
// misconfiguration doesn't flood the log.
func (d *Daemon) handleAck(payload []byte) {
	var ack protocol.HeartbeatAck
	if err := json.Unmarshal(payload, &ack); err != nil {
		log.Printf("[worker] decode heartbeat ack: %v", err)
		return
	}
	d.mu.Lock()
	was := d.state
	d.lastAck = time.Now()
	if ack.OK {
		d.rejectReason = ""
		d.warnCount = 0
		d.state = protocol.ConnStateRegistered
		d.mu.Unlock()
		if was != protocol.ConnStateRegistered {
			log.Printf("[worker] registered with router as node %q", ack.NodeID)
		}
		return
	}
	d.rejectReason = ack.Reason
	d.warnCount++
	// Warn on the first rejection, then every warnEvery-th consecutive
	// rejection. The heartbeat loop ticks once per second while
	// unregistered, so warnEvery=12 spaces repeated warnings about a
	// minute apart.
	const warnEvery = 12
	shouldWarn := d.warnCount == 1 || d.warnCount%warnEvery == 0
	d.state = protocol.ConnStateConnecting
	transitioned := was == protocol.ConnStateRegistered
	d.mu.Unlock()
	if transitioned {
		log.Printf("[worker] router now rejecting heartbeats: %s", ack.Reason)
	}
	if !shouldWarn {
		return
	}
	switch ack.Reason {
	case protocol.AckReasonAuthFailed:
		log.Printf("[worker] enrollment token rejected by router — is it a v4.public enrollment token from `linguine admin create-enrollment-token`, not an sk-mesh-… API key?")
	case protocol.AckReasonInactive:
		log.Printf("[worker] enrollment token revoked or expired — issue a new one with `linguine admin create-enrollment-token`")
	default:
		log.Printf("[worker] router rejected heartbeat: %s (%s)", ack.Reason, ack.Detail)
	}
}

func (d *Daemon) handleJob(ctx context.Context, backtrace []byte, env *protocol.Envelope) {
	d.activeRequests.Add(1)
	defer d.activeRequests.Add(-1)
	body, err := d.engine.Proxy(ctx, env.Payload)
	if err != nil {
		d.sendFrame(backtrace, env.ReqID, &protocol.Frame{Type: protocol.FrameTypeError, Payload: []byte(err.Error())})
		return
	}
	defer body.Close()
	buf := make([]byte, 4096)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			d.sendFrame(backtrace, env.ReqID, &protocol.Frame{Type: protocol.FrameTypeChunk, Payload: chunk})
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				d.sendFrame(backtrace, env.ReqID, &protocol.Frame{Type: protocol.FrameTypeEOF})
			} else {
				d.sendFrame(backtrace, env.ReqID, &protocol.Frame{Type: protocol.FrameTypeError, Payload: []byte(rerr.Error())})
			}
			return
		}
	}
}

func (d *Daemon) sendFrame(backtrace []byte, reqID string, f *protocol.Frame) {
	env := &protocol.Envelope{ReqID: reqID, Payload: f.Encode()}
	if err := d.mesh.SendReply(backtrace, env.Encode()); err != nil {
		log.Printf("[worker] send frame: %v", err)
	}
}
