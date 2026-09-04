package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsy223622/MultiDERP/internal/admin"
	"github.com/lsy223622/MultiDERP/internal/config"
	"github.com/lsy223622/MultiDERP/internal/verifier"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

func TestEmptyConfigStartsWithoutDerperAndReportsHealth(t *testing.T) {
	dir := shortTempDir(t)
	configPath := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	cfg.Server.Admin.Socket = filepath.Join(dir, "run", "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	if err := config.WriteAtomic(configPath, cfg); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}

	d := New(context.Background(), Options{
		ConfigPath:       configPath,
		AdmissionAddress: freeLoopbackAddress(t),
		DerperOutput:     io.Discard,
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

	response, err := (admin.Client{SocketPath: cfg.Server.Admin.Socket, Timeout: time.Second}).Call(context.Background(), admin.Request{Action: "tailnet.list"})
	if err != nil {
		t.Fatalf("admin tailnet.list error = %v", err)
	}
	var statuses []struct{}
	if err := json.Unmarshal(response.Data, &statuses); err != nil || len(statuses) != 0 {
		t.Fatalf("empty tailnet list = %#v, error = %v", statuses, err)
	}

	requestBody, err := json.Marshal(tailcfg.DERPAdmitClientRequest{NodePublic: key.NewNode().Public()})
	if err != nil {
		t.Fatalf("marshal admission request: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+d.admissionAddress+"/admit", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("create admission request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	admissionResponse, err := httpClient.Do(request)
	if err != nil {
		t.Fatalf("admission request error = %v", err)
	}
	_, _ = io.Copy(io.Discard, admissionResponse.Body)
	_ = admissionResponse.Body.Close()
	if admissionResponse.StatusCode != http.StatusOK {
		t.Fatalf("empty-pool admission status = %d, want 200", admissionResponse.StatusCode)
	}
}

func TestMissingConfigFailsBeforeListeners(t *testing.T) {
	dir := shortTempDir(t)
	adminPath := filepath.Join(dir, "run", "admin.sock")
	d := New(context.Background(), Options{
		ConfigPath:       filepath.Join(dir, "missing.yaml"),
		AdmissionAddress: freeLoopbackAddress(t),
	})
	if err := d.Start(context.Background()); err == nil {
		t.Fatal("Start() succeeded without the required config file")
	}
	if _, err := os.Stat(adminPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admin socket after missing-config failure: %v", err)
	}
	if err := d.Shutdown(); err != nil {
		t.Fatalf("Shutdown() after failed Start() error = %v", err)
	}
}

func TestRestartOnlyReloadIsPendingWithoutChangingRuntimeBoundary(t *testing.T) {
	dir := shortTempDir(t)
	configPath := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	cfg.Server.Admin.Socket = filepath.Join(dir, "run", "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Storage.StateDir = filepath.Join(dir, "data")
	cfg.Storage.TailnetStateDir = filepath.Join(dir, "tailnets")
	cfg.Storage.OrphanStateDir = filepath.Join(dir, "orphans")
	if err := config.WriteAtomic(configPath, cfg); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}

	d := New(context.Background(), Options{
		ConfigPath:       configPath,
		AdmissionAddress: freeLoopbackAddress(t),
		DerperOutput:     io.Discard,
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

func TestAdmissionAndConfigReloadConcurrent(t *testing.T) {
	dir := shortTempDir(t)
	configPath := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	cfg.Server.Hostname = "derp.example.com"
	cfg.Server.Admin.Socket = filepath.Join(dir, "run", "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Storage.StateDir = filepath.Join(dir, "data")
	cfg.Storage.TailnetStateDir = filepath.Join(dir, "tailnets")
	cfg.Storage.OrphanStateDir = filepath.Join(dir, "orphans")
	cfg.Tailnets = []config.TailnetConfig{{Name: "alice", Disabled: true, Auth: config.AuthConfig{Type: "web"}}}
	cfg.Normalize()
	if err := config.WriteAtomic(configPath, cfg); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}

	d := New(context.Background(), Options{
		ConfigPath:       configPath,
		AdmissionAddress: freeLoopbackAddress(t),
		DerperOutput:     io.Discard,
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() { _ = d.Shutdown() }()

	v := &daemonBlockingVerifier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	d.manager.Pool().Upsert("alice", v)
	d.admission.SetBarrier(true)
	firstAdmission := make(chan daemonAdmissionResult, 1)
	go func() {
		allow, err := d.admission.Admit(context.Background(), key.NewNode().Public(), keyAddrForDaemonTest())
		firstAdmission <- daemonAdmissionResult{allow: allow, err: err}
	}()
	select {
	case <-v.entered:
	case <-time.After(time.Second):
		t.Fatal("admission did not reach verifier before reload")
	}

	updated := cfg.Clone()
	updated.Logging.Level = "debug"
	if err := config.WriteAtomic(configPath, updated); err != nil {
		t.Fatalf("write reload config: %v", err)
	}
	reloadDone := make(chan admin.Response, 1)
	go func() {
		reloadDone <- d.handleRequest(context.Background(), admin.Request{Action: "config.reload"})
	}()
	select {
	case response := <-reloadDone:
		t.Fatalf("config reload completed before in-flight admission released: %#v", response)
	case <-time.After(20 * time.Millisecond):
	}
	close(v.release)
	if result := <-firstAdmission; result.allow {
		t.Fatalf("released non-matching admission was allowed: %#v", result)
	}
	if response := <-reloadDone; !response.OK {
		t.Fatalf("config reload response = %#v", response)
	}

	start := make(chan struct{})
	errs := make(chan error, 128)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 48; i++ {
			allow, err := d.admission.Admit(context.Background(), key.NewNode().Public(), keyAddrForDaemonTest())
			if err != nil {
				errs <- err
			}
			if allow {
				errs <- errors.New("non-matching reloaded admission was allowed")
			}
			_ = d.healthSnapshot()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 24; i++ {
			desired := cfg.Clone()
			if i%2 == 0 {
				desired.Logging.Level = "info"
			} else {
				desired.Logging.Level = "debug"
			}
			if err := config.WriteAtomic(configPath, desired); err != nil {
				errs <- err
				continue
			}
			if response := d.handleRequest(context.Background(), admin.Request{Action: "config.reload"}); !response.OK {
				errs <- errors.New(response.Message)
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent admission/reload operation: %v", err)
	}
}

func TestDaemonShutdownCancelsInFlightAdmission(t *testing.T) {
	dir := shortTempDir(t)
	configPath := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	cfg.Server.Hostname = "derp.example.com"
	cfg.Server.Admin.Socket = filepath.Join(dir, "run", "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Storage.StateDir = filepath.Join(dir, "data")
	cfg.Storage.TailnetStateDir = filepath.Join(dir, "tailnets")
	cfg.Storage.OrphanStateDir = filepath.Join(dir, "orphans")
	if err := config.WriteAtomic(configPath, cfg); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}

	d := New(context.Background(), Options{
		ConfigPath:       configPath,
		AdmissionAddress: freeLoopbackAddress(t),
		DerperOutput:     io.Discard,
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	v := &daemonBlockingVerifier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	d.manager.Pool().Upsert("alice", v)
	d.admission.SetBarrier(true)
	resultCh := make(chan daemonAdmissionResult, 1)
	go func() {
		allow, err := d.admission.Admit(context.Background(), key.NewNode().Public(), keyAddrForDaemonTest())
		resultCh <- daemonAdmissionResult{allow: allow, err: err}
	}()
	select {
	case <-v.entered:
	case <-time.After(time.Second):
		_ = d.Shutdown()
		t.Fatal("admission did not reach verifier before shutdown")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- d.Shutdown() }()
	select {
	case result := <-resultCh:
		if result.allow {
			t.Fatalf("shutdown allowed in-flight admission: %#v", result)
		}
	case <-time.After(2 * time.Second):
		_ = d.Shutdown()
		t.Fatal("shutdown did not cancel in-flight admission")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown() did not finish after canceling admission")
	}
}

func TestDaemonShutdownCancelsAdmissionBeforeWaitingForAdminMutation(t *testing.T) {
	dir := shortTempDir(t)
	configPath := filepath.Join(dir, "config.yaml")
	cfg := config.Default()
	cfg.Server.Hostname = "derp.example.com"
	cfg.Server.Admin.Socket = filepath.Join(dir, "run", "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Storage.StateDir = filepath.Join(dir, "data")
	cfg.Storage.TailnetStateDir = filepath.Join(dir, "tailnets")
	cfg.Storage.OrphanStateDir = filepath.Join(dir, "orphans")
	cfg.Tailnets = []config.TailnetConfig{{Name: "alice", Auth: config.AuthConfig{Type: "web"}}}
	if err := config.WriteAtomic(configPath, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	blocking := &daemonBlockingVerifier{entered: make(chan struct{}), release: make(chan struct{})}
	d := New(context.Background(), Options{
		ConfigPath:       configPath,
		AdmissionAddress: freeLoopbackAddress(t),
		DerperOutput:     io.Discard,
		Factory:          &daemonBlockingFactory{verifier: blocking},
	})
	d.derper = &daemonFakeProcess{}
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown() })

	resultCh := make(chan daemonAdmissionResult, 1)
	go func() {
		allow, err := d.admission.Admit(context.Background(), key.NewNode().Public(), keyAddrForDaemonTest())
		resultCh <- daemonAdmissionResult{allow: allow, err: err}
	}()
	select {
	case <-blocking.entered:
	case <-time.After(time.Second):
		_ = d.Shutdown()
		t.Fatal("admission did not reach verifier before admin mutation")
	}

	adminResult := make(chan error, 1)
	go func() {
		_, err := (admin.Client{SocketPath: cfg.Server.Admin.Socket, Timeout: 5 * time.Second}).Call(context.Background(), admin.Request{
			Action: "tailnet.disable",
			Name:   "alice",
		})
		adminResult <- err
	}()
	removeDeadline := time.NewTimer(time.Second)
	defer removeDeadline.Stop()
	for d.manager.Pool().Contains("alice") {
		select {
		case <-removeDeadline.C:
			_ = d.Shutdown()
			t.Fatal("admin mutation did not remove verifier from admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- d.Shutdown() }()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown() waited for an admin mutation before canceling admission")
	}
	select {
	case result := <-resultCh:
		if result.allow {
			t.Fatalf("shutdown allowed in-flight admission: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight admission did not finish during shutdown")
	}
	select {
	case <-adminResult:
	case <-time.After(time.Second):
		t.Fatal("admin mutation did not finish after shutdown")
	}
}

func TestDaemonRestartMarksChildUnavailableBeforeStopping(t *testing.T) {
	d := New(context.Background(), Options{DerperOutput: io.Discard})
	t.Cleanup(func() {
		d.admission.Close()
		_ = d.manager.Close()
	})
	fake := &daemonFakeProcess{stopEntered: make(chan struct{}), stopRelease: make(chan struct{})}
	d.derper = fake
	cfg := config.Default()
	cfg.Server.Hostname = "derp.example.com"
	root := t.TempDir()
	cfg.Storage.StateDir = filepath.Join(root, "data")
	cfg.Storage.TailnetStateDir = filepath.Join(root, "tailnets")
	cfg.Storage.OrphanStateDir = filepath.Join(root, "orphans")
	d.mu.Lock()
	d.current = cfg.Clone()
	d.desired = cfg.Clone()
	d.started = true
	d.mu.Unlock()
	d.manager.Pool().Upsert("alice", &daemonReadyVerifier{})
	if err := d.syncDerper(context.Background()); err != nil {
		t.Fatalf("initial syncDerper() error = %v", err)
	}
	if !d.healthSnapshot().DerperUsable {
		t.Fatal("initial fake derper is not usable")
	}
	restartDone := make(chan error, 1)
	go func() { restartDone <- d.restartDerper(context.Background()) }()
	select {
	case <-fake.stopEntered:
	case <-time.After(time.Second):
		t.Fatal("restart did not attempt to stop derper")
	}
	if d.healthSnapshot().DerperUsable {
		t.Fatal("derper remained usable while intentional restart was stopping it")
	}
	close(fake.stopRelease)
	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatalf("restartDerper() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restartDerper() did not finish")
	}
	if !d.healthSnapshot().DerperUsable {
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

type daemonBlockingFactory struct {
	verifier *daemonBlockingVerifier
}

func (f *daemonBlockingFactory) New(context.Context, config.TailnetConfig, string, func(string, ...any)) (verifier.Verifier, error) {
	return f.verifier, nil
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

type daemonReadyVerifier struct{}

func (*daemonReadyVerifier) Name() string { return "alice" }

func (*daemonReadyVerifier) State() verifier.State { return verifier.StateConnected }

func (*daemonReadyVerifier) ContainsNode(context.Context, key.NodePublic) (bool, error) {
	return false, nil
}

func (*daemonReadyVerifier) Status(context.Context) verifier.Status {
	return verifier.Status{Name: "alice", State: verifier.StateConnected, HardeningVerified: true}
}

func (*daemonReadyVerifier) Close() error { return nil }

type daemonBlockingVerifier struct {
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func (v *daemonBlockingVerifier) Name() string { return "alice" }

func (v *daemonBlockingVerifier) State() verifier.State { return verifier.StateConnected }

func (v *daemonBlockingVerifier) ContainsNode(ctx context.Context, _ key.NodePublic) (bool, error) {
	v.enteredOnce.Do(func() { close(v.entered) })
	select {
	case <-v.release:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (v *daemonBlockingVerifier) Status(context.Context) verifier.Status {
	return verifier.Status{Name: v.Name(), State: verifier.StateConnected, HardeningVerified: true}
}

func (v *daemonBlockingVerifier) Close() error { return nil }

type daemonAdmissionResult struct {
	allow bool
	err   error
}

func keyAddrForDaemonTest() netip.Addr {
	return netip.MustParseAddr("192.0.2.2")
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
