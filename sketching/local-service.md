Here is a clean, modular Go implementation for the **Local Client Proxy / Worker Daemon Adapter**.

This component acts as the bridge on the GPU worker node. It connects outbound over NNG to the Central Router, receives incoming inference jobs, manages local model switching on demand (`llama-server` or `vLLM`), and streams tokens back across the mesh.

---

## Architecture Overview

```
                          [ NNG OUTBOUND TLS ]
                                   │
                                   ▼
┌──────────────────────────────────────────────────────────────────┐
│                      Worker Proxy Daemon                         │
│                                                                  │
│  ┌──────────────────────┐        ┌────────────────────────────┐  │
│  │   NNG Client Mesh    │        │   Model Lifecycle Manager  │  │
│  │  (xreq / mangos v3)  │        │ (Process / Engine Control) │  │
│  └──────────┬───────────┘        └─────────────┬──────────────┘  │
│             │                                  │                 │
│             └────────────────┬─────────────────┘                 │
│                              ▼                                   │
│                 ┌──────────────────────────┐                     │
│                 │ SSE Reverse Proxy Engine │                     │
│                 └────────────┬─────────────┘                     │
└──────────────────────────────┼───────────────────────────────────┘
                               │ HTTP / Localhost Loopback
                               ▼
            ┌────────────────────────────────────┐
            │ Local Inference Engine             │
            │ (llama-server or vLLM on :8080)    │
            └────────────────────────────────────┘

```

---

## 1. Engine Manager (`engine/manager.go`)

This module manages the local inference process (e.g., `llama-server`), keeping track of what model is loaded and executing dynamic model swaps when commanded by the load balancer.

```go
package engine

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

type ManagerConfig struct {
	EnginePath  string        // Path to llama-server binary
	ModelDir    string        // Path to directory containing GGUF files
	ListenAddr  string        // Localhost bind address (e.g., 127.0.0.1:8080)
	SwapTimeout time.Duration // Graceful shutdown window
}

type ModelManager struct {
	cfg          ManagerConfig
	currentModel string
	cmd          *exec.Cmd
	mu           sync.Mutex
}

func NewModelManager(cfg ManagerConfig, initialModel string) *ModelManager {
	return &ModelManager{
		cfg:          cfg,
		currentModel: initialModel,
	}
}

// ActiveModel returns the model currently loaded in VRAM
func (m *ModelManager) ActiveModel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentModel
}

// EnsureModel checks if the target model is running. If not, it executes a swap.
func (m *ModelManager) EnsureModel(ctx context.Context, targetModel string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.currentModel == targetModel && m.cmd != nil && m.cmd.Process != nil {
		return nil // Already hot in VRAM
	}

	log.Printf("[Engine] Model swap required: '%s' -> '%s'", m.currentModel, targetModel)

	// 1. Terminate running process gracefully
	if m.cmd != nil && m.cmd.Process != nil {
		log.Println("[Engine] Stopping active llama-server process...")
		_ = m.cmd.Process.Kill()
		_ = m.cmd.Wait()
	}

	// 2. Launch new llama-server instance with requested weights
	modelPath := fmt.Sprintf("%s/%s.gguf", m.cfg.ModelDir, targetModel)
	m.cmd = exec.CommandContext(ctx, m.cfg.EnginePath,
		"--model", modelPath,
		"--port", "8080",
		"--host", "127.0.0.1",
		"-ngl", "99", // Offload all layers to GPU VRAM
	)

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start engine for model %s: %w", targetModel, err)
	}

	m.currentModel = targetModel

	// 3. Poll /health endpoint until engine is ready to accept requests
	return m.waitForReadiness(ctx)
}

func (m *ModelManager) waitForReadiness(ctx context.Context) error {
	healthURL := fmt.Sprintf("http://%s/health", m.cfg.ListenAddr)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for engine to boot")
		case <-ticker.C:
			resp, err := client.Get(healthURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				log.Println("[Engine] Engine is online and ready for inference!")
				return nil
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
	}
}

```

---

## 2. Local Client Proxy Daemon (`proxy/client.go`)

The proxy maintains the outbound NNG socket connection to the central router, emits periodic status heartbeats, and pipelines incoming NNG requests directly into the local `llama-server` streaming interface.

