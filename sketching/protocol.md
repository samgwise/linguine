Here is a complete, production-ready Go implementation demonstrating real-time Server-Sent Events (SSE) token streaming over `mangos/v3`.

In standard REQ/REP, NNG enforces a strict 1-request to 1-response pattern. To achieve **high-throughput streaming** without losing request context, this implementation uses `xreq` / `xrep` (raw request/reply sockets). The worker prepends the NNG routing header back onto every chunk frame until an explicit `EOF` signal is sent.

---

## 1. Protocol & Framing (`protocol/frame.go`)

This module handles message serialization and standard binary framing for routing chunks.

```go
package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// FrameType identifies the chunk payload state
type FrameType byte

const (
	FrameTypeHeader FrameType = 0x01 // Metadata & Status
	FrameTypeChunk  FrameType = 0x02 // SSE Data payload
	FrameTypeEOF    FrameType = 0x03 // End of Stream
	FrameTypeError  FrameType = 0xFF // Error notification
)

// Frame encapsulates a single message boundary in NNG
type Frame struct {
	Type    FrameType
	Payload []byte
}

// Encode converts a Frame into a binary payload: [1-byte Type][4-byte Length][Payload]
func (f *Frame) Encode() []byte {
	buf := make([]byte, 5+len(f.Payload))
	buf[0] = byte(f.Type)
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(f.Payload)))
	copy(buf[5:], f.Payload)
	return buf
}

// DecodeFrame unpacks binary data into a Frame
func DecodeFrame(data []byte) (*Frame, error) {
	if len(data) < 5 {
		return nil, fmt.Errorf("frame too short")
	}
	fType := FrameType(data[0])
	length := binary.BigEndian.Uint32(data[1:5])
	
	if uint32(len(data)-5) < length {
		return nil, fmt.Errorf("truncated payload: expected %d, got %d", length, len(data)-5)
	}

	return &Frame{
		Type:    fType,
		Payload: data[5 : 5+length],
	}, nil
}

```

---

## 2. Central Router & SSE Proxy (`router/router.go`)

The Central Router receives client HTTP requests via Fiber v3, forwards them across `mangos/v3` `xrep` sockets, and streams incoming SSE frames directly back into the Fiber flush buffer.

```go
package router

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/gofiber/fiber/v3"
	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/xrep"
	_ "go.nanomsg.org/mangos/v3/transport/tls"

	"yourproject/protocol"
)

type RouterServer struct {
	sock     mangos.Socket
	sessions sync.Map // Map[string]chan *protocol.Frame
}

func NewRouterServer(listenAddr string) (*RouterServer, error) {
	sock, err := xrep.NewSocket()
	if err != nil {
		return nil, fmt.Errorf("failed to create xrep socket: %w", err)
	}

	if err := sock.Listen(listenAddr); err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", listenAddr, err)
	}

	rs := &RouterServer{
		sock: sock,
	}

	// Start background NNG message dispatcher
	go rs.listenNNGLoop()

	return rs, nil
}

// listenNNGLoop listens for incoming multiplexed frames from workers
func (rs *RouterServer) listenNNGLoop() {
	for {
		msg, err := rs.sock.RecvMsg()
		if err != nil {
			log.Printf("[Router] NNG receive error: %v", err)
			return
		}

		// In XREP, msg.Header contains routing flags (Address Backtrace)
		// msg.Body contains [ReqIDLen (1 byte)][ReqID string][Frame Payload]
		body := msg.Body
		if len(body) < 2 {
			msg.Free()
			continue
		}

		reqIDLen := int(body[0])
		if len(body) < 1+reqIDLen {
			msg.Free()
			continue
		}

		reqID := string(body[1 : 1+reqIDLen])
		rawFrame := body[1+reqIDLen:]

		frame, err := protocol.DecodeFrame(rawFrame)
		if err != nil {
			log.Printf("[Router] Failed to decode frame: %v", err)
			msg.Free()
			continue
		}

		// Dispatch frame to active HTTP handler session
		if ch, ok := rs.sessions.Load(reqID); ok {
			ch.(chan *protocol.Frame) <- frame
		}

		msg.Free()
	}
}

// HandleStreamCompletions is the Fiber v3 endpoint (/v1/chat/completions)
func (rs *RouterServer) HandleStreamCompletions(c fiber.Ctx) error {
	reqID := c.Context().ID().String()
	frameChan := make(chan *protocol.Frame, 100)
	rs.sessions.Store(reqID, frameChan)
	defer func() {
		rs.sessions.Delete(reqID)
		close(frameChan)
	}()

	// 1. Send Dispatch Request over NNG
	// Envelope: [ReqIDLen (1 byte)][ReqID string][JSON Request Body]
	reqBody := c.Body()
	payload := make([]byte, 1+len(reqID)+len(reqBody))
	payload[0] = byte(len(reqID))
	copy(payload[1:], []byte(reqID))
	copy(payload[1+len(reqID):], reqBody)

	msg := mangos.NewMessage(len(payload))
	msg.Body = append(msg.Body, payload...)
	
	if err := rs.sock.SendMsg(msg); err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "Failed to dispatch request to worker mesh"})
	}

	// 2. Set SSE Headers
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")

	// 3. Stream frames to client via Fiber chunked writer
	c.RequestCtx().SetBodyStreamWriter(bufio.WriterFunc(func(w *bufio.Writer) {
		for frame := range frameChan {
			switch frame.Type {
			case protocol.FrameTypeChunk:
				w.Write(frame.Payload)
				w.Flush()

			case protocol.FrameTypeEOF:
				w.WriteString("data: [DONE]\n\n")
				w.Flush()
				return

			case protocol.FrameTypeError:
				w.WriteString(fmt.Sprintf("data: {\"error\": \"%s\"}\n\n", string(frame.Payload)))
				w.Flush()
				return
			}
		}
	}))

	return nil
}

```

