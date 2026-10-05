package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/control"
	"github.com/lsy223622/UniDERP/v2/internal/derper"
	"github.com/lsy223622/UniDERP/v2/internal/health"
	"github.com/lsy223622/UniDERP/v2/internal/logging"
)

type Options struct {
	ConfigPath     string
	ConfigTemplate []byte
	DerperBinary   string
	DerperOutput   io.Writer
	Logger         *log.Logger
}

type derperProcess interface {
	Start(context.Context, config.ServerConfig, string, string) error
	WaitReady(context.Context, config.ServerConfig) error
	Running() bool
	Done() <-chan struct{}
	ExitError(<-chan struct{}) error
	Stop(context.Context) error
}

type Daemon struct {
	mu                 sync.RWMutex
	opMu               sync.Mutex
	derperMu           sync.Mutex
	started            bool
	starting           bool
	startup            bool
	stopping           bool
	childOK            bool
	childGeneration    uint64
	expectedChildStops map[uint64]struct{}

	configPath     string
	configTemplate []byte
	derperBinary   string
	derperOutput   io.Writer
	logFilter      *logging.Filter
	logf           func(string, ...any)

	current        config.Config
	desired        config.Config
	pendingRestart bool
	applyError     string
	runCtx         context.Context
	derper         derperProcess

	adminServer          *admin.Server
	adminListenerStarted bool
	healthServer         *http.Server
	healthListener       net.Listener
	controllerStore      *control.Store
	controllerServer     *http.Server
	controllerListener   net.Listener
	managementServer     *http.Server
	managementListener   net.Listener
	controllerCancel     context.CancelFunc
	controllerDone       chan struct{}
	nodeClient           *cluster.EnrollmentClient
	policyClient         derper.PolicyClient
	nodeCancel           context.CancelFunc
	nodeDone             chan struct{}
	nodeConflict         bool

	fatal     chan error
	fatalOnce sync.Once
	closeOnce sync.Once
}

func New(parent context.Context, options Options) *Daemon {
	if options.ConfigPath == "" {
		options.ConfigPath = config.DefaultConfigPath
	}
	if options.DerperBinary == "" {
		options.DerperBinary = "derper"
	}
	if options.DerperOutput == nil {
		options.DerperOutput = io.Discard
	}
	if options.Logger == nil {
		options.Logger = log.New(os.Stderr, "uniderp: ", log.LstdFlags|log.Lmicroseconds)
	}
	if len(options.ConfigTemplate) == 0 {
		options.ConfigTemplate = config.BootstrapYAML()
	}
	logFilter := logging.New(options.Logger, config.DefaultLoggingLevel)
	d := &Daemon{
		configPath:         options.ConfigPath,
		configTemplate:     append([]byte(nil), options.ConfigTemplate...),
		derperBinary:       options.DerperBinary,
		derperOutput:       options.DerperOutput,
		logFilter:          logFilter,
		logf:               logFilter.Printf,
		fatal:              make(chan error, 1),
		expectedChildStops: make(map[uint64]struct{}),
	}
	d.derper = derper.NewProcess(d.derperBinary, d.derperOutput)
	return d
}

