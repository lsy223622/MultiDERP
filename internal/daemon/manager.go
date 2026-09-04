package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lsy223622/MultiDERP/internal/admission"
	"github.com/lsy223622/MultiDERP/internal/config"
	"github.com/lsy223622/MultiDERP/internal/verifier"
	verifiertsnet "github.com/lsy223622/MultiDERP/internal/verifier/tsnet"
	"gopkg.in/yaml.v3"
)

type managedVerifier struct {
	cfg        config.TailnetConfig
	v          verifier.Verifier
	lastError  string
	retryDelay time.Duration
	nextRetry  time.Time
}

var ErrStateIdentityConflict = errors.New("verifier state identity conflicts with configuration")

type stateIdentity struct {
	Version          int      `yaml:"version"`
	Name             string   `yaml:"name"`
	Hostname         string   `yaml:"hostname"`
	AuthType         string   `yaml:"auth_type"`
	ClientSecretFile string   `yaml:"client_secret_file,omitempty"`
	AuthKeyFile      string   `yaml:"auth_key_file,omitempty"`
	Tags             []string `yaml:"tags,omitempty"`
}

const stateIdentityFile = ".multiderp-identity.yaml"

type VerifierManager struct {
	mu            sync.RWMutex
	applyMu       sync.Mutex
	eligibilityMu sync.Mutex
	records       map[string]*managedVerifier
	desired       config.Config
	pool          *admission.Pool
	factory       verifier.Factory
	logf          func(string, ...any)
	onChange      func(int)

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

type ManagerOptions struct {
	Pool     *admission.Pool
	Factory  verifier.Factory
	Logger   *log.Logger
	Logf     func(string, ...any)
	OnChange func(int)
}

func NewVerifierManager(parent context.Context, options ManagerOptions) *VerifierManager {
	ctx, cancel := context.WithCancel(parent)
	logf := log.Printf
	if options.Logf != nil {
		logf = options.Logf
	} else if options.Logger != nil {
		logf = options.Logger.Printf
	}
	pool := options.Pool
	if pool == nil {
		pool = admission.NewPool()
	}
	factory := options.Factory
	if factory == nil {
		factory = verifiertsnet.Factory{}
	}
	m := &VerifierManager{
		records:  make(map[string]*managedVerifier),
		pool:     pool,
		factory:  factory,
		logf:     logf,
		onChange: options.OnChange,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	go m.watch()
	return m
}

func (m *VerifierManager) Pool() *admission.Pool {
	return m.pool
}

func (m *VerifierManager) Running() bool {
	return !isManagerClosed(m.done)
}

func (m *VerifierManager) Reconcile(ctx context.Context, desired config.Config) error {
	desired.Normalize()
	if err := desired.Validate(); err != nil {
		return err
	}
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.eligibilityMu.Lock()
	defer m.eligibilityMu.Unlock()

	byName := make(map[string]config.TailnetConfig, len(desired.Tailnets))
	for _, item := range desired.Tailnets {
		byName[strings.ToLower(item.Name)] = item
	}
	m.mu.RLock()
	old := make(map[string]*managedVerifier, len(m.records))
	for name, record := range m.records {
		old[name] = record
	}
	m.mu.RUnlock()

	for name, record := range old {
		if _, ok := byName[strings.ToLower(name)]; ok {
			continue
		}
		m.stopRecord(name, record)
		m.mu.Lock()
		if m.records[name] == record {
			delete(m.records, name)
		}
		m.mu.Unlock()
	}

	m.mu.Lock()
	m.desired = desired.Clone()
	m.mu.Unlock()
	for _, cfg := range desired.Tailnets {
		key := strings.ToLower(cfg.Name)
		m.mu.RLock()
		record := m.records[key]
		m.mu.RUnlock()
		if record == nil {
			record = &managedVerifier{cfg: cfg, retryDelay: time.Second}
			m.mu.Lock()
			m.records[key] = record
			m.mu.Unlock()
		} else {
			m.mu.RLock()
			changed := identityChanged(record.cfg, cfg)
			m.mu.RUnlock()
			if changed {
				m.stopRecord(key, record)
				record = &managedVerifier{cfg: cfg, retryDelay: time.Second}
				m.mu.Lock()
				m.records[key] = record
				m.mu.Unlock()
			} else {
				m.mu.Lock()
				record.cfg = cfg
				m.mu.Unlock()
			}
		}

		if cfg.Disabled {
			m.stopRecord(key, record)
			m.mu.Lock()
			record.cfg = cfg
			record.v = nil
			record.lastError = ""
			record.nextRetry = time.Time{}
			m.mu.Unlock()
			continue
		}
		m.mu.RLock()
		errored := record.v != nil && record.v.State() == verifier.StateError
		m.mu.RUnlock()
		if errored {
			m.pool.Remove(key)
			if err := m.stopRecord(key, record); err != nil {
				m.logf("WARN [%s] could not close compatibility-failed verifier before retry: %v", key, err)
				continue
			}
			m.mu.Lock()
			record.lastError = ""
			record.retryDelay = time.Second
			record.nextRetry = time.Time{}
			m.mu.Unlock()
		}
		m.startRecord(ctx, key, record)
	}
	m.syncEligibilityLocked(ctx)
	return nil
}

func identityChanged(old, new config.TailnetConfig) bool {
	return old.Name != new.Name || old.Hostname != new.Hostname || old.Auth.Type != new.Auth.Type ||
		old.Auth.ClientSecretFile != new.Auth.ClientSecretFile || old.Auth.AuthKeyFile != new.Auth.AuthKeyFile ||
		!sameStrings(old.Auth.Tags, new.Auth.Tags)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *VerifierManager) startRecord(ctx context.Context, name string, record *managedVerifier) {
	m.mu.RLock()
	cfg := record.cfg
	current := m.records[name] == record
	started := record.v != nil
	m.mu.RUnlock()
	if !current || cfg.Disabled || started {
		return
	}
	stateDir, err := m.stateDir(cfg.Name)
	if err == nil {
		err = ensurePrivateDir(filepath.Dir(stateDir))
	}
	if err == nil {
		err = ensurePrivateDir(stateDir)
	}
	if err == nil {
		err = ensureStateIdentity(stateDir, cfg)
	}
	if err == nil {
		v, factoryErr := m.factory.New(ctx, cfg, stateDir, m.logf)
		if factoryErr != nil {
			err = factoryErr
		} else {
			if starter, ok := v.(verifier.Starter); ok {
				err = starter.Start(ctx)
			}
			if err != nil {
				_ = v.Close()
			} else {
				accepted := false
				m.mu.Lock()
				if m.records[name] == record && record.v == nil {
					record.v = v
					record.lastError = ""
					record.retryDelay = time.Second
					record.nextRetry = time.Time{}
					accepted = true
				} else {
					_ = v.Close()
				}
				m.mu.Unlock()
				if accepted {
					if callbackSetter, ok := v.(verifier.IneligibleCallbackSetter); ok {
						callbackSetter.SetIneligibleCallback(func() {
							m.mu.RLock()
							current := m.records[name] == record && record.v == v
							m.mu.RUnlock()
							if current {
								m.pool.Remove(name)
							}
						})
					}
				}
			}
		}
	}
	if err != nil {
		m.mu.Lock()
		if m.records[name] == record {
			record.lastError = err.Error()
			if errors.Is(err, ErrStateIdentityConflict) {
				record.nextRetry = time.Time{}
			} else {
				record.nextRetry = time.Now().Add(fullJitter(record.retryDelay))
				if record.retryDelay < time.Minute {
					record.retryDelay *= 2
					if record.retryDelay > time.Minute {
						record.retryDelay = time.Minute
					}
				}
			}
		}
		m.mu.Unlock()
		m.logf("WARN [%s] verifier unavailable: %v", name, err)
	}
}

func fullJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return time.Second
	}
	return time.Duration(rand.Int63n(int64(max) + 1))
}

