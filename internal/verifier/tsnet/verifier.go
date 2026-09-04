package tsnet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"multiderp/internal/config"
	"multiderp/internal/verifier"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	tailscaletsnet "tailscale.com/tsnet"
	"tailscale.com/types/key"
)

var ErrUnavailable = errors.New("verifier is not eligible")

type Factory struct {
	PollInterval time.Duration
}

func (f Factory) New(_ context.Context, cfg config.TailnetConfig, stateDir string, logf func(string, ...any)) (verifier.Verifier, error) {
	if logf == nil {
		logf = log.Printf
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create verifier state directory: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("set verifier state directory permissions: %w", err)
	}
	if err := ensureStateFilePrivate(stateDir); err != nil {
		return nil, err
	}
	secret, err := readAuthSecret(cfg.Auth, stateDir)
	if err != nil {
		return nil, err
	}
	hostname := cfg.Hostname
	if hostname == "" {
		hostname = "multiderp-" + cfg.Name
	}
	s := &tailscaletsnet.Server{
		Hostname:      hostname,
		Dir:           stateDir,
		UserLogf:      func(string, ...any) {},
		Logf:          func(string, ...any) {},
		AdvertiseTags: append([]string(nil), cfg.Auth.Tags...),
	}
	switch cfg.Auth.Type {
	case "oauth":
		s.ClientSecret = secret
	case "auth_key":
		s.AuthKey = secret
	}
	interval := f.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &TSNetVerifier{
		name:         cfg.Name,
		authType:     cfg.Auth.Type,
		hostname:     hostname,
		stateDir:     stateDir,
		server:       s,
		logf:         logf,
		pollInterval: interval,
		state:        verifier.StateConfigured,
	}, nil
}

func readAuthSecret(auth config.AuthConfig, stateDir string) (string, error) {
	if auth.Type == "auth_key" {
		statePath := filepath.Join(stateDir, "tailscaled.state")
		if info, err := os.Stat(statePath); err == nil && info.Mode().IsRegular() {
			return "", nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect existing verifier state: %w", err)
		}
	}
	path := auth.ClientSecretFile
	if auth.Type == "auth_key" {
		path = auth.AuthKeyFile
	}
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read authentication secret metadata: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("authentication secret path is not a regular file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("authentication secret file is writable by group or others")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read authentication secret file: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", errors.New("authentication secret file is empty")
	}
	return secret, nil
}

func ensureStateFilePrivate(stateDir string) error {
	path := filepath.Join(stateDir, "tailscaled.state")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect verifier state file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("verifier state path is not a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set verifier state file permissions: %w", err)
	}
	return nil
}

type TSNetVerifier struct {
	lifecycleMu   sync.Mutex
	mu            sync.RWMutex
	name          string
	authType      string
	hostname      string
	stateDir      string
	server        *tailscaletsnet.Server
	lc            *local.Client
	state         verifier.State
	hardening     bool
	authURL       string
	lastError     string
	status        *ipnstate.Status
	logf          func(string, ...any)
	pollInterval  time.Duration
	ctx           context.Context
	cancel        context.CancelFunc
	started       bool
	serverStarted bool
	retryDelay    time.Duration
	retryAt       time.Time
}

func (v *TSNetVerifier) Name() string { return v.name }

func (v *TSNetVerifier) State() verifier.State {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.state
}

