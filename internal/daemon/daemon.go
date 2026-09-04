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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsy223622/MultiDERP/internal/admin"
	"github.com/lsy223622/MultiDERP/internal/admission"
	"github.com/lsy223622/MultiDERP/internal/config"
	"github.com/lsy223622/MultiDERP/internal/derper"
	"github.com/lsy223622/MultiDERP/internal/health"
	"github.com/lsy223622/MultiDERP/internal/logging"
	"github.com/lsy223622/MultiDERP/internal/verifier"
)

type Options struct {
	ConfigPath       string
	ConfigTemplate   []byte
	DerperBinary     string
	DerperOutput     io.Writer
	AdmissionAddress string
	Factory          verifier.Factory
	Logger           *log.Logger
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
	pendingOperationMu sync.Mutex
	derperMu           sync.Mutex
	started            bool
	starting           bool
	startup            bool
	stopping           bool
	childOK            bool
	childGeneration    uint64
	barrierEpoch       uint64
	expectedChildStops map[uint64]struct{}

	configPath       string
	configTemplate   []byte
	derperBinary     string
	derperOutput     io.Writer
	admissionAddress string
	logFilter        *logging.Filter
	logf             func(string, ...any)

	current        config.Config
	desired        config.Config
	pendingRestart bool
	manager        *VerifierManager
	admission      *admission.Controller
	derper         derperProcess

	adminServer          *admin.Server
	adminListenerStarted bool
	healthServer         *http.Server
	healthListener       net.Listener
	admissionServer      *http.Server
	admissionListener    net.Listener

	fatal            chan error
	fatalOnce        sync.Once
	closeOnce        sync.Once
	admissionServing atomic.Bool
}

