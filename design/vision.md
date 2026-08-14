Based on these updates, here is the refined **Architecture & Load-Balancing Specification** for the zero-trust mesh.

---

# Technical Specification: SQLite, SSE & Dynamic Model Mesh

Initial vision for the linguine local lab control plane

## 1. Single-Binary Embedded Architecture

By utilizing **SQLite** via `modernc.org/sqlite` (pure Go, CGO-free), the entire Central Control Plane compiles into a single, self-contained binary with zero external runtime dependencies.

```
┌────────────────────────────────────────────────────────────────────────┐
│                        Go Router & Web Server                          │
│                                                                        │
│   ┌────────────────────────────────┐   ┌───────────────────────────┐   │
│   │   OpenAI Ingress API (/v1)     │   │   HTMX Web Admin Console  │   │
│   │   (SSE Chunking Middleware)    │   │ (ColaFanta Fiber v3 Stack)│   │
│   └───────────────┬────────────────┘   └─────────────┬─────────────┘   │
│                   │                                  │                 │
│                   ▼                                  ▼                 │
│   ┌────────────────────────────────────────────────────────────┐       │
│   │       Embedded SQLite Engine (modernc.org/sqlite)          │       │
│   │   (Node Inventory, Token Auth, Model Swap Cost Metrics)   │       │
│   └───────────────────────────────┬────────────────────────────┘       │
│                                   │                                    │
│                                   ▼                                    │
│            ┌──────────────────────────────────────────────┐            │
│            │   NNG Central Router (mangos - REQ/REP)     │            │
│            └───────────────────────▲──────────────────────┘            │
└────────────────────────────────────┼───────────────────────────────────┘
                                     │ Outbound TLS (Zero Inbound Ports)
                                     │ Streamed Chunk Framing

```

---

## 2. SSE Streaming Architecture over NNG Mesh

To support OpenAI-compatible streaming (`stream=true`), token chunks are framed into NNG messages and proxied directly from the local inference engine (`llama.cpp` / `vLLM`) through the NNG worker back to the client.

### Token Streaming Packet Flow

```
[ Client ]          [ Go Central Router ]         [ NNG Worker Daemon ]     [ Local Engine ]
    │                         │                            │                       │
    │── POST /v1/chat ───────>│                            │                       │
    │   {"stream": true}      │── Send Request ───────────>│                       │
    │                         │   (NNG REQ)                │── POST /completion ──>│
    │                         │                            │   (Local HTTP)        │
    │                         │                            │                       │
    │<── 200 OK (SSE) ────────│                            │<── Token Stream ──────│
    │    Content-Type:        │                            │    (Chunk 1)          │
    │    text/event-stream    │<── NNG Chunk Frame ────────│                       │
    │                         │                            │                       │
    │<── data: {"delta"...} ──│                            │                       │
    │                         │<── NNG Chunk Frame ────────│<── Token Stream ──────│
    │<── data: {"delta"...} ──│                            │    (Chunk 2)          │
    │                         │                            │                       │
    │<── data: [DONE] ────────│<── NNG EOF Signal ─────────│<── HTTP EOF ──────────│

```

### Protocol Implementation Details

1. **Header Handshake:** When a stream starts, the NNG Worker sends a lightweight metadata frame to the Central Router containing initial HTTP status and headers before token streams arrive.
2. **Chunk Framing:** Subsequent NNG frames wrap individual Server-Sent Event lines (`data: {...}\n\n`).
3. **Keep-Alive & EOF:** An explicit EOF control frame terminates the NNG transaction, signaling the Fiber v3 handler to close the HTTP chunked writer safely.

---

## 3. Dynamic Model Switching & Heuristic Load Balancing

Nodes advertise both their **currently loaded hot model** and their **available on-disk model catalog**. The Central Router maintains a cost-aware routing algorithm to decide whether to route to a hot model node or trigger a dynamic swap on an idle node.

### Node Heartbeat State (Advertised to Router)

Each worker periodically transmits its status over the NNG socket:

```json
{
  "node_id": "gpu-node-01",
  "active_model": "llama-3.1-8b-instruct",
  "catalog": ["llama-3.1-8b-instruct", "mistral-7b-v0.3", "deepseek-coder-6.7b"],
  "vram_total_mb": 24576,
  "vram_free_mb": 18200,
  "active_requests": 2,
  "estimated_tps": 42.5,
  "active_conversations": 35,
  "cached_tokens": 2100000,
  "pinned_sessions": ["abc123", "def456"]
}

```

---

### Heuristic Decision Matrix

When a request arrives for target model $M$:

