package mesh

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samgw/linguine/internal/protocol"
)

// uniqueAddr returns a process-unique inproc listen address so multiple
// topology tests never collide on the same inproc endpoint.
var addrCounter uint64

func uniqueAddr(t *testing.T) string {
	t.Helper()
	n := atomic.AddUint64(&addrCounter, 1)
	return fmt.Sprintf("inproc://linguine-topo-%d-%d", n, time.Now().UnixNano())
}

// streamFrame wraps a Frame in an Envelope and encodes it as a worker reply
// body (the layered wire structure used on the mesh).
func streamFrame(reqID string, f *protocol.Frame) []byte {
	return (&protocol.Envelope{ReqID: reqID, Payload: f.Encode()}).Encode()
}

// decodeStream unpacks a worker reply body into its reqID and Frame.
func decodeStream(t *testing.T, body []byte) (string, *protocol.Frame) {
	t.Helper()
	env, err := protocol.DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	f, err := protocol.DecodeFrame(env.Payload)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return env.ReqID, f
}

// plainEnv encodes a reqID + raw string payload (used for heartbeats and
// simple job dispatches in the targeting test).
func plainEnv(reqID, payload string) []byte {
	return (&protocol.Envelope{ReqID: reqID, Payload: []byte(payload)}).Encode()
}

// decodePlain unpacks a reqID + raw string payload.
func decodePlain(t *testing.T, body []byte) (string, string) {
	t.Helper()
	env, err := protocol.DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env.ReqID, string(env.Payload)
}

// TestTopologyFraming proves multi-frame streaming and reqID correlation:
// one worker sends three framed replies (chunk, chunk, eof) and the router
// receives them in order, each keyed by the same reqID.
func TestTopologyFraming(t *testing.T) {
	addr := uniqueAddr(t)
	router, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer router.Close()
	if err := router.Listen(addr, nil); err != nil {
		t.Fatalf("listen: %v", err)
	}

	worker, err := NewWorker()
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	defer worker.Close()
	if err := worker.Dial(addr, nil, ""); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Worker streams three frames back to the router under one reqID.
	want := []*protocol.Frame{
		{Type: protocol.FrameTypeChunk, Payload: []byte("data: {\"delta\":\"c1\"}\n\n")},
		{Type: protocol.FrameTypeChunk, Payload: []byte("data: {\"delta\":\"c2\"}\n\n")},
		{Type: protocol.FrameTypeEOF, Payload: nil},
	}
	for _, f := range want {
		if err := worker.Send(streamFrame("stream-1", f)); err != nil {
			t.Fatalf("worker send: %v", err)
		}
	}

	for i, wantF := range want {
		msg, err := router.Recv()
		if err != nil {
			t.Fatalf("recv frame %d: %v", i, err)
		}
		t.Logf("frame %d headerlen=%d bodylen=%d", i, len(msg.Header), len(msg.Body))
		reqID, gotF := decodeStream(t, msg.Body)
		msg.Free()
		if reqID != "stream-1" {
			t.Errorf("frame %d reqID: got %q, want %q", i, reqID, "stream-1")
		}
		if gotF.Type != wantF.Type {
			t.Errorf("frame %d type: got %d, want %d", i, gotF.Type, wantF.Type)
		}
		if string(gotF.Payload) != string(wantF.Payload) {
			t.Errorf("frame %d payload: got %q, want %q", i, gotF.Payload, wantF.Payload)
		}
	}
}