func ensureStateIdentity(dir string, cfg config.TailnetConfig) error {
	path := filepath.Join(dir, stateIdentityFile)
	want := stateIdentity{
		Version:          config.CurrentVersion,
		Name:             cfg.Name,
		Hostname:         cfg.Hostname,
		AuthType:         cfg.Auth.Type,
		ClientSecretFile: cfg.Auth.ClientSecretFile,
		AuthKeyFile:      cfg.Auth.AuthKeyFile,
		Tags:             append([]string(nil), cfg.Auth.Tags...),
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var got stateIdentity
		if err := yaml.Unmarshal(data, &got); err != nil {
			return fmt.Errorf("%w: decode state identity: %v", ErrStateIdentityConflict, err)
		}
		if got.Version != want.Version || got.Name != want.Name || got.Hostname != want.Hostname || got.AuthType != want.AuthType ||
			got.ClientSecretFile != want.ClientSecretFile || got.AuthKeyFile != want.AuthKeyFile || !sameStrings(got.Tags, want.Tags) {
			return fmt.Errorf("%w; use reset or remove and add", ErrStateIdentityConflict)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("set state identity permissions: %w", err)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: read state identity: %v", ErrStateIdentityConflict, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("inspect verifier state directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("%w; state identity metadata is missing, use reset or remove and add", ErrStateIdentityConflict)
	}
	data, err = yaml.Marshal(want)
	if err != nil {
		return fmt.Errorf("encode state identity: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".multiderp-identity.*.tmp")
	if err != nil {
		return fmt.Errorf("create state identity: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set state identity permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write state identity: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync state identity: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state identity: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("persist state identity: %w", err)
	}
	return nil
}

func (m *VerifierManager) stopRecord(name string, record *managedVerifier) error {
	m.pool.Remove(name)
	m.mu.RLock()
	v := record.v
	m.mu.RUnlock()
	if v == nil {
		return nil
	}
	err := v.Close()
	m.mu.Lock()
	if record.v == v {
		record.v = nil
		if err != nil {
			record.lastError = err.Error()
		}
	}
	m.mu.Unlock()
	return err
}

func (m *VerifierManager) watch() {
	defer close(m.done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.syncEligibility(m.ctx)
			if m.applyMu.TryLock() {
				m.eligibilityMu.Lock()
				m.retryUnavailable(m.ctx)
				m.eligibilityMu.Unlock()
				m.applyMu.Unlock()
			}
		}
	}
}

func (m *VerifierManager) retryUnavailable(ctx context.Context) {
	now := time.Now()
	m.mu.RLock()
	retries := make([]struct {
		name   string
		record *managedVerifier
	}, 0)
	for name, record := range m.records {
		if record.cfg.Disabled || record.v != nil || record.nextRetry.IsZero() || now.Before(record.nextRetry) {
			continue
		}
		retries = append(retries, struct {
			name   string
			record *managedVerifier
		}{name: name, record: record})
	}
	m.mu.RUnlock()
	for _, retry := range retries {
		m.startRecord(ctx, retry.name, retry.record)
	}
}

func (m *VerifierManager) syncEligibility(ctx context.Context) {
	m.eligibilityMu.Lock()
	defer m.eligibilityMu.Unlock()
	m.syncEligibilityLocked(ctx)
}

func (m *VerifierManager) syncEligibilityLocked(ctx context.Context) {
	before := m.pool.Count()
	m.mu.RLock()
	records := make(map[string]*managedVerifier, len(m.records))
	for name, record := range m.records {
		records[name] = record
	}
	m.mu.RUnlock()
	for name, record := range records {
		m.mu.RLock()
		v := record.v
		disabled := record.cfg.Disabled
		m.mu.RUnlock()
		if disabled || v == nil {
			m.pool.Remove(name)
			continue
		}
		status := v.Status(ctx)
		if verifier.Eligible(status) {
			m.pool.Upsert(name, v)
		} else {
			m.pool.Remove(name)
		}
	}
	after := m.pool.Count()
	if before != after && m.onChange != nil {
		m.onChange(after)
	}
}

func (m *VerifierManager) EligibleCount() int {
	return m.pool.Count()
}

func (m *VerifierManager) Status(name string, ctx context.Context) (verifier.Status, error) {
	return m.status(name, ctx, false)
}

func (m *VerifierManager) StatusVerbose(name string, ctx context.Context) (verifier.Status, error) {
	return m.status(name, ctx, true)
}

func (m *VerifierManager) status(name string, ctx context.Context, verbose bool) (verifier.Status, error) {
	key := strings.ToLower(name)
	m.mu.RLock()
	record := m.records[key]
	if record == nil {
		m.mu.RUnlock()
		return verifier.Status{}, fmt.Errorf("verifier %q not found", name)
	}
	cfg := record.cfg
	v := record.v
	lastError := record.lastError
	m.mu.RUnlock()
	status := verifier.Status{
		Name:              cfg.Name,
		Authentication:    cfg.Auth.Type,
		State:             verifier.StateConfigured,
		Required:          cfg.Required,
		EffectiveRequired: configEffectiveRequired(cfg),
		Node:              cfg.Hostname,
		StateDirectory:    m.mustStateDir(cfg.Name),
	}
	if cfg.Disabled {
		status.State = verifier.StateDisabled
	} else if v != nil {
		if verbose {
			if verboseStatus, ok := v.(verifier.VerboseStatusProvider); ok {
				status = verboseStatus.VerboseStatus(ctx)
			} else {
				status = v.Status(ctx)
			}
		} else {
			status = v.Status(ctx)
		}
		status.Name = cfg.Name
		status.Authentication = cfg.Auth.Type
		status.Required = cfg.Required
		status.EffectiveRequired = configEffectiveRequired(cfg)
		status.StateDirectory = m.mustStateDir(cfg.Name)
	} else if lastError != "" {
		status.State = verifier.StateError
	}
	status.Admission = m.pool.Contains(key)
	if status.LastError == "" {
		status.LastError = lastError
	}
	return status, nil
}

func configEffectiveRequired(cfg config.TailnetConfig) bool {
	return verifier.EffectiveRequired(cfg.Required, cfg.Disabled)
}

func (m *VerifierManager) List(ctx context.Context) []verifier.Status {
	m.mu.RLock()
	desired := m.desired.Clone()
	m.mu.RUnlock()
	result := make([]verifier.Status, 0, len(desired.Tailnets))
	for _, cfg := range desired.Tailnets {
		status, err := m.Status(cfg.Name, ctx)
		if err == nil {
			result = append(result, status)
		}
	}
	return result
}

func (m *VerifierManager) CurrentConfig() config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.desired.Clone()
}

func (m *VerifierManager) Login(ctx context.Context, name string) (string, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.eligibilityMu.Lock()
	defer m.eligibilityMu.Unlock()
	m.mu.RLock()
	record := m.records[strings.ToLower(name)]
	var v verifier.Verifier
	if record != nil {
		v = record.v
	}
	m.mu.RUnlock()
	if v == nil {
		return "", fmt.Errorf("verifier %q is not running", name)
	}
	controller, ok := v.(verifier.LoginController)
	if !ok {
		return "", errors.New("verifier does not support interactive login")
	}
	m.pool.Remove(strings.ToLower(name))
	url, err := controller.Login(ctx)
	if err == nil {
		m.syncEligibilityLocked(ctx)
	}
	return url, err
}

func (m *VerifierManager) Logout(ctx context.Context, name string) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.eligibilityMu.Lock()
	defer m.eligibilityMu.Unlock()
	key := strings.ToLower(name)
	m.mu.RLock()
	record := m.records[key]
	var v verifier.Verifier
	if record != nil {
		v = record.v
	}
	m.mu.RUnlock()
	if record == nil {
		return fmt.Errorf("verifier %q not found", name)
	}
	if v == nil {
		return errors.New("verifier is not running")
	}
	m.pool.Remove(key)
	logout, ok := v.(verifier.LogoutController)
	if !ok {
		_ = v.Close()
		m.setStopped(record, "verifier does not support logout")
		return errors.New("verifier does not support logout")
	}
	logoutErr := logout.Logout(ctx)
	closeErr := v.Close()
	combinedErr := errors.Join(logoutErr, closeErr)
	message := ""
	if combinedErr != nil {
		message = combinedErr.Error()
	}
	m.setStopped(record, message)
	if logoutErr != nil {
		return fmt.Errorf("logout verifier: %w", logoutErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close verifier after logout: %w", closeErr)
	}
	m.mu.Lock()
	record.lastError = ""
	m.mu.Unlock()
	return nil
}

func (m *VerifierManager) setStopped(record *managedVerifier, message string) {
	m.mu.Lock()
	record.v = nil
	if message != "" && message != "<nil>" {
		record.lastError = message
	}
	record.nextRetry = time.Time{}
	m.mu.Unlock()
}

func (m *VerifierManager) Reset(ctx context.Context, name string) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.eligibilityMu.Lock()
	defer m.eligibilityMu.Unlock()
	key := strings.ToLower(name)
	m.mu.RLock()
	record := m.records[key]
	var v verifier.Verifier
	if record != nil {
		v = record.v
	}
	m.mu.RUnlock()
	if record == nil {
		return fmt.Errorf("verifier %q not found", name)
	}
	if v == nil {
		return errors.New("reset requires a running verifier so it can be logged out before state removal")
	}
	m.pool.Remove(key)
	logout, ok := v.(verifier.LogoutController)
	if !ok {
		_ = v.Close()
		m.setStopped(record, "verifier does not support logout")
		return errors.New("reset requires a verifier with a working logout operation")
	}
	if err := logout.Logout(ctx); err != nil {
		_ = v.Close()
		m.setStopped(record, err.Error())
		return fmt.Errorf("reset aborted because logout failed: %w", err)
	}
	if err := v.Close(); err != nil {
		m.setStopped(record, err.Error())
		return fmt.Errorf("reset could not close verifier: %w", err)
	}
	m.setStopped(record, "")
	stateDir, err := m.stateDir(record.cfg.Name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(stateDir); err != nil {
		return fmt.Errorf("remove verifier state: %w", err)
	}
	m.startRecord(ctx, key, record)
	m.syncEligibilityLocked(ctx)
	return nil
}

type OrphanInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	State     string    `json:"state"`
}

func (m *VerifierManager) Orphan(name string) (OrphanInfo, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.eligibilityMu.Lock()
	defer m.eligibilityMu.Unlock()
	info, err := m.prepareOrphanLocked(name)
	if err != nil {
		return OrphanInfo{}, err
	}
	return m.orphanWithInfoLocked(name, info)
}

func (m *VerifierManager) PrepareOrphan(name string) (OrphanInfo, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.eligibilityMu.Lock()
	defer m.eligibilityMu.Unlock()
	return m.prepareOrphanLocked(name)
}

func (m *VerifierManager) OrphanWithInfo(name string, info OrphanInfo) (OrphanInfo, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.eligibilityMu.Lock()
	defer m.eligibilityMu.Unlock()
	return m.orphanWithInfoLocked(name, info)
}

func (m *VerifierManager) prepareOrphanLocked(name string) (OrphanInfo, error) {
	key := strings.ToLower(name)
	m.mu.RLock()
	record := m.records[key]
	if record != nil {
		name = record.cfg.Name
	}
	m.mu.RUnlock()
	if record == nil {
		return OrphanInfo{}, fmt.Errorf("verifier %q not found", name)
	}
	root := m.mustOrphanRoot()
	if strings.TrimSpace(root) == "" {
		return OrphanInfo{}, errors.New("orphan state root is not configured")
	}
	id, err := config.NewOrphanID()
	if err != nil {
		return OrphanInfo{}, err
	}
	orphanDir := filepath.Join(root, id)
	if !config.IsWithin(root, orphanDir) {
		return OrphanInfo{}, errors.New("orphan path escaped configured root")
	}
	return OrphanInfo{ID: id, Name: record.cfg.Name, CreatedAt: time.Now().UTC()}, nil
}

func (m *VerifierManager) orphanWithInfoLocked(name string, info OrphanInfo) (OrphanInfo, error) {
	key := strings.ToLower(name)
	m.mu.RLock()
	record := m.records[key]
	if record != nil {
		name = record.cfg.Name
	}
	m.mu.RUnlock()
	if record == nil {
		return OrphanInfo{}, fmt.Errorf("verifier %q not found", name)
	}
	if !orphanIDPattern.MatchString(info.ID) {
		return OrphanInfo{}, errors.New("invalid orphan id")
	}
	if info.Name != record.cfg.Name || info.CreatedAt.IsZero() {
		return OrphanInfo{}, errors.New("orphan metadata does not match verifier")
	}
	root := m.mustOrphanRoot()
	if strings.TrimSpace(root) == "" {
		return OrphanInfo{}, errors.New("orphan state root is not configured")
	}
	stateDir, err := m.stateDir(record.cfg.Name)
	if err != nil {
		return OrphanInfo{}, err
	}
	orphanDir := filepath.Join(root, info.ID)
	if !config.IsWithin(root, orphanDir) {
		return OrphanInfo{}, errors.New("orphan path escaped configured root")
	}
	m.pool.Remove(key)
	if err := m.stopRecord(key, record); err != nil {
		return OrphanInfo{}, fmt.Errorf("stop verifier before remove: %w", err)
	}
	if err := ensurePrivateDir(root); err != nil {
		return m.orphanFailure(record, fmt.Errorf("create orphan root: %w", err))
	}
	state := "empty"
	if _, err := os.Stat(stateDir); err == nil {
		if err := os.Rename(stateDir, orphanDir); err != nil {
			return m.orphanFailure(record, fmt.Errorf("move verifier state to orphan: %w", err))
		}
		state = "preserved"
	} else if !errors.Is(err, os.ErrNotExist) {
		return m.orphanFailure(record, fmt.Errorf("inspect verifier state: %w", err))
	} else if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		return m.orphanFailure(record, fmt.Errorf("create empty orphan: %w", err))
	}
	metadata := config.OrphanMetadata{ID: info.ID, Name: info.Name, CreatedAt: info.CreatedAt}
	if err := config.WriteOrphanMetadata(orphanDir, metadata); err != nil {
		return m.orphanFailure(record, fmt.Errorf("write orphan metadata: %w", err))
	}
	m.mu.Lock()
	delete(m.records, key)
	m.mu.Unlock()
	return OrphanInfo{ID: metadata.ID, Name: metadata.Name, CreatedAt: metadata.CreatedAt, State: state}, nil
}

