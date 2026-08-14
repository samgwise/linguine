Here is the complete **Database Markup Language (DBML)** schema for the Central Router & Administration Server.

You can paste this directly into [dbdiagram.io](https://dbdiagram.io) or use tools like `@dbml/cli` to auto-generate GORM/SQL migrations for SQLite (`modernc.org/sqlite`).

---

## Central Router Database Schema (`schema.dbml`)

```dbml
// ============================================================================
// Database: Central Router & Web Administration (SQLite / GORM)
// Description: Zero-Trust Distributed LLM Mesh & Gateway Management Schema
// ============================================================================

Project ZeroTrustLLMMesh {
  database_type: 'SQLite'
  Note: 'Schema for PASETO token management, node telemetry tracking, model alias mappings, and dynamic swap logs.'
}

// ----------------------------------------------------------------------------
// 1. Authentication & Access Control
// ----------------------------------------------------------------------------

Table api_keys {
  id          text [pk, note: "UUIDv4 or KSUID"]
  name        text [not null, note: "Human-readable label (e.g., 'Production App Key')"]
  token_hash  text [not null, unique, note: "Bcrypt or Argon2 hash of client API key"]
  prefix      text [not null, note: "First 8 characters for identification (e.g., 'sk-mesh-')"]
  role        text [not null, default: 'client', note: "Enum: 'admin', 'client'"]
  status      text [not null, default: 'active', note: "Enum: 'active', 'revoked', 'expired'"]
  expires_at  datetime [note: "NULL for non-expiring keys"]
  created_at  datetime [not null, default: `CURRENT_TIMESTAMP`]
  updated_at  datetime [not null, default: `CURRENT_TIMESTAMP`]

  Note: "Client credentials for authorizing requests against the OpenAI-compatible /v1 endpoints"
}

Table node_enrollment_tokens {
  id          text [pk, note: "UUIDv4 or KSUID"]
  token_paseto text [not null, unique, note: "Encrypted PASETO bearer token assigned to worker"]
  node_name   text [not null, note: "Expected hostname or identification label"]
  status      text [not null, default: 'active', note: "Enum: 'active', 'revoked', 'expired'"]
  expires_at  datetime [note: "Expiration timestamp for worker onboarding key"]
  created_at  datetime [not null, default: `CURRENT_TIMESTAMP`]

  Note: "Pre-shared enrollment keys generated via HTMX admin UI to authenticate outbound worker nodes"
}

// ----------------------------------------------------------------------------
// 2. Node Inventory & Telemetry
// ----------------------------------------------------------------------------

Table nodes {
  id             text [pk, note: "Assigned Node Identifier (e.g., 'node-gpu-melbourne')"]
  token_id       text [ref: > node_enrollment_tokens.id, note: "Enrollment token used during handshake"]
  status         text [not null, default: 'offline', note: "Enum: 'online', 'offline', 'draining', 'swapping'"]
  remote_addr    text [note: "Outbound IP/TLS endpoint seen by the router"]
  active_model   text [note: "Currently loaded model in GPU VRAM"]
  vram_total_mb  integer [not null, default: 0]
  vram_free_mb   integer [not null, default: 0]
  historical_tps float [not null, default: 0.0, note: "Moving average of Tokens Per Second"]
  last_heartbeat datetime [note: "Timestamp of last received telemetry frame"]
  created_at     datetime [not null, default: `CURRENT_TIMESTAMP`]
  updated_at     datetime [not null, default: `CURRENT_TIMESTAMP`]

  Note: "State registry of active GPU worker nodes connected over NNG"
}

Table node_model_catalogs {
  id             integer [pk, increment]
  node_id        text [not null, ref: > nodes.id, note: "Cascade delete when node is removed"]
  model_name     text [not null, note: "Model weights filename/ID available on local NVMe"]
  file_size_bytes integer [note: "Size on disk for load time estimation"]
  last_scanned_at datetime [not null, default: `CURRENT_TIMESTAMP`]

  Indexes {
    (node_id, model_name) [unique]
  }

  Note: "Catalog of models present on each node's local NVMe disk"
}

// ----------------------------------------------------------------------------
// 3. Model Registry & Routing Configuration
// ----------------------------------------------------------------------------

Table model_aliases {
  id             text [pk, note: "Public API Alias (e.g., 'gpt-4o-mini', 'coder-default')"]
  target_model   text [not null, note: "Actual model identifier on worker node (e.g., 'llama-3.1-8b-instruct')"]
  description    text [note: "Usage context displayed in HTMX UI"]
  is_enabled     boolean [not null, default: true]
  created_at     datetime [not null, default: `CURRENT_TIMESTAMP`]
  updated_at     datetime [not null, default: `CURRENT_TIMESTAMP`]

  Note: "Maps incoming OpenAI request model strings to backing GGUF/vLLM weights"
}

// ----------------------------------------------------------------------------
// 4. Metrics, Audit Logs & Model Swap Performance
// ----------------------------------------------------------------------------

Table model_swap_logs {
  id                integer [pk, increment]
  node_id           text [not null, ref: > nodes.id]
  from_model        text [not null]
  to_model          text [not null]
  swap_duration_ms  integer [not null, note: "Time spent unloading old weights and loading new GGUF"]
  trigger_reason    text [not null, note: "Enum: 'heuristic_switch', 'manual_override', 'idle_unload'"]
  status            text [not null, note: "Enum: 'success', 'timeout', 'failed'"]
  error_message     text
  created_at        datetime [not null, default: `CURRENT_TIMESTAMP`]

  Note: "Performance logs for model swaps used to refine heuristic penalty values (SwapPenalty_i)"
}

Table request_audit_logs {
  id               integer [pk, increment]
  api_key_id       text [ref: > api_keys.id, note: "Nullable for unauthenticated internal requests"]
  node_id          text [ref: > nodes.id, note: "Worker node that served the inference"]
  model_requested  text [not null]
  model_served     text [not null]
  prompt_tokens    integer [default: 0]
  completion_tokens integer [default: 0]
  ttft_ms          integer [note: "Time To First Token in milliseconds"]
  total_duration_ms integer [note: "Total duration of request"]
  was_streamed     boolean [not null, default: false]
  was_model_swapped boolean [not null, default: false]
  status_code      integer [not null, default: 200]
  created_at       datetime [not null, default: `CURRENT_TIMESTAMP`]

  Note: "Audit and usage metric history for dashboard charts and rate limiting"
}

```

---

## Key ER Diagrams & Relationship Summary

```
                      ┌──────────────────┐
                      │     api_keys     │
                      └────────┬─────────┘
                               │ 1:N
                               ▼
┌──────────────────┐  ┌──────────────────┐  ┌──────────────────────┐
│  model_aliases   │  │request_audit_logs│  │node_enrollment_tokens│
└──────────────────┘  └────────┬─────────┘  └──────────┬───────────┘
                               │                       │
                               │ N:1                   │ 1:1
                               ▼                       ▼
                      ┌──────────────────┐  ┌──────────────────────┐
                      │      nodes       │◄─┤ node_model_catalogs  │
                      └────────┬─────────┘  └──────────────────────┘
                               │ 1:N
                               ▼
                      ┌──────────────────┐
                      │ model_swap_logs  │
                      └──────────────────┘

```

### Architectural Details

1. **SQLite Optimization:**
* All primary keys default to string UUIDs/KSUIDs for `api_keys` and `nodes` to simplify multi-threaded insertion in Go without primary-key lock contention.
* Auto-increment integer PKs are used strictly for append-only log tables (`request_audit_logs`, `model_swap_logs`).


2. **Heuristic Calibration Loop:**
* The `model_swap_logs` table records exact durations (`swap_duration_ms`). The Go router queries this historical data at boot to replace static estimates (e.g., `DefaultSwapPenalty = 4s`) with rolling averages tailored to each node's actual NVMe load performance.


3. **OpenAI Compatibility Abstraction:**
* `model_aliases` allows your central server to expose OpenAI-compatible standard names (like `gpt-4o-mini`) while mapping them to specific weights (`llama-3.1-8b-instruct.gguf`) spread across worker nodes.