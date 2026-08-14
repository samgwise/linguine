package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// FrameType identifies the payload carried by a Frame.
type FrameType byte

const (
	// FrameTypeHeader carries metadata (for example an initial heartbeat or
	// response headers) sent before streaming chunks.
	FrameTypeHeader FrameType = 0x01
	// FrameTypeChunk carries a single SSE data line from the inference
	// engine's token stream.
	FrameTypeChunk FrameType = 0x02
	// FrameTypeEOF marks a clean end of stream.
	FrameTypeEOF FrameType = 0x03
	// FrameTypeError carries an error notification; the stream is aborted.
	FrameTypeError FrameType = 0xFF
)

// MaxPayloadLen is the largest payload a frame may carry. It guards against
// malformed length fields triggering oversized allocations on decode.
const MaxPayloadLen = 1 << 24 // 16 MiB

// Frame encapsulates a single message boundary on the NNG mesh.
type Frame struct {
	Type    FrameType
	Payload []byte
}

// headerSize is the fixed wire prefix: 1 byte type + 4 bytes length.
const headerSize = 5

// Encode converts a Frame into its wire format:
//
//	[1-byte Type][4-byte big-endian Length][Payload]
func (f *Frame) Encode() []byte {
	buf := make([]byte, headerSize+len(f.Payload))
	buf[0] = byte(f.Type)
	binary.BigEndian.PutUint32(buf[1:headerSize], uint32(len(f.Payload)))
	copy(buf[headerSize:], f.Payload)
	return buf
}

// ErrFrameTooShort is returned when input is smaller than the fixed header.
var ErrFrameTooShort = errors.New("protocol: frame too short")

// ErrPayloadTruncated is returned when the declared length exceeds the
// remaining input.
var ErrPayloadTruncated = errors.New("protocol: truncated payload")

// ErrPayloadTooLarge is returned when the declared length exceeds
// MaxPayloadLen.
var ErrPayloadTooLarge = errors.New("protocol: payload exceeds MaxPayloadLen")

// DecodeFrame unpacks wire data into a Frame. The payload is copied, so the
// caller may free or reuse the input buffer (important for mangos message
// lifecycles, where msg.Body is freed after the receive loop returns).
func DecodeFrame(data []byte) (*Frame, error) {
	if len(data) < headerSize {
		return nil, fmt.Errorf("%w: have %d bytes", ErrFrameTooShort, len(data))
	}
	fType := FrameType(data[0])
	length := binary.BigEndian.Uint32(data[1:headerSize])
	if length > MaxPayloadLen {
		return nil, fmt.Errorf("%w: declared %d bytes", ErrPayloadTooLarge, length)
	}
	if uint32(len(data)-headerSize) < length {
		return nil, fmt.Errorf("%w: expected %d, got %d", ErrPayloadTruncated, length, len(data)-headerSize)
	}
	payload := make([]byte, length)
	copy(payload, data[headerSize:headerSize+length])
	return &Frame{Type: fType, Payload: payload}, nil
}
