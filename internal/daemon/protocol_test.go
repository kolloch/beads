package daemon

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Request{Version: ProtocolVersion, Op: OpReady, Params: json.RawMessage(`{"filter":{"limit":5}}`)}
	if err := writeMessage(&buf, &in); err != nil {
		t.Fatalf("writeMessage: %v", err)
	}
	var out Request
	if err := readMessage(&buf, &out); err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if out.Version != in.Version || out.Op != in.Op || string(out.Params) != string(in.Params) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestMultipleMessagesOnOneStream(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 3; i++ {
		if err := writeMessage(&buf, &Response{OK: true, Result: json.RawMessage(`123`)}); err != nil {
			t.Fatalf("writeMessage %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		var r Response
		if err := readMessage(&buf, &r); err != nil {
			t.Fatalf("readMessage %d: %v", i, err)
		}
		if !r.OK || string(r.Result) != "123" {
			t.Fatalf("message %d: got %+v", i, r)
		}
	}
	// A clean read at the boundary after the last message must report io.EOF so
	// the server's read loop knows the peer closed.
	var r Response
	if err := readMessage(&buf, &r); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF at end of stream, got %v", err)
	}
}

func TestReadMessageRejectsOversizeFrame(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], maxMessageBytes+1)
	err := readMessage(bytes.NewReader(header[:]), &Request{})
	if err == nil {
		t.Fatal("expected oversize frame to be rejected")
	}
	// The body must not be read/allocated; the guard fires on the length alone.
	if got := err.Error(); !contains(got, "too large") {
		t.Fatalf("expected 'too large' error, got %q", got)
	}
}

func TestReadMessageTruncatedBodyIsError(t *testing.T) {
	// Header claims 10 bytes, but only 4 follow — a truncated frame, not EOF.
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 10)
	r := bytes.NewReader(append(header[:], []byte("abcd")...))
	err := readMessage(r, &Request{})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF for truncated body, got %v", err)
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}
