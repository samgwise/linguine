package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"go.nanomsg.org/mangos/v3"

	"github.com/samgw/linguine/internal/engine"
	"github.com/samgw/linguine/internal/mesh"
	"github.com/samgw/linguine/internal/protocol"
)

// DefaultHeartbeatInterval is the gap between worker heartbeats.
const DefaultHeartbeatInterval = 5 * time.Second

// Daemon is the worker: it dials the router outbound, heartbeats to
// authenticate and stay live, and proxies dispatched requests to a local
// OpenAI-compatible engine, streaming tokens back over the NNG mesh.
type Daemon struct {
	mesh              *mesh.Worker
	engine            engine.Engine
	routerAddr        string
	nodeID            string
	enrollmentToken   string
	activeModel       string
	heartbeatInterval time.Duration
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
	}
	for _, o := range opts {
		o(d)
	}
	return d, nil
}

// Run dials the router and serves jobs until ctx is cancelled or the socket
// is closed.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.mesh.Dial(d.routerAddr); err != nil {
		return fmt.Errorf("worker: dial router: %w", err)
	}
	go d.heartbeatLoop(ctx)
	log.Printf("[worker] %s connected to %s, awaiting jobs", d.nodeID, d.routerAddr)
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
		go d.handleJob(ctx, backtrace, env)
	}
}

// Close releases the mesh socket.
func (d *Daemon) Close() error { return d.mesh.Close() }

func (d *Daemon) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(d.heartbeatInterval)
	defer ticker.Stop()
	d.sendHeartbeat() // announce immediately on connect
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.sendHeartbeat()
		}
	}
}

func (d *Daemon) sendHeartbeat() {
	hb := protocol.Heartbeat{
		NodeID:          d.nodeID,
		EnrollmentToken: d.enrollmentToken,
		ActiveModel:     d.activeModel,
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

func (d *Daemon) handleJob(ctx context.Context, backtrace []byte, env *protocol.Envelope) {
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
