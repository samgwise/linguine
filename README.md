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
listen = "ws://127.0.0.1:9000/mesh"   # loopback; front /mesh with a TLS reverse proxy
# To terminate mesh TLS in-process instead (no reverse proxy), use wss:// and
# provide cert/key. If both are omitted the router auto-generates a
# self-signed cert on first run and logs its fingerprint for workers to pin:
#   listen = "wss://0.0.0.0:9000/mesh"
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
# router HTTP on 0.0.0.0:8443, mesh on ws://127.0.0.1:9000/mesh
# admin HTTP on 127.0.0.1:8444
```

### 5. Run the worker

`worker.toml` (on the GPU machine):

```toml
node_id = "node-gpu-sydney"
enrollment_token = "v4.public.…from step 3…"

[router]
nng_addr = "wss://router.example.com/mesh"  # the router's /mesh endpoint
# tls_ca_file = "/etc/linguine/router-ca.pem"    # for a private CA; omit for a public CA
# tls_fingerprint = "sha256/…="                  # pin a self-signed router cert (TOFU)
# http_proxy = "http://corp-proxy:3128"          # egress via a corporate CONNECT proxy
#                                               # (omit to use HTTPS_PROXY/HTTP_PROXY env)

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
5s), node detail, request audit log (request history plus admin auth events).

## Production ingress (one 443)

The recommended deployment fronts `/v1`, `/admin`, and the worker mesh on a
single 443 with one TLS cert. The router keeps all three listeners on loopback
(`http.listen` should be `127.0.0.1:8443` here so API keys never cross the
public network in the clear) and nginx terminates TLS once.

```nginx
server {
    listen 443 ssl;
    server_name router.example.com;
    ssl_certificate     /etc/letsencrypt/live/router.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/router.example.com/privkey.pem;

    # OpenAI-compatible ingress. Disable buffering and allow long streams.
    location /v1/ {
        proxy_pass http://127.0.0.1:8443;
        proxy_set_header Host $host;
        proxy_buffering off;
        proxy_read_timeout 1h;
    }

    # Worker mesh — WebSocket upgrade to the ws:// loopback listener.
    location /mesh {
        proxy_pass http://127.0.0.1:9000/mesh;
        proxy_set_header Host $host;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 1h;
    }

    # Admin dashboard.
    location /admin/ {
        proxy_pass http://127.0.0.1:8444;
        proxy_set_header Host $host;
    }
}
```

With this setup a worker needs only its URL and enrollment token — no TLS
files — because `wss://router.example.com/mesh` verifies against the public
CA via system roots. If you terminate mesh TLS in-process instead (`wss://`
with `nng.tls`), run `linguine serve` once to print the auto-generated
cert's fingerprint and set `router.tls_fingerprint` on each worker.

If you enable built-in `http.tls` instead of proxying `/v1`, note the cert is
loaded once at startup: add a `--deploy-hook` to your certbot renewal to
restart the `linguine` service so renewed certs are picked up.

## Worker proxy egress

If a worker sits behind a corporate HTTP proxy, point it at the mesh via
`router.http_proxy` (or the standard `HTTPS_PROXY` / `NO_PROXY` environment
variables). The worker tunnels the `wss://` dial through the proxy using HTTP
CONNECT. (mangos' built-in WebSocket dialer ignores proxy env vars, so
linguine vendors a patched ws transport that honours them.)

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
  Front `/v1` with TLS (a reverse proxy or `http.tls`) so keys are never sent
  in cleartext; the router warns at startup if `/v1` is non-loopback with TLS
  off.
- Workers enrol via a PASETO v4 token and dial **outbound** — no inbound
  ports needed on worker machines, so they sit safely behind NAT.
- The worker mesh runs over `wss://` (TLS) — either terminated by your
  reverse proxy on 443 or in-process with `nng.tls`. A plaintext `ws://`
  listener must stay loopback; the router warns if it is exposed.
- The admin dashboard is localhost-only by design; expose it only through
  your own TLS-terminating reverse proxy or a private network.
- The router persists an Ed25519 signer key and an admin session secret;
  rotate both to invalidate outstanding enrollment tokens and dashboard
  sessions respectively. Revoking an admin key also kills its dashboard
  sessions immediately.

## What's next

- **Phase 1b** (not yet built): cost-aware hot/cold model swapping across
  the fleet — the router will decide whether to wait for a hot model or
  trigger a swap on an idle node, based on a `WaitTime + SwapPenalty` score.
- **Phase 2**: capability-based routing, session affinity/KV-cache,
  failure frames, Prometheus/OpenTelemetry observability.
- **Phase 3**: multi-tenant quotas, multi-GPU topology, self-tuning
  residency.
