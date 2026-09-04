package admin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	connectionTimeout = 10 * time.Second
	requestTimeout    = 30 * time.Second
)

type Handler func(context.Context, Request) Response

type Server struct {
	path      string
	handler   Handler
	listener  net.Listener
	stop      chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	stopOnce  sync.Once
	mu        sync.Mutex
	cancel    context.CancelFunc
	workers   sync.WaitGroup
	workersMu sync.Mutex
	accepting bool
	serving   bool
}

func NewServer(path string, handler Handler) *Server {
	return &Server{
		path:    path,
		handler: handler,
		stop:    make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (s *Server) Start() error {
	if s.handler == nil {
		return errors.New("admin handler is required")
	}
	if s.path == "" {
		return errors.New("admin socket path is required")
	}
	socketDir := filepath.Dir(s.path)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return fmt.Errorf("create admin socket directory: %w", err)
	}
	if socketDir != "." && socketDir != "" {
		if err := os.Chmod(socketDir, 0o700); err != nil {
			return fmt.Errorf("set admin socket directory permissions: %w", err)
		}
	}
	if err := removeStaleSocket(s.path); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("listen on admin socket %q: %w", s.path, err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.path)
		return fmt.Errorf("set admin socket permissions: %w", err)
	}
	s.listener = listener
	s.workersMu.Lock()
	s.accepting = true
	s.workersMu.Unlock()
	return nil
}

func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("admin server is not started")
	}
	serveCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	if isClosed(s.stop) {
		cancel()
	}
	s.mu.Unlock()
	s.workersMu.Lock()
	s.serving = s.accepting && !isClosed(s.stop)
	s.workersMu.Unlock()
	defer cancel()
	defer func() {
		s.workersMu.Lock()
		s.accepting = false
		s.serving = false
		s.workersMu.Unlock()
		close(s.closed)
	}()
	for {
		if deadlineListener, ok := s.listener.(interface{ SetDeadline(time.Time) error }); ok {
			_ = deadlineListener.SetDeadline(time.Now().Add(time.Second))
		}
		conn, err := s.listener.Accept()
		if err != nil {
			if serveCtx.Err() != nil || isClosed(s.stop) {
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return fmt.Errorf("accept admin connection: %w", err)
		}
		s.workersMu.Lock()
		accepting := s.accepting && !isClosed(s.stop)
		if accepting {
			s.workers.Add(1)
		}
		s.workersMu.Unlock()
		if !accepting {
			_ = conn.Close()
			return nil
		}
		go func() {
			defer s.workers.Done()
			s.handleConnection(serveCtx, conn)
		}()
	}
}

func (s *Server) handleConnection(parent context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(connectionTimeout))
	var request Request
	if err := ReadFrame(conn, &request); err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(connectionTimeout))
		_ = WriteFrame(conn, Failure("invalid admin request"))
		return
	}
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()
	_ = conn.SetDeadline(time.Now().Add(requestTimeout + connectionTimeout))
	response := s.handler(ctx, request)
	_ = conn.SetWriteDeadline(time.Now().Add(connectionTimeout))
	_ = WriteFrame(conn, response)
}

func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.StopAccepting()
		s.workers.Wait()
		if s.path != "" {
			removeErr := os.Remove(s.path)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
				err = removeErr
			}
		}
	})
	return err
}

func (s *Server) StopAccepting() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stop)
		s.workersMu.Lock()
		s.accepting = false
		s.serving = false
		s.workersMu.Unlock()
		s.mu.Lock()
		cancel := s.cancel
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if s.listener != nil {
			err = s.listener.Close()
		}
	})
	return err
}

func (s *Server) Wait() {
	s.workers.Wait()
}

func (s *Server) Running() bool {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	return s.serving && s.accepting && !isClosed(s.stop)
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing admin socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("admin socket path %q already exists and is not a socket", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale admin socket: %w", err)
	}
	return nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

type Client struct {
	SocketPath string
	Timeout    time.Duration
}

func (c Client) Call(ctx context.Context, request Request) (Response, error) {
	if c.SocketPath == "" {
		return Response{}, errors.New("admin socket path is required")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = requestTimeout + connectionTimeout
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return Response{}, fmt.Errorf("connect to admin socket: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := WriteFrame(conn, request); err != nil {
		return Response{}, err
	}
	var response Response
	if err := ReadFrame(conn, &response); err != nil {
		return Response{}, err
	}
	if !response.OK {
		return response, errors.New(response.Error)
	}
	return response, nil
}
