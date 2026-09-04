package admin

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	want := Request{Action: "tailnet.add", Name: "alice", AuthType: "web", Confirm: true}
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, want); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}
	var got Request
	if err := ReadFrame(&buffer, &got); err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	if got != want {
		t.Fatalf("ReadFrame() = %#v, want %#v", got, want)
	}
}

func TestFrameRejectsOversizeAndTrailingJSON(t *testing.T) {
	if err := WriteFrame(io.Discard, strings.Repeat("x", MaxFrameSize)); err == nil {
		t.Fatal("WriteFrame() accepted an oversized frame")
	}

	payload, err := json.Marshal(Request{Action: "tailnet.list"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	payload = append(payload, []byte(` {"action":"tailnet.status"}`)...)
	var frame bytes.Buffer
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	frame.Write(length[:])
	frame.Write(payload)
	var request Request
	if err := ReadFrame(&frame, &request); err == nil || !strings.Contains(err.Error(), "more than one JSON value") {
		t.Fatalf("ReadFrame() error = %v, want trailing-value error", err)
	}
}

func TestFrameRejectsInvalidLengths(t *testing.T) {
	for _, size := range []uint32{0, MaxFrameSize + 1} {
		var frame bytes.Buffer
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], size)
		frame.Write(length[:])
		var value any
		if err := ReadFrame(&frame, &value); err == nil || !strings.Contains(err.Error(), "outside the allowed range") {
			t.Fatalf("ReadFrame(size=%d) error = %v, want length error", size, err)
		}
	}
}

func TestFrameReadShortInput(t *testing.T) {
	var value any
	err := ReadFrame(strings.NewReader("\x00\x00"), &value)
	if err == nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame() error = %v, want wrapped unexpected EOF", err)
	}
}