func (d *Daemon) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started || d.starting || d.stopping {
		d.mu.Unlock()
		return errors.New("daemon is already started")
	}
	d.starting = true
	d.mu.Unlock()
	createdConfig, err := config.CreateFileIfMissing(d.configPath, d.configTemplate)
	if err != nil {
		return d.abortStart(err)
	}
	if createdConfig {
		d.logf("INFO created missing configuration file %s from the bundled example", d.configPath)
	}
	parsed, err := config.LoadFile(d.configPath)
	if err != nil {
		return d.abortStart(err)
	}
	d.logFilter.SetLevel(parsed.Config.Logging.Level)
	d.logf("INFO configuration loaded from %s", d.configPath)
	for _, warning := range parsed.Warnings {
		d.logf("WARN %s", warning)
	}
	d.mu.Lock()
	d.current = parsed.Config.Clone()
	d.desired = parsed.Config.Clone()
	d.pendingRestart = false
	d.mu.Unlock()
	d.runCtx = ctx
	if err := d.startManagement(parsed.Config); err != nil {
		return d.abortStart(err)
	}
	if err := d.prepareRole(ctx, parsed.Config); err != nil {
		d.setApplyError(err)
	}
	if err := d.startAdminServer(parsed.Config.Server.Admin.Socket); err != nil {
		return d.abortStart(err)
	}
	if err := d.startHealthServer(parsed.Config.Server.Health.Listen); err != nil {
		return d.abortStart(err)
	}
	if d.applyError == "" {
		if err := d.syncDerper(ctx); err != nil {
			d.setApplyError(err)
		}
	}
	d.mu.Lock()
	d.started = true
	d.starting = false
	d.startup = true
	d.mu.Unlock()
	if d.nodeClient != nil && d.derper.Running() {
		d.startNodeControl(ctx)
	}
	return nil
}

func (d *Daemon) abortStart(err error) error {
	d.mu.Lock()
	d.starting = false
	d.mu.Unlock()
	_ = d.Shutdown()
	return err
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := d.Start(ctx); err != nil {
		return err
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-d.fatal:
	}
	shutdownErr := d.Shutdown()
	return errors.Join(runErr, shutdownErr)
}

func (d *Daemon) syncDerper(ctx context.Context) error {
	return d.syncDerperConfig(ctx, d.activeConfig())
}

func (d *Daemon) syncDerperConfig(ctx context.Context, cfg config.Config) error {
	d.derperMu.Lock()
	defer d.derperMu.Unlock()
	d.mu.RLock()
	if d.stopping {
		d.mu.RUnlock()
		return errors.New("daemon is stopping")
	}
	nodeConflict := d.nodeConflict
	d.mu.RUnlock()
	if cfg.SetupRequired || cfg.Server.Hostname == "" || nodeConflict {
		return nil
	}
	if d.derper.Running() {
		return nil
	}
	d.mu.Lock()
	d.childOK = false
	d.childGeneration++
	childGeneration := d.childGeneration
	d.mu.Unlock()
	keyPath := filepath.Join(cfg.Storage.StateDir, "derper", "derper.key")
	d.logf("INFO starting derper child")
	if d.controllerListener == nil {
		return errors.New("node management listener is not running")
	}
	if err := d.derper.Start(ctx, cfg.Server, d.controllerListener.Addr().String(), keyPath); err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := d.derper.WaitReady(readyCtx, cfg.Server); err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = d.derper.Stop(cleanupCtx)
		cleanupCancel()
		d.mu.Lock()
		if d.childGeneration == childGeneration {
			d.childOK = false
		}
		d.mu.Unlock()
		return err
	}
	if !d.publishChildIfCurrent(childGeneration) {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = d.derper.Stop(cleanupCtx)
		cleanupCancel()
		d.mu.Lock()
		if d.childGeneration == childGeneration {
			d.childOK = false
		}
		d.mu.Unlock()
		return nil
	}
	d.monitorDerper(childGeneration)
	d.logf("INFO derper is ready")
	return nil
}

func (d *Daemon) monitorDerper(generation uint64) {
	done := d.derper.Done()
	if done == nil {
		return
	}
	go func() {
		<-done
		err := d.derper.ExitError(done)
		d.derperMu.Lock()
		defer d.derperMu.Unlock()
		d.mu.Lock()
		_, intentional := d.expectedChildStops[generation]
		delete(d.expectedChildStops, generation)
		if d.stopping {
			intentional = true
		}
		current := d.childGeneration == generation
		if current {
			d.childOK = false
		}
		d.mu.Unlock()
		if !intentional {
			if err == nil {
				err = errors.New("derper exited unexpectedly")
			}
			d.logf("ERROR derper child exited: %v", err)
			d.setApplyError(fmt.Errorf("derper child failed: %w", err))
		} else {
			d.logf("INFO derper child stopped")
		}
	}()
}