```
                       [ Incoming Request for Model M ]
                                      │
               ┌──────────────────────┴──────────────────────┐
               ▼                                             ▼
     [ Nodes with M Loaded ]                      [ Nodes with M on Disk ]
    (Hot State: Cost = 0s)                   (Cold State: Cost = Swap Penalty)
               │                                             │
               ▼                                             ▼
   Calculate Total Latency:                      Calculate Swap Latency:
   T_hot = Queued_Tokens / TPS                   T_cold = T_unload + T_load
               │                                             │
               └──────────────────────┬──────────────────────┘
                                      │
                                      ▼
                        Select Min(T_hot, T_cold)

```

---

### Load Balancer Cost Formula

The router assigns a **Score** $S_i$ to each node $i$, picking the lowest score:

$$S_i = \text{WaitTime}_i + \text{SwapPenalty}_i + \text{VRAMPenalty}_i - \text{CacheAffinity}_i$$

1. **Active Queue Wait Time ($\text{WaitTime}_i$):**

$$\text{WaitTime}_i = \frac{\sum \text{Remaining Tokens in Queue}_i}{\text{Historical TPS}_i}$$



*(If Node $i$ already has model $M$ loaded and an empty queue, $\text{WaitTime}_i = 0$.)*
2. **Model Swap Penalty ($\text{SwapPenalty}_i$):**
* **Model $M$ is currently loaded:** $0\text{ ms}$
* **Model $M$ is on disk (Requires Unload/Reload):**

$$\text{SwapPenalty}_i = T_{\text{unload}} + T_{\text{load\_disk\_to\_vram}}$$



*(Typically $3,000\text{ ms}$ to $8,000\text{ ms}$ depending on GGUF size and NVMe speeds).*
* **Model $M$ not on disk:** $\infty$ (Node excluded)


3. **KV-Cache Affinity Bonus ($\text{CacheAffinity}_i$):**
A negative cost (bonus) subtracted when Node $i$ already holds the KV cache for the request's session, avoiding prompt reprocessing replay (see §6).

$$\text{CacheAffinity}_i = \begin{cases} T_{\text{replay}}(\text{cached\_tokens}_i) & \text{if session cache resident on } i \\ 0 & \text{otherwise} \end{cases}$$


4. **Throttling Avoidance Rule:**
If a hot node's estimated queue wait time $\text{WaitTime}_{\text{hot}}$ exceeds an idle cold node's swap penalty $\text{SwapPenalty}_{\text{cold}}$, the router orders the cold node to **swap models and take the work**.

---

## 4. Model Swap Lifecycle Control (Worker Side)

When the router routes a job requiring model $M$ to a worker running model $N$:

1. **Instruct Swap:** The worker receives a payload containing `{"action": "swap_and_run", "model": "mistral-7b"}`.
2. **Graceful Drain:** The worker pauses new incoming NNG slots and lets active requests on model $N$ complete.
3. **Local Engine Re-exec / API Reload:**
* **`llama.cpp` (`llama-server`):** The worker daemon issues an API call to `/props` / `/models/load` or restarts `llama-server` with `--model /path/to/mistral-7b.gguf`.
* **`vLLM`:** The worker invokes the local `/reload` endpoint or re-executes the Python process with new model flags.


4. **Ack & Execute:** Once loaded into VRAM, the worker acknowledges readiness over NNG and begins streaming tokens.

---

## 5. Capability-Based Routing

Routing strictly by model name couples clients to a specific weight file. As the fleet's catalog evolves, operators want to retire or swap models without breaking callers, so the ingress accepts an optional **capability profile** and resolves it to a candidate model set before invoking the cost-aware balancer in §3. This layer sits upstream of node selection: capability resolution decides *which models can serve the request*, and §3 decides *which node serves it*.

### Request Shape

A client sends either `model` (exact, OpenAI-standard) or `requirements`:

```json
{
  "requirements": {
    "coding": true,
    "vision": false,
    "context": 128000,
    "reasoning": "high"
  }
}
```

### Capability Profile Registry

A profile registry persisted in SQLite maps a named profile to an ordered list of candidate models plus hard constraints:

```json
{
  "profile": "coding-small",
  "candidates": ["deepseek-coder-6.7b", "qwen-coder-7b", "llama-coder-7b"],
  "constraints": { "min_context": 32768, "vision": false }
}
```

### Resolution Flow

```
[ Client Request ]
   │  { requirements }  or  { model }
   ▼
[ Capability Resolver ] ── exact model? ──► use directly
   │  else match profile
   ▼
[ Candidate Model Set ]
   ▼
[ §3 Cost-Aware Balancer ] (picks lowest-score node across all candidates)
```

