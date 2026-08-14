# linguine

A self-hosted, zero-trust control plane for proxying local OpenAI-compatible
LLM endpoints to a secure public address. A single Go binary runs the
**router** (public `/v1` ingress + worker mesh); a second binary runs the
**worker** daemon on each GPU machine, dialling outbound so it works behind
NAT. An HTMX **admin dashboard** watches the fleet.

```
[ Client ] ── POST /v1/chat/completions ─▶ [ Router (public) ]
                                        │ NNG mesh (outbound-only)
                                        ▼
                              [ Worker ] ── proxy ─▶ [ local LLM ]
```

## Requirements

- Go 1.25+ (the toolchain auto-fetches the right version via `go.mod`)
- A local OpenAI-compatible endpoint on each worker machine
  (llama-server, Ollama, LM Studio, vLLM, etc.)
- A public host for the router (VPS / cloud instance / your own infra)
- A reverse proxy for the admin dashboard (Caddy, Nginx, Tailscale, etc.)

## Build

```sh
go build ./cmd/linguine   # router + admin CLI
go build ./cmd/worker    # worker daemon
```

Both binaries are self-contained (CGO-free SQLite via `modernc.org/sqlite`).

## Quick start (single machine)

Run the router and one worker on the same machine to prove the path
end-to-end before deploying across machines.

### 1. Configure the router

`router.toml`:

```toml
[http]
listen = "0.0.0.0:8443"   # public /v1 ingress

[db]
path = "linguine.db"

[signer]
key_path = "linguine-signer.key"

[nng]
listen = "tcp://0.0.0.0:9000"   # worker mesh (workers dial this)
# For production, use TLS and provide cert/key:
#   listen = "tls+tcp://0.0.0.0:9000"
#   [nng.tls]
#   cert_file = "/etc/linguine/nng-cert.pem"
#   key_file  = "/etc/linguine/nng-key.pem"

[admin]
listen = "127.0.0.1:8444"            # localhost-only; reverse proxy fronts this
session_secret_path = "linguine-admin.key"
```

Every field has a `LINGUINE_ROUTER_*` env override, so a config file is
optional — env vars alone are enough for containerised deploys.

### 2. Create credentials

The router creates its signer key and admin session secret automatically on
first run (persisted to the configured paths). Create one client API key
(for `/v1` callers) and one admin API key (for dashboard login):

```sh
LINGUINE_ROUTER_DB_PATH=./linguine.db \
LINGUINE_ROUTER_SIGNER_KEY_PATH=./linguine-signer.key \
LINGUINE_ROUTER_ADMIN_SESSION_SECRET_PATH=./linguine-admin.key \
linguine admin create-key --name "prod-app"          # client key (sk-mesh-…)
linguine admin create-key --name "operator" --role admin  # admin key (sk-mesh-…)
```

Each key is shown **once** — store it securely immediately.

### 3. Create a worker enrollment token

```sh
linguine admin create-enrollment-token --node node-gpu-sydney
# Enrollment token (v4.public.…) — pass this to the worker machine
```

### 4. Run the router

```sh
linguine --config router.toml serve
# router HTTP on 0.0.0.0:8443, mesh on tcp://0.0.0.0:9000
# admin HTTP on 127.0.0.1:8444
```

### 5. Run the worker

`worker.toml` (on the GPU machine):

```toml
node_id = "node-gpu-sydney"
enrollment_token = "v4.public.…from step 3…"

[router]
nng_addr = "tls+tcp://router.example.com:9000"  # the router's NNG listen addr
# tls_ca_file = "/etc/linguine/router-ca.pem"   # for tls+tcp://

[engine]
url = "http://127.0.0.1:8080/v1/chat/completions"  # local LLM endpoint
```

```sh
linguine-worker --config worker.toml
# worker node-gpu-sydney connected, proxying to http://127.0.0.1:8080
```

The worker probes the local engine's `/v1/models` and advertises the catalog
in its heartbeats, so the router knows what each node can serve.

### 6. Call it

```sh
curl https://router.example.com/v1/chat/completions \
  -H "Authorization: Bearer sk-mesh-…" \
  -H "Content-Type: application/json" \
  -d '{"model":"llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

Streaming (`"stream": true`) returns SSE chunks; non-stream returns JSON.

## Admin dashboard

The dashboard listens on `127.0.0.1:8444` and is meant to sit behind a
reverse proxy that terminates TLS. It is **not** exposed on the public
ingress.

### Caddy

```caddyfile
admin.linguine.example.com {
    reverse_proxy 127.0.0.1:8444
}
```

### Nginx

```nginx
server {
    listen 443 ssl;
    server_name admin.linguine.example.com;
    ssl_certificate     /etc/ssl/linguine.pem;
    ssl_certificate_key /etc/ssl/linguine.key;

    location / {
        proxy_pass http://127.0.0.1:8444;
        proxy_set_header Host $host;
    }
}
```

### Tailscale (no reverse proxy)

Serve the dashboard over the tailnet without a public address:

```sh
linguine --config router.toml serve   # admin on 127.0.0.1:8444
# From your laptop on the tailnet:
#   http://<router-host>:8444/admin
```

Sign in at `/admin/login` with the **admin API key** (the one created with
`--role admin`). The session cookie lasts 12 hours.

Pages: dashboard home (fleet summary), node inventory (auto-refreshes every
5s), node detail, request audit log.

## Operating multiple workers

Repeat steps 3 and 5 for each GPU machine with a unique `node_id` and its
own enrollment token. The router's least-connections selection routes each
request to the node with the fewest in-flight jobs, so adding workers scales
throughput without config changes on the router.

To retire a worker, stop the daemon and revoke its enrollment token:

```sh
# (planned: admin revoke-enrollment-token --id <token-id>)
# For now, flip the row directly:
sqlite3 linguine.db "UPDATE node_enrollment_tokens SET status='revoked' WHERE id='<id>'"
```

## Security notes

- `/v1` requires a client API key (`Authorization: Bearer sk-mesh-…`).
- Workers enrol via a PASETO v4 token and dial **outbound** — no inbound
  ports needed on worker machines, so they sit safely behind NAT.
- The admin dashboard is localhost-only by design; expose it only through
  your own TLS-terminating reverse proxy or a private network.
- The router persists an Ed25519 signer key and an admin session secret;
  rotate both to invalidate outstanding enrollment tokens and dashboard
  sessions respectively.

## What's next

- **Phase 1b** (not yet built): cost-aware hot/cold model swapping across
  the fleet — the router will decide whether to wait for a hot model or
  trigger a swap on an idle node, based on a `WaitTime + SwapPenalty` score.
- **Phase 2**: capability-based routing, session affinity/KV-cache,
  failure frames, Prometheus/OpenTelemetry observability.
- **Phase 3**: multi-tenant quotas, multi-GPU topology, self-tuning
  residency.
