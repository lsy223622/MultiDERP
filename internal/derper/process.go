package derper

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"multiderp/internal/config"
)

type Process struct {
	Binary string
	Output io.Writer

	mu       sync.Mutex
	command  *exec.Cmd
	done     chan struct{}
	exitErrs map[<-chan struct{}]error
}

func NewProcess(binary string, output io.Writer) *Process {
	if binary == "" {
		binary = "derper"
	}
	if output == nil {
		output = io.Discard
	}
	return &Process{Binary: binary, Output: output}
}

func (p *Process) Start(ctx context.Context, server config.ServerConfig, admissionAddress, keyPath string) error {
	p.mu.Lock()
	if p.command != nil {
		p.mu.Unlock()
		return errors.New("derper is already running")
	}
	if err := EnsureKey(keyPath); err != nil {
		p.mu.Unlock()
		return err
	}
	args, err := BuildArgs(server, admissionAddress, keyPath)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return err
	}
	command := exec.Command(p.Binary, args...)
	command.Stdout = p.Output
	command.Stderr = p.Output
	if err := command.Start(); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("start derper: %w", err)
	}
	done := make(chan struct{})
	p.command = command
	p.done = done
	if p.exitErrs == nil {
		p.exitErrs = make(map[<-chan struct{}]error)
	}
	p.mu.Unlock()
	go func() {
		err := command.Wait()
		p.mu.Lock()
		p.exitErrs[done] = err
		if p.command == command {
			p.command = nil
		}
		p.mu.Unlock()
		close(done)
	}()
	return nil
}

func (p *Process) WaitReady(ctx context.Context, server config.ServerConfig) error {
	p.mu.Lock()
	if p.command == nil {
		p.mu.Unlock()
		return errors.New("derper is not running")
	}
	p.mu.Unlock()
	host, _, err := net.SplitHostPort(server.DERP.Listen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		if ip != nil && ip.To4() == nil {
			host = "::1"
		} else {
			host = "127.0.0.1"
		}
	}
	scheme := "http"
	transport := http.DefaultTransport
	if server.DERP.TLSMode == "passthrough" {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // child certificate is deployment-owned.
	}
	client := &http.Client{Transport: transport, Timeout: 500 * time.Millisecond}
	url := scheme + "://" + net.JoinHostPort(host, portFromAddress(server.DERP.Listen)) + "/derp/probe"
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		response, requestErr := client.Get(url)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return fmt.Errorf("derper did not become ready at %s", url)
		case <-p.processDone():
			return errors.New("derper exited before becoming ready")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func portFromAddress(address string) string {
	_, port, _ := net.SplitHostPort(address)
	return port
}

func (p *Process) processDone() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done == nil {
		ch := make(chan struct{})
		return ch
	}
	return p.done
}

func (p *Process) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.command != nil
}

func (p *Process) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

func (p *Process) ExitError(done <-chan struct{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErrs[done]
}

func (p *Process) Stop(ctx context.Context) error {
	p.mu.Lock()
	command := p.command
	done := p.done
	p.mu.Unlock()
	if command == nil || done == nil {
		return nil
	}
	return stopCommand(ctx, command, done)
}

func stopCommand(ctx context.Context, command *exec.Cmd, done <-chan struct{}) error {
	if command == nil || done == nil {
		return nil
	}
	if command.Process != nil {
		_ = command.Process.Signal(os.Interrupt)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
			return nil
		case <-timer.C:
			return ctx.Err()
		}
	}
}
