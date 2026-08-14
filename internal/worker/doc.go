// Package worker implements the worker daemon: an outbound NNG dial to the
// central router, periodic heartbeats, and proxying of dispatched requests
// to a local OpenAI-compatible inference endpoint with token streams framed
// back across the mesh.
package worker