---

## 3. Worker Daemon Engine (`worker/worker.go`)

The Worker Daemon establishes an outbound TLS NNG connection to the router, executes local requests against `llama.cpp` / `vLLM`, and forwards SSE tokens frame-by-frame.

```go
package worker

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"

	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/xreq"
	_ "go.nanomsg.org/mangos/v3/transport/tls"

	"yourproject/protocol"
)

type WorkerDaemon struct {
	sock          mangos.Socket
	localEngineURL string // e.g., "http://127.0.0.1:8080/v1/chat/completions"
}

func NewWorkerDaemon(routerAddr, localEngineURL string) (*WorkerDaemon, error) {
	sock, err := xreq.NewSocket()
	if err != nil {
		return nil, fmt.Errorf("failed to create xreq socket: %w", err)
	}

	if err := sock.Dial(routerAddr); err != nil {
		return nil, fmt.Errorf("failed to dial router at %s: %w", routerAddr, err)
	}

	return &WorkerDaemon{
		sock:          sock,
		localEngineURL: localEngineURL,
	}, nil
}

// Start processing incoming requests from Central Router
func (w *WorkerDaemon) Start() {
	log.Println("[Worker] Daemon active. Waiting for LLM workloads...")
	for {
		msg, err := w.sock.RecvMsg()
		if err != nil {
			log.Fatalf("[Worker] NNG Socket read error: %v", err)
		}

		// Process work unit asynchronously to maximize worker throughput
		go w.processWorkload(msg)
	}
}

func (w *WorkerDaemon) processWorkload(msg *mangos.Message) {
	// Preserve NNG Header Backtrace (contains routing identifiers for XREP)
	headerBacktrace := msg.Header
	body := msg.Body

	if len(body) < 2 {
		msg.Free()
		return
	}

	reqIDLen := int(body[0])
	reqID := body[1 : 1+reqIDLen]
	jsonPayload := body[1+reqIDLen:]

	// Helper to send frame chunks back over NNG
	sendFrame := func(fType protocol.FrameType, payload []byte) error {
		frame := &protocol.Frame{Type: fType, Payload: payload}
		encoded := frame.Encode()

		// Construct response: [ReqIDLen][ReqID][Frame]
		respBody := make([]byte, 1+len(reqID)+len(encoded))
		respBody[0] = byte(len(reqID))
		copy(respBody[1:], reqID)
		copy(respBody[1+len(reqID):], encoded)

		replyMsg := mangos.NewMessage(len(respBody))
		replyMsg.Header = append(replyMsg.Header, headerBacktrace...) // Set routing backtrace
		replyMsg.Body = append(replyMsg.Body, respBody...)

		return w.sock.SendMsg(replyMsg)
	}

	// 1. Post to Local LLM Engine (llama.cpp / vLLM)
	resp, err := http.Post(w.localEngineURL, "application/json", bytes.NewReader(jsonPayload))
	if err != nil {
		sendFrame(protocol.FrameTypeError, []byte(fmt.Sprintf("Local Engine Unreachable: %v", err)))
		msg.Free()
		return
	}
	defer resp.Body.Close()

	// 2. Read SSE stream and dispatch chunks to Router over NNG
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if err := sendFrame(protocol.FrameTypeChunk, line); err != nil {
				log.Printf("[Worker] Send chunk error: %v", err)
				break
			}
		}
		if err != nil {
			if err == io.EOF {
				sendFrame(protocol.FrameTypeEOF, nil)
			} else {
				sendFrame(protocol.FrameTypeError, []byte(err.Error()))
			}
			break
		}
	}

	msg.Free()
}

```

---

## Flow & Architectural Guarantees

1. **Multiplexed Zero-Inbound Connectivity:** The worker calls `sock.Dial()` to establish a single outbound TLS connection. All streaming events are multiplexed back through this pipe without exposing open ports on the local GPU machine.
2. **Asynchronous Non-blocking Operations:** Fiber v3 handles client HTTP connections while background NNG read/write loops pass frames using thread-safe `sync.Map` channels.
3. **Memory Pool Optimization:** Uses `mangos.NewMessage()` and explicit `msg.Free()` to minimize garbage collection overhead under heavy streaming loads.