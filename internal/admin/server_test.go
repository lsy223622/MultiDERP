package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type recordingConn struct {
	net.Conn
	mu             sync.Mutex
	deadlines      []time.Time
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (c *recordingConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()
	return c.Conn.SetDeadline(deadline)
}

func (c *recordingConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadlines = append(c.readDeadlines, deadline)
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(deadline)
}

func (c *recordingConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadlines = append(c.writeDeadlines, deadline)
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(deadline)
}

func TestServerClientRoundTrip(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "admin.sock")
	server := NewServer(socket, func(_ context.Context, request Request) Response {
		if request.Action != "tailnet.list" {
			return Failure("unexpected action")
		}
		return Success("ok", map[string]string{"name": "alice"})
	})
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()

	response, err := (Client{SocketPath: socket, Timeout: time.Second}).Call(context.Background(), Request{Action: "tailnet.list"})
	if err != nil {
		t.Fatalf("Client.Call() error = %v", err)
	}
	if !response.OK || response.Message != "ok" {
		t.Fatalf("Client.Call() response = %#v", response)
	}
	var data map[string]string
	if err := json.Unmarshal(response.Data, &data); err != nil || data["name"] != "alice" {
		t.Fatalf("response data = %#v, error = %v", data, err)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not stop after Close()")
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket stat error = %v, want not-exist", err)
	}
}

func TestServerDoesNotRemoveNonSocketPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	server := NewServer(path, func(context.Context, Request) Response { return Success("", nil) })
	if err := server.Start(); err == nil {
		t.Fatal("Start() accepted a non-socket path")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatalf("sentinel after failed Start() = %q, error = %v", data, err)
	}
}

func TestServerRejectsInvalidRequestWithFailureFrame(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "admin.sock")
	server := NewServer(socket, func(context.Context, Request) Response { return Failure("invalid action") })
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()

	client := Client{SocketPath: socket, Timeout: time.Second}
	response, err := client.Call(context.Background(), Request{Action: ""})
	if err == nil || response.OK {
		t.Fatalf("invalid action response = %#v, error = %v; want failure", response, err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve() did not stop")
	}
}

func TestHandleConnectionGivesHandlerTheRequestTimeout(t *testing.T) {
	server := NewServer("unused", nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	server.handler = func(context.Context, Request) Response {
		close(entered)
		<-release
		return Success("ok", nil)
	}
	serverSide, clientSide := net.Pipe()
	wrapped := &recordingConn{Conn: serverSide}
	done := make(chan struct{})
	go func() {
		server.handleConnection(context.Background(), wrapped)
		close(done)
	}()
	if err := WriteFrame(clientSide, Request{Action: "tailnet.list"}); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	wrapped.mu.Lock()
	processingDeadlines := append([]time.Time(nil), wrapped.deadlines...)
	readDeadlines := append([]time.Time(nil), wrapped.readDeadlines...)
	wrapped.mu.Unlock()
	if len(readDeadlines) != 1 {
		t.Fatalf("read deadlines = %d, want 1", len(readDeadlines))
	}
	if len(processingDeadlines) != 1 || time.Until(processingDeadlines[0]) < requestTimeout {
		t.Fatalf("processing deadlines = %#v, want a full request timeout", processingDeadlines)
	}
	close(release)
	var response Response
	if err := ReadFrame(clientSide, &response); err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = clientSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleConnection did not finish")
	}
}
