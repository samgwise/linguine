// Package engine abstracts the local inference backend. The ProxyEngine
// forwards to an already-running OpenAI-compatible endpoint; a future
// ManagedEngine will own the engine process lifecycle and model swaps.
package engine