func (v *TSNetVerifier) Start(ctx context.Context) error {
	v.lifecycleMu.Lock()
	v.mu.Lock()
	if v.started {
		v.mu.Unlock()
		v.lifecycleMu.Unlock()
		return nil
	}
	v.started = true
	v.state = verifier.StateStarting
	v.ctx, v.cancel = context.WithCancel(context.Background())
	v.retryDelay = time.Second
	v.retryAt = time.Time{}
	v.mu.Unlock()
	v.logf("INFO [%s] verifier state: %s", v.name, verifier.StateStarting.String())
	if err := ctx.Err(); err != nil {
		v.setFailure(verifier.StateError, err)
		v.lifecycleMu.Unlock()
		return err
	}
	lc, err := v.server.LocalClient()
	if err != nil {
		v.setFailure(verifier.StateDegraded, err)
		v.lifecycleMu.Unlock()
		return err
	}
	v.mu.Lock()
	v.lc = lc
	v.serverStarted = true
	v.mu.Unlock()
	if err := ensureStateFilePrivate(v.stateDir); err != nil {
		v.lifecycleMu.Unlock()
		_ = v.Close()
		return err
	}
	if err := v.reconcileLocked(ctx); err != nil {
		v.logf("WARN [%s] verifier reconcile: %v", v.name, err)
	}
	v.mu.RLock()
	watchCtx := v.ctx
	v.mu.RUnlock()
	v.lifecycleMu.Unlock()
	go v.watch(watchCtx)
	return nil
}

func (v *TSNetVerifier) watch(ctx context.Context) {
	ticker := time.NewTicker(v.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := v.reconcile(ctx); err != nil {
				v.logf("WARN [%s] verifier recovery: %v", v.name, err)
			}
		}
	}
}

func (v *TSNetVerifier) reconcile(ctx context.Context) error {
	v.lifecycleMu.Lock()
	defer v.lifecycleMu.Unlock()
	return v.reconcileLocked(ctx)
}

func (v *TSNetVerifier) reconcileLocked(ctx context.Context) error {
	v.mu.RLock()
	lc := v.lc
	hardened := v.hardening
	state := v.state
	retryAt := v.retryAt
	v.mu.RUnlock()
	if state == verifier.StateStopping {
		return errors.New("verifier is stopping")
	}
	if state == verifier.StateError {
		return nil
	}
	if lc == nil {
		return errors.New("local client is not initialized")
	}
	status, err := lc.Status(ctx)
	if err != nil {
		return v.failureWithRetry(verifier.StateDegraded, err)
	}
	v.mu.Lock()
	v.status = status
	v.authURL = status.AuthURL
	v.mu.Unlock()
	if status.BackendState == ipn.NeedsLogin.String() || status.BackendState == ipn.NeedsMachineAuth.String() || !status.HaveNodeKey {
		hardened = false
		v.setHardening(false)
		if v.authType == "web" && status.AuthURL == "" {
			if err := lc.StartLoginInteractive(ctx); err != nil {
				return v.failureForHardening(verifier.StateDegraded, wrapCompatibilityError("StartLoginInteractive", err, "start interactive login"))
			}
			status, err = waitForLoginStatus(ctx, lc)
			if err != nil {
				return v.failureWithRetry(verifier.StateDegraded, fmt.Errorf("read login status: %w", err))
			}
			v.mu.Lock()
			v.status = status
			v.authURL = status.AuthURL
			v.mu.Unlock()
			if status.BackendState != ipn.NeedsLogin.String() && status.HaveNodeKey {
				// The login completed while the request was being prepared. Continue
				// through the normal running and hardening checks below.
			} else {
				v.setWaitingForLogin()
				return nil
			}
		} else {
			v.setWaitingForLogin()
			return nil
		}
	}
	if status.BackendState != ipn.Running.String() {
		return v.failureWithRetry(verifier.StateDegraded, fmt.Errorf("backend state is %q", status.BackendState))
	}
	if hardened {
		v.setState(verifier.StateConnected)
		return nil
	}
	if state == verifier.StateDegraded && !retryAt.IsZero() && time.Now().Before(retryAt) {
		return nil
	}
	v.setState(verifier.StateHardening)
	if err := ApplyAndValidate(ctx, lc); err != nil {
		v.setHardening(false)
		return v.failureForHardening(verifier.StateDegraded, err)
	}
	status, err = lc.Status(ctx)
	if err != nil {
		v.setHardening(false)
		return v.failureWithRetry(verifier.StateDegraded, err)
	}
	if status.BackendState != ipn.Running.String() || !status.HaveNodeKey {
		v.setHardening(false)
		return v.failureWithRetry(verifier.StateDegraded, errors.New("verifier stopped during hardening"))
	}
	v.mu.Lock()
	v.status = status
	v.authURL = status.AuthURL
	v.lastError = ""
	v.hardening = true
	v.state = verifier.StateConnected
	v.retryDelay = time.Second
	v.retryAt = time.Time{}
	v.mu.Unlock()
	v.logf("INFO [%s] hardening verified", v.name)
	return nil
}

