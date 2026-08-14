// Package mesh wraps the NNG raw sockets that carry the linguine control
// plane: the central router runs an xrep listener, and worker daemons dial
// out with xreq sockets. Raw sockets drop the strict request/reply state
// machine so a worker can stream many reply frames for one dispatch, and so
// the router can send a job to a worker before that worker has "requested"
// anything beyond its heartbeat.
//
// Correlation between a dispatch and its streamed replies is done with the
// reqID carried inside the body envelope (see package protocol). mangos raw
// req/rep also imposes a backtrace protocol: every message body must begin
// with a 4-byte request-id whose high bit is set, or the receiving raw socket
// drops it; a fixed backtraceToken (see below) satisfies this. The only other
// mangos header field we rely on is the 4-byte pipe id that xrep uses to
// route an outbound message to a specific connected worker.
package mesh

import (
	"encoding/binary"
	"fmt"

	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/xrep"
	"go.nanomsg.org/mangos/v3/protocol/xreq"
	_ "go.nanomsg.org/mangos/v3/transport/all"
)

// PipeID is mangos's 4-byte routing identifier for a single connected peer
// pipe. The router learns each worker's PipeID from that worker's heartbeat
// and uses it to target dispatches.
type PipeID uint32

// backtraceToken is a fixed 4-byte mangos request-id whose high bit is set
// (0x80000000). mangos raw req/rep uses a backtrace protocol: the receiver
// (xrep) consumes 4-byte chunks from the front of the body until it finds one
// whose first byte has the high bit set, treating them as request-id hops.
// Without a high-bit-terminated prefix the receiver drops the message. Every
// mesh message therefore carries this token as its leading backtrace entry so
// the receiver terminates after one chunk and delivers the rest (the body
// envelope) intact. Correlation is still done with the envelope reqID, so the
// token value is a constant, not a per-request id.
var backtraceToken = [4]byte{0x80, 0x00, 0x00, 0x00}

// Router is the central xrep socket: it listens for outbound worker dials,
// learns worker pipe ids from their heartbeats, and dispatches jobs to a
// chosen worker by pipe id.
type Router struct {
	sock mangos.Socket
}

// NewRouter creates an xrep router socket with a 2-second receive deadline so
// Recv never blocks indefinitely.
func NewRouter() (*Router, error) {
	sock, err := xrep.NewSocket()
	if err != nil {
		return nil, fmt.Errorf("mesh: xrep socket: %w", err)
	}
	if err := sock.SetOption(mangos.OptionRecvDeadline, recvDeadline); err != nil {
		_ = sock.Close()
		return nil, fmt.Errorf("mesh: set recv deadline: %w", err)
	}
	return &Router{sock: sock}, nil
}

// Listen binds the router to a mangos listen address (e.g.
// "inproc://linguine-x" for tests, "tls+tcp://0.0.0.0:9000" for production).
func (r *Router) Listen(addr string) error {
	if err := r.sock.Listen(addr); err != nil {
		return fmt.Errorf("mesh: listen %s: %w", addr, err)
	}
	return nil
}

// Recv blocks for up to the receive deadline for an inbound message from any
// connected worker. The caller must call Free on the returned message once
// done with it.
func (r *Router) Recv() (*mangos.Message, error) {
	return r.sock.RecvMsg()
}

// SendTo sends body to a specific worker pipe. The message header is
// [pipeID (4)][backtraceToken (4)]: xrep pops the leading pipe id to route to
// that peer, and the remaining backtrace token lets the worker's raw xreq
// receiver terminate its backtrace scan and deliver the body envelope intact.
func (r *Router) SendTo(pipe PipeID, body []byte) error {
	msg := mangos.NewMessage(len(body))
	msg.Header = append(msg.Header, make([]byte, 4)...)
	binary.BigEndian.PutUint32(msg.Header, uint32(pipe))
	msg.Header = append(msg.Header, backtraceToken[:]...)
	msg.Body = append(msg.Body, body...)
	if err := r.sock.SendMsg(msg); err != nil {
		return fmt.Errorf("mesh: send to pipe %d: %w", pipe, err)
	}
	return nil
}

// Close releases the socket.
func (r *Router) Close() error { return r.sock.Close() }

// Worker is the outbound xreq socket: it dials the router, sends heartbeats
// and reply frames, and receives dispatched jobs.
type Worker struct {
	sock mangos.Socket
}

// NewWorker creates an xreq worker socket with a 2-second receive deadline.
func NewWorker() (*Worker, error) {
	sock, err := xreq.NewSocket()
	if err != nil {
		return nil, fmt.Errorf("mesh: xreq socket: %w", err)
	}
	if err := sock.SetOption(mangos.OptionRecvDeadline, recvDeadline); err != nil {
		_ = sock.Close()
		return nil, fmt.Errorf("mesh: set recv deadline: %w", err)
	}
	return &Worker{sock: sock}, nil
}

// Dial connects outbound to the router's listen address.
func (w *Worker) Dial(addr string) error {
	if err := w.sock.Dial(addr); err != nil {
		return fmt.Errorf("mesh: dial %s: %w", addr, err)
	}
	return nil
}

// Send sends body to the router, prepending the backtraceToken so the
// router's raw xrep receiver accepts the message. With a single connected
// peer (the router) routing is unambiguous; correlation is by the body
// envelope's reqID, not the mangos request-id.
func (w *Worker) Send(body []byte) error {
	msg := mangos.NewMessage(len(body))
	msg.Header = append(msg.Header, backtraceToken[:]...)
	msg.Body = append(msg.Body, body...)
	if err := w.sock.SendMsg(msg); err != nil {
		return fmt.Errorf("mesh: worker send: %w", err)
	}
	return nil
}

// SendReply sends a reply frame back to the router, copying the backtrace
// header from the received job so mangos routes it correctly. This is the
// standard raw-req reply path; it is used when a worker streams chunks back.
func (w *Worker) SendReply(backtrace, body []byte) error {
	msg := mangos.NewMessage(len(body))
	msg.Header = append(msg.Header, backtrace...)
	msg.Body = append(msg.Body, body...)
	if err := w.sock.SendMsg(msg); err != nil {
		return fmt.Errorf("mesh: worker reply: %w", err)
	}
	return nil
}

// Recv blocks for up to the receive deadline for an inbound message (a
// dispatched job) from the router. The caller must Free the returned message.
func (w *Worker) Recv() (*mangos.Message, error) {
	return w.sock.RecvMsg()
}

// Close releases the socket.
func (w *Worker) Close() error { return w.sock.Close() }

// PipeFromHeader extracts the 4-byte pipe id from an xrep receive header.
// The xrep receive header begins with the sender's pipe id (4 bytes),
// optionally followed by a mangos request id; only the leading pipe id is
// needed for routing.
func PipeFromHeader(header []byte) (PipeID, error) {
	if len(header) < 4 {
		return 0, fmt.Errorf("mesh: header too short for pipe id: have %d bytes", len(header))
	}
	return PipeID(binary.BigEndian.Uint32(header[:4])), nil
}