func (m *VerifierManager) orphanFailure(record *managedVerifier, err error) (OrphanInfo, error) {
	m.mu.Lock()
	if record.v == nil {
		record.lastError = err.Error()
		record.nextRetry = time.Time{}
	}
	m.mu.Unlock()
	return OrphanInfo{}, err
}

var orphanIDPattern = regexp.MustCompile(`^orphan-[0-9a-f]{32}$`)

func (m *VerifierManager) ListOrphans() ([]OrphanInfo, error) {
	root := m.mustOrphanRoot()
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("orphan state root is not configured")
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []OrphanInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]OrphanInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !orphanIDPattern.MatchString(entry.Name()) {
			continue
		}
		metadata, err := config.ReadOrphanMetadata(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read orphan %q metadata: %w", entry.Name(), err)
		}
		if metadata.ID != entry.Name() || metadata.Name == "" || metadata.CreatedAt.IsZero() {
			return nil, fmt.Errorf("orphan %q metadata is invalid", entry.Name())
		}
		state := "empty"
		children, readErr := os.ReadDir(filepath.Join(root, entry.Name()))
		if readErr == nil && len(children) > 1 {
			state = "preserved"
		}
		result = append(result, OrphanInfo{ID: metadata.ID, Name: metadata.Name, CreatedAt: metadata.CreatedAt, State: state})
	}
	return result, nil
}