func (v *TSNetVerifier) Login(ctx context.Context) (string, error) {
	v.lifecycleMu.Lock()
	defer v.lifecycleMu.Unlock()
	if v.authType != "web" {
		return "", errors.New("interactive login is only available for web authentication")
	}
	v.mu.RLock()
	lc := v.lc
	v.mu.RUnlock()
	if lc == nil {
		return "", errors.New("verifier is not started")
	}
	if err := lc.StartLoginInteractive(ctx); err != nil {
		return "", fmt.Errorf("start login: %w", err)
	}
	status, err := waitForLoginStatus(ctx, lc)
	if err != nil {
		return "", fmt.Errorf("read login status: %w", err)
	}
	v.mu.Lock()
	v.status = status
	v.authURL = status.AuthURL
	changed := v.state != verifier.StateWaitingForLogin
	v.state = verifier.StateWaitingForLogin
	v.hardening = false
	v.lastError = ""
	v.retryDelay = time.Second
	v.retryAt = time.Time{}
	v.mu.Unlock()
	if changed {
		v.logf("INFO [%s] verifier state: %s", v.name, verifier.StateWaitingForLogin.String())
	}
	return v.AuthURL(), nil
}

const loginURLWait = 2 * time.Second

func waitForLoginStatus(ctx context.Context, lc *local.Client) (*ipnstate.Status, error) {
	if lc == nil {
		return nil, errors.New("local client is nil")
	}
	pollCtx, cancel := context.WithTimeout(ctx, loginURLWait)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last *ipnstate.Status
	for {
		status, err := lc.Status(pollCtx)
		if err != nil {
			if last != nil && errors.Is(pollCtx.Err(), context.DeadlineExceeded) {
				return last, nil
			}
			return nil, err
		}
		if status == nil {
			return nil, errors.New("login status is nil")
		}
		last = status
		if status.AuthURL != "" || status.BackendState != ipn.NeedsLogin.String() {
			return status, nil
		}
		select {
		case <-ticker.C:
		case <-pollCtx.Done():
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) {
				return last, nil
			}
			return nil, pollCtx.Err()
		}
	}
}

func (v *TSNetVerifier) AuthURL() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.authURL
}

func (v *TSNetVerifier) Logout(ctx context.Context) error {
	v.lifecycleMu.Lock()
	defer v.lifecycleMu.Unlock()
	v.mu.RLock()
	lc := v.lc
	v.mu.RUnlock()
	if lc == nil {
		return errors.New("verifier is not started")
	}
	if err := lc.Logout(ctx); err != nil {
		return fmt.Errorf("logout verifier: %w", err)
	}
	v.mu.Lock()
	v.hardening = false
	changed := v.state != verifier.StateConfigured
	v.state = verifier.StateConfigured
	v.lastError = ""
	v.retryDelay = time.Second
	v.retryAt = time.Time{}
	v.mu.Unlock()
	if changed {
		v.logf("INFO [%s] verifier state: %s", v.name, verifier.StateConfigured.String())
	}
	return nil
}