When several candidates satisfy a profile, the §3 balancer runs across the union of eligible nodes for every candidate and the lowest-score node wins. A request therefore lands on whichever hot node satisfies the capability, rather than waiting on one named model.

---

## 6. Session Affinity & KV-Cache Awareness

Re-processing a long prompt on every turn is the dominant latency cost in multi-turn traffic. Once a conversation's KV cache is resident on a node, subsequent turns should pin to that node until the cache expires.

### Session Affinity

A `session_id` (derived from a client-supplied `chat_id`, or hashed from the API key plus conversation prefix) travels with each request. The router keeps an affinity table in SQLite:

```json
{
  "session_id": "abc123",
  "node_id": "gpu-node-01",
  "model": "llama-3.1-8b-instruct",
  "last_seen": 1723298400,
  "expires_at": 1723302000
}
```

Routing decision:

1. If `session_id` has a live affinity entry and the node is healthy, route directly and skip swap.
2. If the entry has expired (idle timeout) or the node is gone, fall back to §3 balancing and re-establish affinity on the chosen node.

### KV-Cache Awareness

Workers advertise cache state in each heartbeat (see §3 Node Heartbeat State: `active_conversations`, `cached_tokens`, `pinned_sessions`). When a session must re-route — say its pinned node died — the balancer prefers a node that already holds an overlapping cache prefix, applying the `CacheAffinity` bonus from the §3 score formula.

### Affinity vs. Load Trade-off

Affinity is not absolute. If a pinned node's queue wait exceeds the replay cost of repopulating the cache elsewhere, the router may break affinity and re-pin to a cooler node. This mirrors the hot/cold swap rule in §3, generalized from models to sessions.

---

## 7. Failure Domains & Recovery Semantics

The mesh must degrade gracefully. Three failure classes are handled explicitly.

### Router Restart / SQLite Loss

The router is stateless apart from SQLite. On restart it rebuilds its live view from worker heartbeats; in-flight SSE streams are dropped and clients receive a standard error event. SQLite runs in WAL mode with periodic checkpointing, so a clean crash is recoverable from the WAL. Token auth and the session affinity table are the only router-owned persistent state, and both are read back from disk on boot.

### Worker Disappears Mid-Stream

A worker's NNG socket dying mid-stream must produce a clean SSE termination, not a hang. The router wraps each proxied stream with a watchdog:

```
[ Router streaming to client ]      [ Worker NNG socket dies ]
        │                                      │
        ▼                                      ▼
   watchdog timeout ──► emit data: {"error":{"code":"node_lost"}}
                        emit data: [DONE]
                        close HTTP chunked writer
```

The normal EOF path is an explicit `{"state": "aborted"}` NNG control frame from the worker; the watchdog covers the case where the worker cannot send it.

### Split Brain (Future: Multi-Router)

Single-router is the initial deployment. If multiple routers are introduced, they must not double-schedule. The schema reserves a `router_epoch` field on every NNG instruction plus an advisory lock table in SQLite for leader election; workers reject instructions carrying a stale epoch. This is stubbed in the schema now and activated only when high availability is required.

---

## 8. Observability

From day one the router exposes a `/metrics` endpoint in Prometheus text format and emits OpenTelemetry spans for each request lifecycle.

### Metrics

| Metric | Type | Source |
|---|---|---|
| `linguine_requests_total` | counter | ingress |
| `linguine_tokens_generated_total` | counter | NNG chunk frames |
| `linguine_queue_wait_seconds` | histogram | router |
| `linguine_model_swaps_total` | counter | worker |
| `linguine_swap_duration_seconds` | histogram | worker |
| `linguine_stream_disconnects_total` | counter | router watchdog |
| `linguine_failed_generations_total` | counter | router / worker |

### Traces

Each request yields a trace rooted at the ingress span, with child spans for capability resolution, node selection, the NNG round-trip, and per-chunk streaming. `session_id` and `node_id` are attached as span attributes so cache-miss replays and model swaps are visible in trace waterfalls.

---

## Implementation Roadmap

```
1. SQLite Schema Setup (Tokens, Nodes, Swap Logs, Sessions, KV-Cache Stats)
2. NNG Mangos REQ/REP SSE Framing Prototype
3. Worker Engine Adapter (llama-server Lifecycle Manager)
4. Heuristic Router Implementation (Cost Score + Cache-Affinity Bonus)
5. HTMX Dashboard Integration (ColaFanta Templ UI)
6. Capability Profile Registry & Requirement→Model Resolution
7. Session Affinity Table & KV-Cache-Aware Re-Routing
8. Failure Handling (Abort/Node-Lost Frames, Router Restart Recovery, Worker Watchdog)
9. Observability Layer (Prometheus /metrics, OpenTelemetry Traces)

```