func (d *Daemon) reportFatal(err error) {
	if err == nil {
		return
	}
	d.fatalOnce.Do(func() { d.fatal <- err })
}

func (d *Daemon) startAdminServer(path string) error {
	d.adminServer = admin.NewServer(path, d.handleRequest)
	if err := d.adminServer.Start(); err != nil {
		return err
	}
	d.adminListenerStarted = true
	go func() {
		if err := d.adminServer.Serve(context.Background()); err != nil {
			d.reportFatal(err)
		}
	}()
	return nil
}

func (d *Daemon) startHealthServer(address string) error {
	d.healthServer = &http.Server{Handler: health.NewServer(d.healthSnapshot).Handler()}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen health endpoint on %q: %w", address, err)
	}
	d.healthListener = listener
	go func() {
		if err := d.healthServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.reportFatal(fmt.Errorf("health server: %w", err))
		}
	}()
	return nil
}

func (d *Daemon) healthSnapshot() health.Snapshot {
	d.mu.RLock()
	live := (d.started || d.starting) && !d.stopping
	startup := d.startup
	childOK := d.childOK
	started := d.started
	pendingRestart := d.pendingRestart
	d.mu.RUnlock()
	if live && started {
		live = d.adminServer != nil && d.adminServer.Running() && d.controllerListener != nil
	}
	var nodeStatus cluster.ControlStatus
	if d.nodeClient != nil {
		nodeStatus = d.nodeClient.ControlStatus()
	}
	usable := false
	if childOK && d.derper.Running() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		applied, err := d.policyClient.Status(ctx)
		cancel()
		usable = applied.Usable
		nodeStatus.Traffic = applied.Traffic
		nodeStatus.TrafficObservedAt = applied.TrafficObservedAt
		nodeStatus.ActiveConnections = applied.ActiveConnections
		if applied.Revision != 0 || err == nil {
			nodeStatus.AppliedRevision = applied.Revision
		}
	}
	nodeStatus.Usable = usable
	return health.Snapshot{
		Live:           live,
		Startup:        startup,
		DerperUsable:   usable,
		Node:           nodeStatus,
		PendingRestart: pendingRestart,
		Ready:          usable,
	}
}

func (d *Daemon) handleRequest(ctx context.Context, request admin.Request) admin.Response {
	d.mu.RLock()
	stopping := d.stopping
	d.mu.RUnlock()
	if stopping {
		return admin.Failure("daemon is stopping")
	}
	if err := ctx.Err(); err != nil {
		return admin.Failure("admin request canceled: " + err.Error())
	}
	switch request.Action {
	case "node.enroll":
		return d.nodeAdmin(ctx, request)
	case "controller.init", "controller.recover":
		return d.controllerAdmin(ctx, request)
	case "config.reload":
		return d.reloadConfig(ctx)
	case "derp.restart":
		d.opMu.Lock()
		defer d.opMu.Unlock()
		if err := d.restartDerper(ctx); err != nil {
			return admin.Failure(err.Error())
		}
		return admin.Success("derper restarted", nil)
	default:
		return admin.Failure(fmt.Sprintf("unknown admin action %q", request.Action))
	}
}

func (d *Daemon) reloadConfig(ctx context.Context) admin.Response {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	parsed, err := config.LoadFile(d.configPath)
	if err != nil {
		return admin.Failure(err.Error())
	}
	pendingRestart := config.RestartOnlyChanged(d.activeConfig(), parsed.Config)
	if err := d.commitConfig(ctx, parsed.Config, parsed.Warnings); err != nil {
		return admin.Failure(err.Error())
	}
	d.logf("INFO configuration reload completed")
	message := "configuration reloaded"
	if pendingRestart {
		message += "; restart required for pending listener, TLS, admin, health, storage, or hostname changes"
	}
	return admin.Success(message, nil)
}

func (d *Daemon) commitConfig(ctx context.Context, cfg config.Config, warnings []string) error {
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := config.WriteAtomic(d.configPath, cfg); err != nil {
		return err
	}
	return d.applyCommittedConfig(ctx, cfg, warnings)
}

