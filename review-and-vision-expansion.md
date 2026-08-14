This is a solid vision. My first reaction is that you're not really designing an LLM router anymore, you're designing a distributed inference control plane. The architecture is actually closer to a lightweight, self-hosted version of what systems like Kubernetes + KServe, Ray Serve, or commercial LLM gateways provide, but heavily optimized for homelab/small-cluster deployment.

I've reviewed the document (vision.md).

Has this been done before?
Partially, yes

There are several projects that overlap with individual pieces of your design:

Project	Similarities	Missing vs Your VisionLiteLLM	OpenAI-compatible proxy, routing, model abstraction	No distributed model swap scheduling
OpenRouter	Multi-model routing	Cloud-focused, not local mesh
vLLM	High-performance inference serving	Not a distributed control plane
Ray Serve	Distributed model deployment	Much heavier operational footprint
KServe	Dynamic model loading	Requires Kubernetes
Ollama	Local model lifecycle management	Single-node focus
LocalAI	OpenAI API compatibility	Limited distributed scheduling
Oobabooga + llama.cpp fleets	Distributed inference nodes	Typically lack central scheduling intelligence

The specific combination of:

OpenAI-compatible ingress
Self-contained Go binary
SQLite control plane
Outbound-only workers
Dynamic model hot/cold swapping
Cost-aware scheduling
SSE proxying across the mesh

is something I haven't seen packaged exactly this way.

The closest conceptual comparison is probably:

"LiteLLM gateway + Nomad scheduler + Ollama model manager compressed into one binary."

What I think is missing
1. Capability-based routing

Currently you route by model name.

Eventually you'll want:

{
  "requirements": {
    "coding": true,
    "vision": false,
    "context": 128000,
    "reasoning": "high"
  }
}


instead of

{
  "model": "deepseek-coder-6.7b"
}


This lets operators swap models without breaking clients.

The router maps:

coding-small


to

deepseek-coder
qwen-coder
llama-coder


depending on availability.

Every serious LLM gateway eventually adds this abstraction layer.

2. Multi-GPU topology awareness

Your heartbeat tracks VRAM.

You may also want:

{
    "gpu_count": 2,
    "interconnect": "nvlink",
    "numa_group": 0
}


because:

Mixtral
DeepSeek
future MoE models
huge context models

often need different scheduling decisions than small instruct models.

3. Context-aware scheduling

Current score is:

queue_wait + swap_cost


but inference latency is dominated by:

prompt_tokens
generation_tokens
context_length


Example:

Node A:

model loaded
queue 15 seconds

Node B:

swap cost 10 seconds

If request contains:

400k context


Node A may still be better.

You'll eventually want:

score =
queue_time +
swap_penalty +
estimated_prompt_processing +
estimated_generation_time

4. Session affinity

One thing missing from the roadmap is conversation stickiness.

For example:

chat_id = abc123


If turn 2 of the conversation gets routed somewhere else:

KV cache lost
prompt replay required
latency spikes

A huge performance win is:

chat session
  -> node affinity


until idle timeout expires.

This is probably the biggest omission.

5. KV-cache awareness

The next step after session affinity.

Worker advertises:

{
  "active_conversations": 35,
  "cached_tokens": 2100000
}


Router prefers nodes already holding the cache.

This is a major optimization used by modern inference systems.

6. Failure domains

Your design describes routing but not failure handling.

Questions I'd answer:

Router restart

What happens if SQLite disappears?

Worker disappears mid-stream

You need:

{
  "state": "aborted"
}


and clean SSE termination.

Split brain

If you ever run multiple routers:

leader election
shared state
heartbeats

become relevant.

7. Authentication model

You mention token auth in SQLite.

I'd think further about:

User
sam

API Key
sk_xxx

Organization
design-lab

Quotas
100M tokens/day


This becomes important very quickly if multiple people share the cluster.

8. Model lifecycle policies

Right now the heuristic is reactive:

request arrives → decide swap.

You may also want predictive behavior:

last 24h demand


to decide:

models that remain resident
models eligible for eviction
prewarming

Essentially an LRU/LFU cache for VRAM.

9. Observability

I would add from day one:

Prometheus metrics


and maybe

OpenTelemetry traces


Metrics you'll want:

requests/sec
tokens/sec
queue wait
model swap count
swap duration
stream disconnects
failed generations

You'll thank yourself later.

10. Queueing model

REQ/REP is simple, but eventually becomes limiting.

You may discover you need:

Router
  -> dispatch queue

Workers
  -> consume when ready


rather than strict request-response ownership.

Especially once one worker is streaming and another becomes available.

I wouldn't change it yet, but I'd keep an eye on it.

The thing I think is genuinely novel

The most interesting part isn't the gateway or SQLite.

It's this concept:

Treat GPU VRAM as a distributed cache of models and schedule work based on whether waiting for a hot model is cheaper than loading a cold model.

That's much closer to how an operating system thinks about memory than how most local-LLM projects think about inference.

Most existing systems either:

pin models permanently, or
use manual deployment.

Your design tries to automatically optimize loaded-model placement across the fleet.

That could be genuinely valuable for small labs, universities, and homelabs where there are many models and not enough VRAM to keep everything resident.

My overall assessment

I'd rate the architecture:

Technical feasibility: 9/10
Complexity: 7/10
Novelty: 7.5/10
Chance people would use it: 8/10

The biggest missing features are:

Session affinity
KV-cache awareness
Capability-based routing
Observability
Failure/recovery semantics

Without those it's a good router. With those it's a serious distributed inference platform.