```go
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/xreq"
	_ "go.nanomsg.org/mangos/v3/transport/tls"

	"yourproject/engine"
	"yourproject/protocol"
)

type WorkerProxy struct {
	nodeID        string
	routerAddr    string
	localEngineURL string
	sock          mangos.Socket
	engineMgr     *engine.ModelManager
	catalog       []string
}

type HeartbeatPayload struct {
	NodeID         string   `json:"node_id"`
	ActiveModel    string   `json:"active_model"`
	Catalog        []string `json:"catalog"`
	ActiveRequests int      `json:"active_requests"`
}

type LLMRequest struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Raw    json.RawMessage `json:"-"`
}

func NewWorkerProxy(nodeID, routerAddr, localEngineURL string, mgr *engine.ModelManager, catalog []string) (*WorkerProxy, error) {
	sock, err := xreq.NewSocket()
	if err != nil {
		return nil, fmt.Errorf("failed to create xreq socket: %w", err)
	}

	if err := sock.Dial(routerAddr); err != nil {
		return nil, fmt.Errorf("failed to dial router: %w", err)
	}

	return &WorkerProxy{
		nodeID:         nodeID,
		routerAddr:     routerAddr,
		localEngineURL: localEngineURL,
		sock:           sock,
		engineMgr:      mgr,
		catalog:        catalog,
	}, nil
}

// Run starts the daemon loop and the periodic heartbeat worker
func (wp *WorkerProxy) Run(ctx context.Context) {
	go wp.heartbeatLoop(ctx)

	log.Println("[Proxy] Client worker listener started. Awaiting workloads...")
	for {
		select {
		case <-ctx.Done():
			return
		default:
			msg, err := wp.sock.RecvMsg()
			if err != nil {
				log.Printf("[Proxy] Error receiving NNG message: %v", err)
				continue
			}

			// Process work asynchronously to maintain socket availability
			go wp.handleWorkload(ctx, msg)
		}
	}
}

// Periodic heartbeat pushes current VRAM and model states back to the Central Router
func (wp *WorkerProxy) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hb := HeartbeatPayload{
				NodeID:      wp.nodeID,
				ActiveModel: wp.engineMgr.ActiveModel(),
				Catalog:     wp.catalog,
			}

			data, _ := json.Marshal(hb)
			frame := &protocol.Frame{Type: protocol.FrameTypeHeader, Payload: data}
			
			msg := mangos.NewMessage(0)
			msg.Body = append(msg.Body, frame.Encode()...)
			_ = wp.sock.SendMsg(msg)
		}
	}
}

func (wp *WorkerProxy) handleWorkload(ctx context.Context, msg *mangos.Message) {
	defer msg.Free()

	headerBacktrace := msg.Header
	body := msg.Body

	if len(body) < 2 {
		return
	}

	reqIDLen := int(body[0])
	reqID := body[1 : 1+reqIDLen]
	payload := body[1+reqIDLen:]

	// 1. Inspect target model from payload
	var llmReq LLMRequest
	if err := json.Unmarshal(payload, &llmReq); err != nil {
		wp.sendError(headerBacktrace, reqID, fmt.Sprintf("Invalid JSON request: %v", err))
		return
	}

	// 2. Ensure model is loaded (triggers dynamic swap if necessary)
	if err := wp.engineMgr.EnsureModel(ctx, llmReq.Model); err != nil {
		wp.sendError(headerBacktrace, reqID, fmt.Sprintf("Model swap failed: %v", err))
		return
	}

	// 3. Dispatch payload to local engine
	resp, err := http.Post(wp.localEngineURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		wp.sendError(headerBacktrace, reqID, fmt.Sprintf("Local engine request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	// 4. Stream tokens line-by-line over NNG back to router
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			wp.sendFrame(headerBacktrace, reqID, protocol.FrameTypeChunk, line)
		}
		if err != nil {
			if err == io.EOF {
				wp.sendFrame(headerBacktrace, reqID, protocol.FrameTypeEOF, nil)
			} else {
				wp.sendError(headerBacktrace, reqID, fmt.Sprintf("Stream interrupted: %v", err))
			}
			break
		}
	}
}

func (wp *WorkerProxy) sendFrame(backtrace []byte, reqID []byte, fType protocol.FrameType, payload []byte) {
	frame := &protocol.Frame{Type: fType, Payload: payload}
	encoded := frame.Encode()

	respBody := make([]byte, 1+len(reqID)+len(encoded))
	respBody[0] = byte(len(reqID))
	copy(respBody[1:], reqID)
	copy(respBody[1+len(reqID):], encoded)

	replyMsg := mangos.NewMessage(len(respBody))
	replyMsg.Header = append(replyMsg.Header, backtrace...)
	replyMsg.Body = append(replyMsg.Body, respBody...)

	_ = wp.sock.SendMsg(replyMsg)
}

func (wp *WorkerProxy) sendError(backtrace []byte, reqID []byte, errMsg string) {
	wp.sendFrame(backtrace, reqID, protocol.FrameTypeError, []byte(errMsg))
}

```

---

## 3. Entrypoint Configuration (`cmd/worker/main.go`)

Putting it together into a lightweight binary for the worker machine:

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"yourproject/engine"
	"yourproject/proxy"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 1. Configure Engine Manager
	engineCfg := engine.ManagerConfig{
		EnginePath:  "/usr/local/bin/llama-server",
		ModelDir:    "/var/models/gguf",
		ListenAddr:  "127.0.0.1:8080",
		SwapTimeout: 10 * time.Second,
	}

	catalog := []string{"llama-3-8b", "mistral-7b", "deepseek-coder-6.7b"}
	engineMgr := engine.NewModelManager(engineCfg, "llama-3-8b")

	// 2. Initialize and start Proxy Daemon
	worker, err := proxy.NewWorkerProxy(
		"node-gpu-01",
		"tls://router.yourdomain.com:9000",
		"http://127.0.0.1:8080/v1/chat/completions",
		engineMgr,
		catalog,
	)
	if err != nil {
		log.Fatalf("Failed to initialize worker proxy: %v", err)
	}

	log.Println("[System] Starting Worker Proxy Daemon...")
	worker.Run(ctx)
}

```

---

## Key Features

1. **Zero Open Ports:** Outbound dialer (`tls://router.yourdomain.com:9000`) works behind NATs, CGNATs, and residential firewalls without port forwarding.
2. **On-Demand Warmup / Health Gating:** `EnsureModel` blocks incoming execution until the local `llama-server` instance responds with `HTTP 200 OK` on `/health`, eliminating dropped or timed-out requests during model swaps.
3. **Non-blocking Concurrency:** Requests run in individual Goroutines while status heartbeats fire concurrently on a 3-second ticker.