func (m *VerifierManager) PurgeOrphan(id string) error {
	if !orphanIDPattern.MatchString(id) {
		return errors.New("invalid orphan id")
	}
	root := m.mustOrphanRoot()
	if strings.TrimSpace(root) == "" {
		return errors.New("orphan state root is not configured")
	}
	dir := filepath.Join(root, id)
	if !config.IsWithin(root, dir) {
		return errors.New("orphan path escaped configured root")
	}
	metadata, err := config.ReadOrphanMetadata(dir)
	if err != nil {
		return fmt.Errorf("read orphan metadata: %w", err)
	}
	if metadata.ID != id {
		return errors.New("orphan metadata id does not match requested id")
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("purge orphan state %q: %w", id, err)
	}
	return nil
}

func (m *VerifierManager) stateDir(name string) (string, error) {
	m.mu.RLock()
	root := m.desired.Storage.TailnetStateDir
	m.mu.RUnlock()
	if root == "" {
		return "", errors.New("tailnet state root is not configured")
	}
	path := filepath.Join(root, name)
	if !config.IsWithin(root, path) {
		return "", errors.New("verifier state path escaped configured root")
	}
	return path, nil
}

func (m *VerifierManager) mustStateDir(name string) string {
	path, err := m.stateDir(name)
	if err != nil {
		return ""
	}
	return path
}

func (m *VerifierManager) mustOrphanRoot() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.desired.Storage.OrphanStateDir
}

func ensurePrivateDir(path string) error {
	if path == "" {
		return errors.New("directory path is empty")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if filepath.Clean(path) == "." {
		return nil
	}
	return os.Chmod(path, 0o700)
}

func (m *VerifierManager) Close() error {
	m.cancel()
	<-m.done
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.eligibilityMu.Lock()
	defer m.eligibilityMu.Unlock()
	m.pool.Clear()
	m.mu.RLock()
	records := make([]*managedVerifier, 0, len(m.records))
	for _, record := range m.records {
		records = append(records, record)
	}
	m.mu.RUnlock()
	var errs []error
	for _, record := range records {
		if err := m.stopRecord(record.cfg.Name, record); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isManagerClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
