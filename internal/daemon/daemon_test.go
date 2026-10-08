package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/config"
)

func TestEmptyConfigStartsWithoutDerperAndReportsHealth(t *testing.T) {
	dir := shortTempDir(t)
	configPath := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	cfg.Server.Admin.Socket = filepath.Join(dir, "run", "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Controller.Listen = freeLoopbackAddress(t)
	cfg.Controller.Database = filepath.Join(dir, "controller.sqlite")
	cfg.Controller.KeyFile = filepath.Join(dir, "controller.key")
	if err := config.WriteAtomic(configPath, cfg); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}

	d := New(context.Background(), Options{
		ConfigPath:   configPath,
		DerperOutput: io.Discard,
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = d.Shutdown() }()
	if d.derper.Running() {
		t.Fatal("empty config started derper")
	}

	httpClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	for path, wantStatus := range map[string]int{
		"/health/live":    http.StatusOK,
		"/health/startup": http.StatusOK,
		"/health/ready":   http.StatusServiceUnavailable,
	} {
		response, err := httpClient.Get("http://" + d.healthListener.Addr().String() + path)
		if err != nil {
			t.Fatalf("GET %s error = %v", path, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != wantStatus {
			t.Errorf("GET %s status = %d, want %d", path, response.StatusCode, wantStatus)
		}
	}

}

func TestMissingConfigIsCreatedBeforeListeners(t *testing.T) {
	dir := shortTempDir(t)
	cfg := config.Default()
	cfg.Server.Admin.Socket = filepath.Join(dir, "run", "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Controller.Listen = freeLoopbackAddress(t)
	cfg.Controller.Database = filepath.Join(dir, "controller.sqlite")
	cfg.Controller.KeyFile = filepath.Join(dir, "controller.key")
	templatePath := filepath.Join(dir, "template.yaml")
	if err := config.WriteAtomic(templatePath, cfg); err != nil {
		t.Fatalf("WriteAtomic() template error = %v", err)
	}
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	configPath := filepath.Join(dir, "missing.yaml")
	d := New(context.Background(), Options{
		ConfigPath:     configPath,
		ConfigTemplate: template,
		DerperOutput:   io.Discard,
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = d.Shutdown() }()
	if d.derper.Running() {
		t.Fatal("missing config started derper")
	}
	created, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read created config: %v", err)
	}
	if !bytes.Equal(created, template) {
		t.Fatalf("created config differs from template:\n%s", created)
	}
}

func TestRestartOnlyReloadIsPendingWithoutChangingRuntimeBoundary(t *testing.T) {
	dir := shortTempDir(t)
	configPath := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	cfg.Server.Admin.Socket = filepath.Join(dir, "run", "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Controller.Listen = freeLoopbackAddress(t)
	cfg.Controller.Database = filepath.Join(dir, "controller.sqlite")
	cfg.Controller.KeyFile = filepath.Join(dir, "controller.key")
	cfg.Storage.StateDir = filepath.Join(dir, "data")
	if err := config.WriteAtomic(configPath, cfg); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}

	d := New(context.Background(), Options{
		ConfigPath:   configPath,
		DerperOutput: io.Discard,
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = d.Shutdown() }()

	updated := cfg.Clone()
	updated.Server.DERP.Listen = ":3379"
	if err := config.WriteAtomic(configPath, updated); err != nil {
		t.Fatalf("write updated config: %v", err)
	}
	response := d.handleRequest(context.Background(), admin.Request{Action: "config.reload"})
	if !response.OK || !strings.Contains(response.Message, "restart required") {
		t.Fatalf("restart-only reload response = %#v", response)
	}
	if got := d.activeConfig().Server.DERP.Listen; got != cfg.Server.DERP.Listen {
		t.Fatalf("active DERP listener = %q, want %q", got, cfg.Server.DERP.Listen)
	}
	if got := d.currentConfig().Server.DERP.Listen; got != updated.Server.DERP.Listen {
		t.Fatalf("desired DERP listener = %q, want %q", got, updated.Server.DERP.Listen)
	}
	if !d.healthSnapshot().PendingRestart {
		t.Fatal("health snapshot did not report pending restart")
	}
}

func TestDaemonRestartMarksChildUnavailableBeforeStopping(t *testing.T) {
	d := New(context.Background(), Options{DerperOutput: io.Discard})
	fake := &daemonFakeProcess{stopEntered: make(chan struct{}), stopRelease: make(chan struct{})}
	d.derper = fake
	cfg := config.Default()
	cfg.Server.Hostname = "derp.example.com"
	root := t.TempDir()
	cfg.Storage.StateDir = filepath.Join(root, "data")
	d.mu.Lock()
	d.current = cfg.Clone()
	d.desired = cfg.Clone()
	d.started = true
	d.mu.Unlock()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.controllerListener = listener
	t.Cleanup(func() { listener.Close(); d.Shutdown() })
	if err := d.syncDerper(context.Background()); err != nil {
		t.Fatalf("initial syncDerper() error = %v", err)
	}
	if !d.derper.Running() || !d.childOK {
		t.Fatal("initial fake derper is not usable")
	}
	stopEntered, stopRelease := fake.stopEntered, fake.stopRelease
	restartDone := make(chan error, 1)
	go func() { restartDone <- d.restartDerper(context.Background()) }()
	select {
	case <-stopEntered:
	case <-time.After(time.Second):
		t.Fatal("restart did not attempt to stop derper")
	}
	if d.healthSnapshot().DerperUsable {
		t.Fatal("derper remained usable while intentional restart was stopping it")
	}
	close(stopRelease)
	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatalf("restartDerper() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restartDerper() did not finish")
	}
	if !d.derper.Running() || !d.childOK {
		t.Fatal("derper was not usable after successful restart")
	}
	fake.mu.Lock()
	starts := fake.starts
	fake.mu.Unlock()
	if starts != 2 {
		t.Fatalf("fake derper starts = %d, want 2", starts)
	}
}

type daemonFakeProcess struct {
	mu          sync.Mutex
	running     bool
	done        chan struct{}
	exitErrs    map[<-chan struct{}]error
	starts      int
	stopEntered chan struct{}
	stopRelease chan struct{}
}

func (p *daemonFakeProcess) Start(context.Context, config.ServerConfig, string, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return errors.New("fake derper already running")
	}
	p.done = make(chan struct{})
	if p.exitErrs == nil {
		p.exitErrs = make(map[<-chan struct{}]error)
	}
	p.exitErrs[p.done] = nil
	p.running = true
	p.starts++
	return nil
}

func (p *daemonFakeProcess) WaitReady(context.Context, config.ServerConfig) error { return nil }

func (p *daemonFakeProcess) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

func (p *daemonFakeProcess) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

func (p *daemonFakeProcess) ExitError(done <-chan struct{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErrs[done]
}

func (p *daemonFakeProcess) Stop(context.Context) error {
	p.mu.Lock()
	done := p.done
	running := p.running
	entered := p.stopEntered
	release := p.stopRelease
	p.stopEntered, p.stopRelease = nil, nil
	p.running = false
	p.mu.Unlock()
	if !running || done == nil {
		return nil
	}
	if entered != nil {
		close(entered)
	}
	if release != nil {
		<-release
	}
	close(done)
	return nil
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}
	return address
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "md-")
	if err != nil {
		t.Fatalf("create short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