func (d *Daemon) applyCommittedConfig(ctx context.Context, cfg config.Config, warnings []string) error {
	d.mu.RLock()
	active := d.current.Clone()
	started := d.started
	d.mu.RUnlock()
	runtime := cfg.Clone()
	if started {
		runtime.Server = active.Server
		runtime.Storage = active.Storage
		runtime.Controller = active.Controller
		runtime.Node = active.Node
	}
	pendingRestart := config.RestartOnlyChanged(runtime, cfg)
	for _, warning := range warnings {
		d.logf("WARN %s", warning)
	}
	d.logFilter.SetLevel(cfg.Logging.Level)
	d.mu.Lock()
	d.desired = cfg.Clone()
	if started {
		d.current = runtime.Clone()
	} else {
		d.current = cfg.Clone()
	}
	d.pendingRestart = pendingRestart
	d.mu.Unlock()
	if err := d.syncDerper(ctx); err != nil {
		d.reportFatal(err)
		return err
	}
	if pendingRestart {
		d.logf("WARN configuration committed with pending restart")
	}
	return nil
}

func (d *Daemon) currentConfig() config.Config {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.desired.Clone()
}

func (d *Daemon) desiredConfig() config.Config {
	return d.currentConfig()
}

func (d *Daemon) activeConfig() config.Config {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.current.Clone()
}

func (d *Daemon) restartDerper(ctx context.Context) error {
	d.derperMu.Lock()
	d.mu.Lock()
	d.childOK = false
	if d.derper.Running() {
		d.expectedChildStops[d.childGeneration] = struct{}{}
	}
	d.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := d.derper.Stop(stopCtx); err != nil {
		d.derperMu.Unlock()
		d.reportFatal(err)
		return err
	}
	d.derperMu.Unlock()
	if err := d.syncDerper(ctx); err != nil {
		d.reportFatal(err)
		return err
	}
	return nil
}

func (d *Daemon) Shutdown() error {
	var err error
	d.closeOnce.Do(func() { err = d.shutdownInternal() })
	return err
}

func (d *Daemon) shutdownInternal() error {
	var err error
	d.mu.Lock()
	d.starting = false
	d.stopping = true
	d.startup = false
	d.childOK = false
	d.mu.Unlock()
	if d.nodeCancel != nil {
		d.nodeCancel()
		<-d.nodeDone
	}
	if d.adminServer != nil {
		if adminErr := d.adminServer.StopAccepting(); adminErr != nil {
			err = errors.Join(err, adminErr)
		}
	}
	if d.adminServer != nil {
		d.adminServer.Wait()
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if d.derper != nil {
		if stopErr := d.derper.Stop(stopCtx); stopErr != nil {
			err = errors.Join(err, stopErr)
		}
		d.derperMu.Lock()
		d.derperMu.Unlock()
	}
	if d.adminServer != nil {
		if adminErr := d.adminServer.Close(); adminErr != nil {
			err = errors.Join(err, adminErr)
		}
	}
	d.closeListeners()
	if d.controllerCancel != nil {
		d.controllerCancel()
		<-d.controllerDone
	}
	if d.controllerStore != nil {
		err = errors.Join(err, d.controllerStore.Close())
	}
	return err
}

func (d *Daemon) closeListeners() {
	if d.controllerServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := d.controllerServer.Shutdown(ctx); err != nil {
			_ = d.controllerServer.Close()
		}
		cancel()
	}
	if d.managementServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := d.managementServer.Shutdown(ctx); err != nil {
			_ = d.managementServer.Close()
		}
		cancel()
	}
	if d.healthServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = d.healthServer.Shutdown(ctx)
		cancel()
	}
	if d.healthListener != nil {
		_ = d.healthListener.Close()
		d.healthListener = nil
	}
	if d.adminListenerStarted {
		d.adminListenerStarted = false
	}
}

func (d *Daemon) publishChildIfCurrent(generation uint64) bool {
	if !d.derper.Running() {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping || d.childGeneration != generation {
		return false
	}
	d.childOK = true
	return true
}
