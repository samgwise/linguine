package protocol

import (
	"errors"
	"fmt"
)

// MaxReqIDLen is the largest reqID an Envelope can carry. The wire format
// prefixes the reqID with a single length byte, so reqIDs cannot exceed 255
// bytes; request-id generators must stay within this bound.
const MaxReqIDLen = 255

// Envelope wraps a payload with a request id so the central router can
// correlate multiplexed streaming replies from workers. This encoding sits
// inside the NNG message body; the NNG routing header (the worker's pipe id)
// is carried separately by mangos in msg.Header and is not part of this
// envelope.
//
// A worker reply is an Envelope whose payload is an encoded Frame; a router
// dispatch is an Envelope whose payload is the raw request body.
type Envelope struct {
	ReqID   string
	Payload []byte
}

// Encode converts an Envelope into its wire format:
//
//	[1-byte ReqIDLen][ReqID bytes][Payload bytes]
//
// ReqIDs longer than MaxReqIDLen would be silently truncated by the length
// byte; callers must keep reqIDs within the limit.
func (e *Envelope) Encode() []byte {
	reqID := []byte(e.ReqID)
	buf := make([]byte, 1+len(reqID)+len(e.Payload))
	buf[0] = byte(len(reqID))
	copy(buf[1:], reqID)
	copy(buf[1+len(reqID):], e.Payload)
	return buf
}

// ErrEnvTooShort is returned when input has no length byte.
var ErrEnvTooShort = errors.New("protocol: envelope too short")

// ErrReqIDTruncated is returned when the declared reqID length exceeds the
// remaining input.
var ErrReqIDTruncated = errors.New("protocol: truncated reqID")

// DecodeEnvelope unpacks wire data into an Envelope. The payload is copied so
// the caller may free or reuse the input buffer (mangos frees msg.Body after
// the receive loop returns).
func DecodeEnvelope(data []byte) (*Envelope, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("%w: have %d bytes", ErrEnvTooShort, len(data))
	}
	reqIDLen := int(data[0])
	if len(data) < 1+reqIDLen {
		return nil, fmt.Errorf("%w: expected %d, got %d", ErrReqIDTruncated, reqIDLen, len(data)-1)
	}
	reqID := string(data[1 : 1+reqIDLen])
	payload := make([]byte, len(data)-1-reqIDLen)
	copy(payload, data[1+reqIDLen:])
	return &Envelope{ReqID: reqID, Payload: payload}, nil
}