// TestTopologyTargeting proves the router can dispatch to a specific worker:
// two workers dial in and heartbeat; the router learns each pipe id, sends
// job-A to worker A and job-B to worker B, and each worker receives only its
// own job. This closes the targeting gap in sketching/protocol.md:174.
func TestTopologyTargeting(t *testing.T) {
	addr := uniqueAddr(t)
	router, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer router.Close()
	if err := router.Listen(addr, nil); err != nil {
		t.Fatalf("listen: %v", err)
	}

	workerA, err := NewWorker()
	if err != nil {
		t.Fatalf("new worker A: %v", err)
	}
	defer workerA.Close()
	if err := workerA.Dial(addr, nil, ""); err != nil {
		t.Fatalf("dial A: %v", err)
	}
	workerB, err := NewWorker()
	if err != nil {
		t.Fatalf("new worker B: %v", err)
	}
	defer workerB.Close()
	if err := workerB.Dial(addr, nil, ""); err != nil {
		t.Fatalf("dial B: %v", err)
	}

	// Each worker heartbeats with a payload naming itself, so the router can
	// map label -> pipe id.
	if err := workerA.Send(plainEnv("hb", "A")); err != nil {
		t.Fatalf("hb A send: %v", err)
	}
	if err := workerB.Send(plainEnv("hb", "B")); err != nil {
		t.Fatalf("hb B send: %v", err)
	}

	pipes := make(map[string]PipeID)
	for len(pipes) < 2 {
		msg, err := router.Recv()
		if err != nil {
			t.Fatalf("recv heartbeat: %v", err)
		}
		t.Logf("heartbeat headerlen=%d bodylen=%d", len(msg.Header), len(msg.Body))
		pipe, err := PipeFromHeader(msg.Header)
		if err != nil {
			msg.Free()
			t.Fatalf("pipe from header: %v", err)
		}
		_, label := decodePlain(t, msg.Body)
		msg.Free()
		pipes[label] = pipe
	}
	pipeA, okA := pipes["A"]
	pipeB, okB := pipes["B"]
	if !okA || !okB {
		t.Fatalf("did not learn both pipe ids: %v", pipes)
	}
	if pipeA == pipeB {
		t.Fatalf("both workers share a pipe id: %d", pipeA)
	}

	// Dispatch a distinct job to each worker by pipe id.
	if err := router.SendTo(pipeA, plainEnv("job", "job-A")); err != nil {
		t.Fatalf("send to A: %v", err)
	}
	if err := router.SendTo(pipeB, plainEnv("job", "job-B")); err != nil {
		t.Fatalf("send to B: %v", err)
	}

	msgA, err := workerA.Recv()
	if err != nil {
		t.Fatalf("worker A recv: %v", err)
	}
	t.Logf("job A headerlen=%d bodylen=%d", len(msgA.Header), len(msgA.Body))
	_, payloadA := decodePlain(t, msgA.Body)
	msgA.Free()
	if payloadA != "job-A" {
		t.Errorf("worker A got %q, want job-A (targeting failed)", payloadA)
	}

	msgB, err := workerB.Recv()
	if err != nil {
		t.Fatalf("worker B recv: %v", err)
	}
	t.Logf("job B headerlen=%d bodylen=%d", len(msgB.Header), len(msgB.Body))
	_, payloadB := decodePlain(t, msgB.Body)
	msgB.Free()
	if payloadB != "job-B" {
		t.Errorf("worker B got %q, want job-B (targeting failed)", payloadB)
	}
}

// TestTopologyRoundTrip proves the full dispatch-and-stream flow the real
// worker uses: the router dispatches a job to a worker by pipe id, the worker
// receives it and streams framed replies back (copying the received backtrace
// header), and the router correlates the replies by envelope reqID.
func TestTopologyRoundTrip(t *testing.T) {
	addr := uniqueAddr(t)
	router, err := NewRouter()
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	defer router.Close()
	if err := router.Listen(addr, nil); err != nil {
		t.Fatalf("listen: %v", err)
	}

	worker, err := NewWorker()
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	defer worker.Close()
	if err := worker.Dial(addr, nil, ""); err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Worker heartbeats so the router learns its pipe id.
	if err := worker.Send(plainEnv("hb", "only")); err != nil {
		t.Fatalf("hb send: %v", err)
	}
	hbMsg, err := router.Recv()
	if err != nil {
		t.Fatalf("recv hb: %v", err)
	}
	pipe, err := PipeFromHeader(hbMsg.Header)
	if err != nil {
		hbMsg.Free()
		t.Fatalf("pipe from header: %v", err)
	}
	hbMsg.Free()

	// Router dispatches a job to the worker by pipe id.
	if err := router.SendTo(pipe, plainEnv("job", "do-work")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Worker receives the job and streams three framed replies back, copying
	// the received backtrace header so mangos routes the replies to the router.
	jobMsg, err := worker.Recv()
	if err != nil {
		t.Fatalf("worker recv job: %v", err)
	}
	backtrace := append([]byte{}, jobMsg.Header...)
	_, jobPayload := decodePlain(t, jobMsg.Body)
	jobMsg.Free()
	if jobPayload != "do-work" {
		t.Fatalf("job payload: got %q, want do-work", jobPayload)
	}

	want := []*protocol.Frame{
		{Type: protocol.FrameTypeChunk, Payload: []byte("data: {\"delta\":\"r1\"}\n\n")},
		{Type: protocol.FrameTypeChunk, Payload: []byte("data: {\"delta\":\"r2\"}\n\n")},
		{Type: protocol.FrameTypeEOF, Payload: nil},
	}
	for _, f := range want {
		if err := worker.SendReply(backtrace, streamFrame("rr-1", f)); err != nil {
			t.Fatalf("worker reply: %v", err)
		}
	}

	for i, wantF := range want {
		msg, err := router.Recv()
		if err != nil {
			t.Fatalf("recv reply %d: %v", i, err)
		}
		t.Logf("reply %d headerlen=%d bodylen=%d", i, len(msg.Header), len(msg.Body))
		reqID, gotF := decodeStream(t, msg.Body)
		msg.Free()
		if reqID != "rr-1" {
			t.Errorf("reply %d reqID: got %q, want rr-1", i, reqID)
		}
		if gotF.Type != wantF.Type {
			t.Errorf("reply %d type: got %d, want %d", i, gotF.Type, wantF.Type)
		}
		if string(gotF.Payload) != string(wantF.Payload) {
			t.Errorf("reply %d payload: got %q, want %q", i, gotF.Payload, wantF.Payload)
		}
	}
}