func New(parent context.Context, options Options) *Daemon {
	if options.ConfigPath == "" {
		options.ConfigPath = config.DefaultConfigPath
	}
	if options.AdmissionAddress == "" {
		options.AdmissionAddress = config.DefaultAdmissionAddress
	}
	if options.DerperBinary == "" {
		options.DerperBinary = "derper"
	}
	if options.DerperOutput == nil {
		options.DerperOutput = io.Discard
	}
	if options.Logger == nil {
		options.Logger = log.New(os.Stderr, "multiderp: ", log.LstdFlags|log.Lmicroseconds)
	}
	if len(options.ConfigTemplate) == 0 {
		options.ConfigTemplate = config.ExampleYAML()
	}
	logFilter := logging.New(options.Logger, config.DefaultLoggingLevel)
	pool := admission.NewPool()
	d := &Daemon{
		configPath:         options.ConfigPath,
		configTemplate:     append([]byte(nil), options.ConfigTemplate...),
		derperBinary:       options.DerperBinary,
		derperOutput:       options.DerperOutput,
		admissionAddress:   options.AdmissionAddress,
		logFilter:          logFilter,
		logf:               logFilter.Printf,
		admission:          admission.NewController(pool, admission.DefaultLimits()),
		fatal:              make(chan error, 1),
		expectedChildStops: make(map[uint64]struct{}),
	}
	d.derper = derper.NewProcess(d.derperBinary, d.derperOutput)
	d.manager = NewVerifierManager(parent, ManagerOptions{
		Pool:    pool,
		Factory: options.Factory,
		Logger:  options.Logger,
		Logf:    d.logf,
		OnChange: func(int) {
			d.poolChanged(parent)
		},
	})
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
	d.pendingOperationMu.Lock()
	err = d.recoverPendingOperationLocked(ctx)
	d.pendingOperationMu.Unlock()
	if err != nil {
		return d.abortStart(err)
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
	d.denyAdmission()
	if err := d.startAdmissionServer(); err != nil {
		return d.abortStart(err)
	}
	if err := d.startAdminServer(parsed.Config.Server.Admin.Socket); err != nil {
		return d.abortStart(err)
	}
	if err := d.startHealthServer(parsed.Config.Server.Health.Listen); err != nil {
		return d.abortStart(err)
	}
	if err := d.manager.Reconcile(ctx, parsed.Config); err != nil {
		return d.abortStart(err)
	}
	if err := d.syncDerper(ctx); err != nil {
		return d.abortStart(err)
	}
	d.mu.Lock()
	d.started = true
	d.starting = false
	d.startup = true
	d.mu.Unlock()
	if d.manager.EligibleCount() > 0 && !d.derper.Running() {
		if err := d.syncDerper(ctx); err != nil {
			return d.abortStart(err)
		}
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

func (d *Daemon) poolChanged(parent context.Context) {
	d.mu.RLock()
	started := d.started && !d.stopping
	d.mu.RUnlock()
	if !started {
		return
	}
	d.denyAdmission()
	go func() {
		if err := d.syncDerper(parent); err != nil {
			d.reportFatal(err)
		}
	}()
}

func (d *Daemon) syncDerper(ctx context.Context) error {
	d.derperMu.Lock()
	defer d.derperMu.Unlock()
	d.mu.RLock()
	if d.stopping {
		d.mu.RUnlock()
		return errors.New("daemon is stopping")
	}
	cfg := d.current.Clone()
	barrierEpoch := d.barrierEpoch
	childGeneration := d.childGeneration
	d.mu.RUnlock()
	if d.manager.EligibleCount() == 0 {
		d.denyAdmission()
		return nil
	}
	if d.derper.Running() {
		d.mu.Lock()
		stopping := d.stopping
		if !stopping && d.childOK && d.barrierEpoch == barrierEpoch && d.childGeneration == childGeneration {
			d.admission.SetBarrier(true)
		}
		d.mu.Unlock()
		if stopping {
			return errors.New("daemon is stopping")
		}
		return nil
	}
	barrierEpoch = d.denyAdmission()
	d.mu.Lock()
	d.childOK = false
	d.childGeneration++
	childGeneration = d.childGeneration
	d.mu.Unlock()
	keyPath := filepath.Join(cfg.Storage.StateDir, "derper", "derper.key")
	d.logf("INFO starting derper child")
	if err := d.derper.Start(ctx, cfg.Server, d.admissionAddress, keyPath); err != nil {
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
	if !d.publishChildIfCurrent(barrierEpoch, childGeneration) {
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
			d.barrierEpoch++
			d.admission.SetBarrier(false)
		}
		d.mu.Unlock()
		if !intentional {
			if err == nil {
				err = errors.New("derper exited unexpectedly")
			}
			d.logf("ERROR derper child exited: %v", err)
			d.reportFatal(fmt.Errorf("derper child failed: %w", err))
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

func (d *Daemon) startAdmissionServer() error {
	if err := derper.ValidateAdmissionAddress(d.admissionAddress); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", d.admissionAddress)
	if err != nil {
		return fmt.Errorf("listen admission controller on %q: %w", d.admissionAddress, err)
	}
	d.admissionListener = listener
	d.admissionServer = &http.Server{Handler: d.admission.Handler()}
	d.admissionServing.Store(true)
	go func() {
		defer d.admissionServing.Store(false)
		if err := d.admissionServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.reportFatal(fmt.Errorf("admission server: %w", err))
		}
	}()
	return nil
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
	pendingRestart := d.pendingRestart
	d.mu.RUnlock()
	if live && d.started {
		live = d.manager.Running() && d.admission.Running() && d.admissionServing.Load() && d.adminServer != nil && d.adminServer.Running()
	}
	statuses := d.manager.List(context.Background())
	requiredFailures := 0
	for _, status := range statuses {
		if status.EffectiveRequired && !verifier.Eligible(status) {
			requiredFailures++
		}
	}
	eligible := d.manager.EligibleCount()
	return health.Snapshot{
		Live:              live,
		Startup:           startup,
		DerperUsable:      childOK,
		EligibleVerifiers: eligible,
		RequiredFailures:  requiredFailures,
		PendingRestart:    pendingRestart,
		Ready:             childOK && eligible > 0 && requiredFailures == 0,
	}
}

func (d *Daemon) handleRequest(ctx context.Context, request admin.Request) admin.Response {
	d.mu.RLock()
	stopping := d.stopping
	d.mu.RUnlock()
	if stopping {
		return admin.Failure("daemon is stopping")
	}
	d.pendingOperationMu.Lock()
	defer d.pendingOperationMu.Unlock()
	if err := d.recoverPendingOperationLocked(ctx); err != nil {
		return admin.Failure("pending operation recovery failed: " + err.Error())
	}
	if err := ctx.Err(); err != nil {
		return admin.Failure("admin request canceled: " + err.Error())
	}
	switch request.Action {
	case "tailnet.list":
		return admin.Success("", d.manager.List(ctx))
	case "tailnet.status":
		var status verifier.Status
		var err error
		if request.Verbose {
			status, err = d.manager.StatusVerbose(request.Name, ctx)
		} else {
			status, err = d.manager.Status(request.Name, ctx)
		}
		if err != nil {
			return admin.Failure(err.Error())
		}
		return admin.Success("", status)
	case "tailnet.add":
		return d.addTailnet(ctx, request)
	case "tailnet.enable", "tailnet.disable":
		return d.setTailnetDisabled(ctx, request)
	case "tailnet.login":
		d.opMu.Lock()
		defer d.opMu.Unlock()
		url, err := d.manager.Login(ctx, request.Name)
		if err != nil {
			return admin.Failure(err.Error())
		}
		return admin.Success("authentication required", map[string]string{"auth_url": url})
	case "tailnet.logout":
		d.opMu.Lock()
		defer d.opMu.Unlock()
		if err := d.manager.Logout(ctx, request.Name); err != nil {
			return admin.Failure(err.Error())
		}
		return admin.Success("verifier logged out", nil)
	case "tailnet.reset":
		d.opMu.Lock()
		defer d.opMu.Unlock()
		if err := d.manager.Reset(ctx, request.Name); err != nil {
			return admin.Failure(err.Error())
		}
		if err := d.syncDerper(ctx); err != nil {
			d.reportFatal(err)
			return admin.Failure(err.Error())
		}
		return d.verifierResponse(ctx, request.Name, "verifier reset")
	case "tailnet.remove":
		return d.removeTailnet(ctx, request)
	case "orphan.list":
		items, err := d.manager.ListOrphans()
		if err != nil {
			return admin.Failure(err.Error())
		}
		return admin.Success("", items)
	case "orphan.purge":
		if !request.Confirm {
			return admin.Failure("orphan purge requires explicit confirmation")
		}
		d.opMu.Lock()
		defer d.opMu.Unlock()
		if err := d.manager.PurgeOrphan(request.Name); err != nil {
			return admin.Failure(err.Error())
		}
		return admin.Success("orphan state purged: "+request.Name, nil)
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

func (d *Daemon) addTailnet(ctx context.Context, request admin.Request) admin.Response {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	cfg := d.currentConfig()
	for _, item := range cfg.Tailnets {
		if strings.EqualFold(item.Name, request.Name) {
			return admin.Failure(fmt.Sprintf("verifier %q already exists", request.Name))
		}
	}
	authType := request.AuthType
	if authType == "" {
		authType = "web"
	}
	item := config.TailnetConfig{Name: request.Name, Auth: config.AuthConfig{Type: authType, ClientSecretFile: request.ClientSecretFile, AuthKeyFile: request.AuthKeyFile, Tags: append([]string(nil), request.Tags...)}}
	if request.Required != nil {
		item.Required = *request.Required
	}
	cfg.Tailnets = append(cfg.Tailnets, item)
	if err := d.commitConfig(ctx, cfg, nil); err != nil {
		return admin.Failure(err.Error())
	}
	status, err := d.manager.Status(request.Name, ctx)
	if err != nil {
		return admin.Success("verifier created", nil)
	}
	message := "verifier created"
	data := map[string]any{"status": status}
	if status.AuthURL != "" {
		message = "authentication required"
		data["auth_url"] = status.AuthURL
	}
	return admin.Success(message, data)
}

func (d *Daemon) setTailnetDisabled(ctx context.Context, request admin.Request) admin.Response {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	cfg := d.currentConfig()
	found := false
	for i := range cfg.Tailnets {
		if strings.EqualFold(cfg.Tailnets[i].Name, request.Name) {
			cfg.Tailnets[i].Disabled = request.Action == "tailnet.disable"
			found = true
			break
		}
	}
	if !found {
		return admin.Failure(fmt.Sprintf("verifier %q not found", request.Name))
	}
	if err := d.commitConfig(ctx, cfg, nil); err != nil {
		return admin.Failure(err.Error())
	}
	return d.verifierResponse(ctx, request.Name, "verifier configuration updated")
}

func (d *Daemon) verifierResponse(ctx context.Context, name, message string) admin.Response {
	status, err := d.manager.Status(name, ctx)
	if err != nil {
		return admin.Success(message, nil)
	}
	data := map[string]any{"status": status}
	if status.AuthURL != "" {
		return admin.Success("authentication required", map[string]any{"status": status, "auth_url": status.AuthURL})
	}
	return admin.Success(message, data)
}

func (d *Daemon) removeTailnet(ctx context.Context, request admin.Request) admin.Response {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	oldConfig := d.currentConfig()
	cfg := oldConfig.Clone()
	newTailnets := make([]config.TailnetConfig, 0, len(cfg.Tailnets))
	actualName := ""
	for _, item := range cfg.Tailnets {
		if strings.EqualFold(item.Name, request.Name) {
			actualName = item.Name
			continue
		}
		newTailnets = append(newTailnets, item)
	}
	if actualName == "" {
		return admin.Failure(fmt.Sprintf("verifier %q not found", request.Name))
	}
	cfg.Tailnets = newTailnets
	if err := cfg.Validate(); err != nil {
		return admin.Failure(err.Error())
	}
	orphan, err := d.manager.PrepareOrphan(actualName)
	if err != nil {
		return admin.Failure(err.Error())
	}
	runtime := d.activeConfig()
	stateRoot := runtime.Storage.TailnetStateDir
	orphanRoot := runtime.Storage.OrphanStateDir
	stateDir := filepath.Join(stateRoot, actualName)
	orphanDir := filepath.Join(orphanRoot, orphan.ID)
	if !config.IsWithin(stateRoot, stateDir) || !config.IsWithin(orphanRoot, orphanDir) {
		return admin.Failure("remove verifier paths escaped configured storage roots")
	}
	operationPath := d.removeOperationPath()
	operation := config.RemoveOperation{
		Version:    config.RemoveOperationVersion,
		Phase:      config.RemovePhasePrepared,
		Name:       actualName,
		StateRoot:  stateRoot,
		OrphanRoot: orphanRoot,
		StateDir:   stateDir,
		OrphanDir:  orphanDir,
		Orphan:     config.OrphanMetadata{ID: orphan.ID, Name: orphan.Name, CreatedAt: orphan.CreatedAt},
		OldConfig:  oldConfig,
		NewConfig:  cfg,
	}
	if err := config.WriteRemoveOperation(operationPath, operation); err != nil {
		return admin.Failure(err.Error())
	}
	denyEpoch := d.denyAdmission()
	if err := config.WriteAtomic(d.configPath, cfg); err != nil {
		removeErr := config.RemoveRemoveOperation(operationPath)
		d.restoreAdmission(denyEpoch)
		return admin.Failure(errors.Join(err, removeErr).Error())
	}
	operation.Phase = config.RemovePhaseConfigCommit
	if err := config.WriteRemoveOperation(operationPath, operation); err != nil {
		return admin.Failure(fmt.Sprintf("remove verifier committed configuration but could not advance pending operation: %v", err))
	}
	if _, err := d.manager.OrphanWithInfo(actualName, orphan); err != nil {
		return admin.Failure(fmt.Sprintf("remove verifier is pending state preservation: %v", err))
	}
	operation.Phase = config.RemovePhaseStateMoved
	if err := config.WriteRemoveOperation(operationPath, operation); err != nil {
		return admin.Failure(fmt.Sprintf("remove verifier moved state but could not advance pending operation: %v", err))
	}
	if err := d.applyCommittedConfig(ctx, cfg, nil); err != nil {
		return admin.Failure(fmt.Sprintf("remove verifier is pending runtime reconciliation: %v", err))
	}
	if err := config.RemoveRemoveOperation(operationPath); err != nil {
		return admin.Failure(fmt.Sprintf("remove verifier completed but pending operation cleanup failed: %v", err))
	}
	return admin.Success(fmt.Sprintf("verifier removed; state preserved as orphan %s", orphan.ID), map[string]string{"orphan_id": orphan.ID})
}

func (d *Daemon) reloadConfig(ctx context.Context) admin.Response {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	parsed, err := config.LoadFile(d.configPath)
	if err != nil {
		return admin.Failure(err.Error())
	}
	if err := config.ValidateReload(d.desiredConfig(), parsed.Config); err != nil {
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
	d.denyAdmission()
	if err := d.manager.Reconcile(ctx, runtime); err != nil {
		return err
	}
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
	d.denyAdmission()
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
	d.denyAdmission()
	if d.adminServer != nil {
		if adminErr := d.adminServer.StopAccepting(); adminErr != nil {
			err = errors.Join(err, adminErr)
		}
	}
	if d.admission != nil {
		d.admission.Close()
	}
	if d.adminServer != nil {
		d.adminServer.Wait()
	}
	if d.manager != nil {
		d.manager.Pool().Clear()
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
	if d.manager != nil {
		if managerErr := d.manager.Close(); managerErr != nil {
			err = errors.Join(err, managerErr)
		}
	}
	if d.adminServer != nil {
		if adminErr := d.adminServer.Close(); adminErr != nil {
			err = errors.Join(err, adminErr)
		}
	}
	d.closeListeners()
	return err
}

func (d *Daemon) closeListeners() {
	if d.healthServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = d.healthServer.Shutdown(ctx)
		cancel()
	}
	if d.admissionServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = d.admissionServer.Shutdown(ctx)
		cancel()
	}
	if d.healthListener != nil {
		_ = d.healthListener.Close()
		d.healthListener = nil
	}
	if d.admissionListener != nil {
		d.admissionServing.Store(false)
		_ = d.admissionListener.Close()
		d.admissionListener = nil
	}
	if d.adminListenerStarted {
		d.adminListenerStarted = false
	}
}

func (d *Daemon) denyAdmission() uint64 {
	d.mu.Lock()
	d.barrierEpoch++
	epoch := d.barrierEpoch
	d.admission.SetBarrier(false)
	d.mu.Unlock()
	return epoch
}

func (d *Daemon) publishChildIfCurrent(epoch, generation uint64) bool {
	if !d.derper.Running() {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping || d.barrierEpoch != epoch || d.childGeneration != generation {
		return false
	}
	d.childOK = true
	d.admission.SetBarrier(true)
	return true
}

func (d *Daemon) restoreAdmission(epoch uint64) {
	if d.manager.EligibleCount() == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping || d.barrierEpoch != epoch || !d.childOK {
		return
	}
	d.admission.SetBarrier(true)
}
