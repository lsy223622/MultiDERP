package admin

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const MaxFrameSize = 64 << 10

type Request struct {
	Action           string `json:"action"`
	Name             string `json:"name,omitempty"`
	AuthType         string `json:"auth_type,omitempty"`
	ClientSecretFile string `json:"client_secret_file,omitempty"`
	AuthKeyFile      string `json:"auth_key_file,omitempty"`
	Required         *bool  `json:"required,omitempty"`
	Confirm          bool   `json:"confirm,omitempty"`
	Verbose          bool   `json:"verbose,omitempty"`
}

type Response struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func Success(message string, data any) Response {
	response := Response{OK: true, Message: message}
	if data != nil {
		if encoded, err := json.Marshal(data); err == nil {
			response.Data = encoded
		} else {
			return Failure(fmt.Sprintf("encode response data: %v", err))
		}
	}
	return response
}

func Failure(message string) Response {
	return Response{Error: message}
}

func WriteFrame(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	if len(payload) == 0 || len(payload) > MaxFrameSize {
		return fmt.Errorf("frame size %d is outside the allowed range", len(payload))
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	if _, err := w.Write(length[:]); err != nil {
		return fmt.Errorf("write frame length: %w", err)
	}
	written := 0
	for written < len(payload) {
		n, err := w.Write(payload[written:])
		if err != nil {
			return fmt.Errorf("write frame: %w", err)
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		written += n
	}
	return nil
}

func ReadFrame(r io.Reader, value any) error {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return fmt.Errorf("read frame length: %w", err)
	}
	size := binary.BigEndian.Uint32(length[:])
	if size == 0 || size > MaxFrameSize {
		return fmt.Errorf("frame size %d is outside the allowed range", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("read frame: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode frame: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("frame contains more than one JSON value")
		}
		return fmt.Errorf("decode trailing frame data: %w", err)
	}
	return nil
}
