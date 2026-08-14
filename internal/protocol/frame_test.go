package protocol

import (
	"bytes"
	"errors"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		typ     FrameType
		payload []byte
	}{
		{"header empty", FrameTypeHeader, nil},
		{"chunk small", FrameTypeChunk, []byte("data: {\"delta\":\"hi\"}\n\n")},
		{"chunk empty", FrameTypeChunk, nil},
		{"eof empty", FrameTypeEOF, nil},
		{"error message", FrameTypeError, []byte("local engine unreachable")},
		{"large payload", FrameTypeChunk, bytes.Repeat([]byte("x"), 1<<16)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &Frame{Type: tc.typ, Payload: tc.payload}
			enc := f.Encode()
			got, err := DecodeFrame(enc)
			if err != nil {
				t.Fatalf("DecodeFrame error: %v", err)
			}
			if got.Type != tc.typ {
				t.Errorf("type: got %d, want %d", got.Type, tc.typ)
			}
			if !bytes.Equal(got.Payload, tc.payload) {
				t.Errorf("payload mismatch: got %q, want %q", got.Payload, tc.payload)
			}
		})
	}
}

func TestDecodeFrameErrors(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		target error
	}{
		{"empty", []byte{}, ErrFrameTooShort},
		{"single byte", []byte{0x01}, ErrFrameTooShort},
		{"four bytes", []byte{0x01, 0, 0, 0}, ErrFrameTooShort},
		{"truncated payload", append([]byte{byte(FrameTypeChunk), 0, 0, 0, 5}, []byte("ab")...), ErrPayloadTruncated},
		{"oversized length", []byte{byte(FrameTypeChunk), 0xFF, 0xFF, 0xFF, 0xFF}, ErrPayloadTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeFrame(tc.data)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tc.target) {
				t.Errorf("error: got %v, want wrapping %v", err, tc.target)
			}
		})
	}
}

func TestDecodeFrameCopiesPayload(t *testing.T) {
	// DecodeFrame must copy the payload so the caller may free or reuse the
	// input buffer (mangos frees msg.Body after the receive loop returns).
	enc := (&Frame{Type: FrameTypeChunk, Payload: []byte("hello")}).Encode()
	got, err := DecodeFrame(enc)
	if err != nil {
		t.Fatalf("DecodeFrame error: %v", err)
	}
	for i := range enc {
		enc[i] = 0
	}
	if !bytes.Equal(got.Payload, []byte("hello")) {
		t.Errorf("payload aliased input: got %q after mutating source", got.Payload)
	}
}

func TestEncodeWireFormat(t *testing.T) {
	// Explicit byte layout: type, then big-endian uint32 length, then payload.
	f := &Frame{Type: FrameTypeChunk, Payload: []byte("ab")}
	got := f.Encode()
	want := []byte{byte(FrameTypeChunk), 0, 0, 0, 2, 'a', 'b'}
	if !bytes.Equal(got, want) {
		t.Errorf("wire format: got %v, want %v", got, want)
	}
}