func (v *TSNetVerifier) ContainsNode(ctx context.Context, node key.NodePublic) (bool, error) {
	v.mu.RLock()
	lc := v.lc
	eligible := v.state == verifier.StateConnected && v.hardening
	v.mu.RUnlock()
	if lc == nil || !eligible {
		return false, ErrUnavailable
	}
	_, err := lc.WhoIsNodeKey(ctx, node)
	if errors.Is(err, local.ErrPeerNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (v *TSNetVerifier) Status(context.Context) verifier.Status {
	return v.statusSnapshot(false)
}

func (v *TSNetVerifier) VerboseStatus(context.Context) verifier.Status {
	return v.statusSnapshot(true)
}

func (v *TSNetVerifier) statusSnapshot(verbose bool) verifier.Status {
	v.mu.RLock()
	defer v.mu.RUnlock()
	status := verifier.Status{
		Name:              v.name,
		Authentication:    v.authType,
		State:             v.state,
		HardeningVerified: v.hardening,
		AuthURL:           v.authURL,
		Node:              v.hostname,
		StateDirectory:    v.stateDir,
		LastError:         v.lastError,
	}
	if v.status != nil {
		status.Tailnet = ""
		if v.status.CurrentTailnet != nil {
			status.Tailnet = v.status.CurrentTailnet.Name
		}
		for _, ip := range v.status.TailscaleIPs {
			status.TailscaleIPs = append(status.TailscaleIPs, ip.String())
		}
		if verbose && v.status.Self != nil && !v.status.Self.PublicKey.IsZero() {
			status.NodeKey = v.status.Self.PublicKey.String()
		}
	}
	return status
}

func (v *TSNetVerifier) Close() error {
	v.mu.RLock()
	cancel := v.cancel
	v.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	v.lifecycleMu.Lock()
	v.mu.Lock()
	if v.cancel != nil {
		v.cancel()
	}
	changed := v.state != verifier.StateStopping
	v.state = verifier.StateStopping
	v.hardening = false
	server := v.server
	serverStarted := v.serverStarted
	v.serverStarted = false
	v.mu.Unlock()
	if changed {
		v.logf("INFO [%s] verifier state: %s", v.name, verifier.StateStopping.String())
	}
	if server == nil || !serverStarted {
		v.lifecycleMu.Unlock()
		return nil
	}
	err := server.Close()
	v.lifecycleMu.Unlock()
	if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

func (v *TSNetVerifier) setState(state verifier.State) {
	v.mu.Lock()
	changed := v.state != state
	v.state = state
	if state == verifier.StateConnected {
		v.lastError = ""
	}
	v.mu.Unlock()
	if changed {
		v.logf("INFO [%s] verifier state: %s", v.name, state.String())
	}
}

func (v *TSNetVerifier) setHardening(value bool) {
	v.mu.Lock()
	v.hardening = value
	v.mu.Unlock()
}

func (v *TSNetVerifier) setWaitingForLogin() {
	v.mu.Lock()
	changed := v.state != verifier.StateWaitingForLogin
	v.state = verifier.StateWaitingForLogin
	v.hardening = false
	v.lastError = ""
	v.retryDelay = time.Second
	v.retryAt = time.Time{}
	v.mu.Unlock()
	if changed {
		v.logf("INFO [%s] verifier state: %s", v.name, verifier.StateWaitingForLogin.String())
	}
}

func (v *TSNetVerifier) failureWithRetry(state verifier.State, err error) error {
	v.setFailure(state, err)
	v.scheduleRetry()
	return err
}

func (v *TSNetVerifier) failureForHardening(state verifier.State, err error) error {
	if errors.Is(err, ErrHardeningCompatibility) {
		return v.setFailure(verifier.StateError, err)
	}
	return v.failureWithRetry(state, err)
}

func (v *TSNetVerifier) scheduleRetry() {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if !v.retryAt.IsZero() && now.Before(v.retryAt) {
		return
	}
	delay := v.retryDelay
	if delay <= 0 {
		delay = time.Second
	}
	wait := time.Duration(rand.Int63n(int64(delay) + 1))
	v.retryAt = now.Add(wait)
	if delay < time.Minute {
		v.retryDelay = delay * 2
		if v.retryDelay > time.Minute {
			v.retryDelay = time.Minute
		}
	}
}

func (v *TSNetVerifier) setFailure(state verifier.State, err error) error {
	v.mu.Lock()
	changed := v.state != state
	v.state = state
	v.hardening = false
	if err != nil {
		v.lastError = err.Error()
	}
	if state == verifier.StateError {
		v.retryAt = time.Time{}
		v.retryDelay = time.Second
	}
	v.mu.Unlock()
	if changed {
		v.logf("INFO [%s] verifier state: %s", v.name, state.String())
	}
	return err
}
