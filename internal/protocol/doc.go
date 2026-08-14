// Package protocol defines the binary framing used to carry streamed
// OpenAI-compatible completions across the NNG mesh between the central
// router and worker nodes. A frame carries a typed payload (header, chunk,
// EOF or error) with a length-prefixed wire format.
package protocol
