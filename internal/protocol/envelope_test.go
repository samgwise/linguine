package protocol

import (
	"bytes"
	"errors"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		reqID   string
		payload []byte
	}{
		{"simple", "req-123", []byte("hello world")},
		{"empty payload", "abc", nil},
		{"empty reqID", "", []byte("data")},
		{"long reqID", string(bytes.Repeat([]byte("r"), 200)), []byte("payload")},
		{"max reqID", string(bytes.Repeat([]byte("z"), MaxReqIDLen)), []byte("end")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &Envelope{ReqID: tc.reqID, Payload: tc.payload}
			enc := env.Encode()
			got, err := DecodeEnvelope(enc)
			if err != nil {
				t.Fatalf("DecodeEnvelope error: %v", err)
			}
			if got.ReqID != tc.reqID {
				t.Errorf("reqID: got %q, want %q", got.ReqID, tc.reqID)
			}
			if !bytes.Equal(got.Payload, tc.payload) {
				t.Errorf("payload mismatch: got %q, want %q", got.Payload, tc.payload)
			}
		})
	}
}

func TestDecodeEnvelopeErrors(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		target error
	}{
		{"empty", []byte{}, ErrEnvTooShort},
		{"truncated reqID", []byte{5, 'a', 'b'}, ErrReqIDTruncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeEnvelope(tc.data)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tc.target) {
				t.Errorf("error: got %v, want wrapping %v", err, tc.target)
			}
		})
	}
}

func TestDecodeEnvelopeCopiesPayload(t *testing.T) {
	enc := (&Envelope{ReqID: "x", Payload: []byte("keep")}).Encode()
	got, err := DecodeEnvelope(enc)
	if err != nil {
		t.Fatalf("DecodeEnvelope error: %v", err)
	}
	for i := range enc {
		enc[i] = 0
	}
	if !bytes.Equal(got.Payload, []byte("keep")) {
		t.Errorf("payload aliased input: got %q after mutating source", got.Payload)
	}
}

func TestEnvelopeComposesWithFrame(t *testing.T) {
	// A worker reply is an Envelope whose payload is an encoded Frame; this is
	// the exact layered structure used on the mesh.
	frame := &Frame{Type: FrameTypeChunk, Payload: []byte("data: {\"delta\":\"a\"}\n\n")}
	env := &Envelope{ReqID: "req-9", Payload: frame.Encode()}
	enc := env.Encode()

	gotEnv, err := DecodeEnvelope(enc)
	if err != nil {
		t.Fatalf("DecodeEnvelope error: %v", err)
	}
	gotFrame, err := DecodeFrame(gotEnv.Payload)
	if err != nil {
		t.Fatalf("DecodeFrame error: %v", err)
	}
	if gotFrame.Type != FrameTypeChunk {
		t.Errorf("frame type: got %d, want %d", gotFrame.Type, FrameTypeChunk)
	}
	if !bytes.Equal(gotFrame.Payload, frame.Payload) {
		t.Errorf("frame payload mismatch: got %q, want %q", gotFrame.Payload, frame.Payload)
	}
}
