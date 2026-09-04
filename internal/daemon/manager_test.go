package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lsy223622/MultiDERP/internal/admission"
	"github.com/lsy223622/MultiDERP/internal/config"
	"github.com/lsy223622/MultiDERP/internal/verifier"
	"tailscale.com/types/key"
)

type managerFakeVerifier struct {
	mu           sync.Mutex
	name         string
	tags         []string
	state        verifier.State
	hardened     bool
	onIneligible func()
	logoutErr    error
	closeErr     error
	closed       bool
}

func (f *managerFakeVerifier) Name() string { return f.name }

func (f *managerFakeVerifier) State() verifier.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *managerFakeVerifier) Start(context.Context) error {
	f.mu.Lock()
	f.state = verifier.StateConnected
	f.hardened = true
	f.mu.Unlock()
	return nil
}

func (f *managerFakeVerifier) ContainsNode(context.Context, key.NodePublic) (bool, error) {
	return false, nil
}

func (f *managerFakeVerifier) Status(context.Context) verifier.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return verifier.Status{Name: f.name, State: f.state, HardeningVerified: f.hardened}
}

func (f *managerFakeVerifier) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logoutErr != nil {
		return f.logoutErr
	}
	f.state = verifier.StateConfigured
	f.hardened = false
	return nil
}

func (f *managerFakeVerifier) Close() error {
	f.mu.Lock()
	f.closed = true
	f.state = verifier.StateStopping
	err := f.closeErr
	f.mu.Unlock()
	return err
}

func (f *managerFakeVerifier) SetIneligibleCallback(callback func()) {
	f.mu.Lock()
	f.onIneligible = callback
	f.mu.Unlock()
}

func (f *managerFakeVerifier) markIneligible() {
	f.mu.Lock()
	f.state = verifier.StateDegraded
	f.hardened = false
	callback := f.onIneligible
	f.mu.Unlock()
	if callback != nil {
		callback()
	}
}

type managerFakeFactory struct {
	mu         sync.Mutex
	verifiers  []*managerFakeVerifier
	nextLogout error
	nextClose  error
}

func (f *managerFakeFactory) New(_ context.Context, cfg config.TailnetConfig, _ string, _ func(string, ...any)) (verifier.Verifier, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := &managerFakeVerifier{
		name:      cfg.Name,
		tags:      append([]string(nil), cfg.Auth.Tags...),
		state:     verifier.StateConfigured,
		logoutErr: f.nextLogout,
		closeErr:  f.nextClose,
	}
	f.nextLogout = nil
	f.nextClose = nil
	f.verifiers = append(f.verifiers, v)
	return v, nil
}

func managerConfig(t *testing.T, name string) config.Config {
	t.Helper()
	cfg := config.Default()
	root := t.TempDir()
	cfg.Storage.StateDir = filepath.Join(root, "data")
	cfg.Storage.TailnetStateDir = filepath.Join(root, "tailnets")
	cfg.Storage.OrphanStateDir = filepath.Join(root, "orphans")
	cfg.Server.Hostname = "derp.example.com"
	cfg.Tailnets = []config.TailnetConfig{{Name: name, Auth: config.AuthConfig{Type: "web"}}}
	cfg.Normalize()
	return cfg
}

func newTestManager(t *testing.T, factory *managerFakeFactory) *VerifierManager {
	t.Helper()
	manager := NewVerifierManager(context.Background(), ManagerOptions{Pool: admission.NewPool(), Factory: factory})
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func TestManagerReconcileDisableAndEnableIsDenyFirst(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	cfg := managerConfig(t, "alice")
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	if got := manager.EligibleCount(); got != 1 {
		t.Fatalf("initial eligible count = %d, want 1", got)
	}

	disabled := cfg.Clone()
	disabled.Tailnets[0].Disabled = true
	if err := manager.Reconcile(context.Background(), disabled); err != nil {
		t.Fatalf("disable Reconcile() error = %v", err)
	}
	if got := manager.EligibleCount(); got != 0 {
		t.Fatalf("disabled eligible count = %d, want 0", got)
	}
	status, err := manager.Status("alice", context.Background())
	if err != nil {
		t.Fatalf("disabled Status() error = %v", err)
	}
	if status.State != verifier.StateDisabled || status.Admission || status.EffectiveRequired {
		t.Fatalf("disabled status = %#v", status)
	}

	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("enable Reconcile() error = %v", err)
	}
	if got := manager.EligibleCount(); got != 1 {
		t.Fatalf("enabled eligible count = %d, want 1", got)
	}
}

func TestManagerRemovesVerifierImmediatelyWhenHardeningIsLost(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	if err := manager.Reconcile(context.Background(), managerConfig(t, "alice")); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !manager.Pool().Contains("alice") {
		t.Fatal("verifier was not admitted after successful start")
	}
	factory.mu.Lock()
	verifier := factory.verifiers[0]
	factory.mu.Unlock()
	verifier.markIneligible()
	if manager.Pool().Contains("alice") {
		t.Fatal("verifier remained admitted after hardening was lost")
	}
}

func TestManagerPassesAdvertiseTagsToFactory(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	cfg := managerConfig(t, "alice")
	cfg.Tailnets[0].Auth.Tags = []string{"tag:first", "tag:second"}
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	factory.mu.Lock()
	got := append([]string(nil), factory.verifiers[0].tags...)
	factory.mu.Unlock()
	if want := []string{"tag:first", "tag:second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("factory advertise tags = %#v, want %#v", got, want)
	}
}

func TestManagerLogoutFailureStopsWithoutDeletingState(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	cfg := managerConfig(t, "alice")
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	stateDir := filepath.Join(cfg.Storage.TailnetStateDir, "alice")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	marker := filepath.Join(stateDir, "identity.marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write state marker: %v", err)
	}
	factory.mu.Lock()
	factory.verifiers[0].logoutErr = errors.New("logout unavailable")
	factory.mu.Unlock()

	if err := manager.Logout(context.Background(), "alice"); err == nil {
		t.Fatal("Logout() succeeded despite LocalAPI logout failure")
	}
	if got := manager.EligibleCount(); got != 0 {
		t.Fatalf("eligible count after failed logout = %d, want 0", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("state marker after failed logout: %v", err)
	}
	status, err := manager.Status("alice", context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != verifier.StateError {
		t.Fatalf("status after failed logout = %#v, want Error", status)
	}
}

func TestManagerResetRequiresLogoutAndRecreatesState(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	cfg := managerConfig(t, "alice")
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	stateDir := filepath.Join(cfg.Storage.TailnetStateDir, "alice")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	marker := filepath.Join(stateDir, "identity.marker")
	if err := os.WriteFile(marker, []byte("remove"), 0o600); err != nil {
		t.Fatalf("write state marker: %v", err)
	}
	if err := manager.Reset(context.Background(), "alice"); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state marker after reset: %v, want not-exist", err)
	}
	if got := manager.EligibleCount(); got != 1 {
		t.Fatalf("eligible count after reset = %d, want 1", got)
	}

	if err := manager.Logout(context.Background(), "alice"); err != nil {
		t.Fatalf("Logout() after reset error = %v", err)
	}
	if err := os.WriteFile(marker, []byte("must keep"), 0o600); err != nil {
		t.Fatalf("write marker after logout: %v", err)
	}
	if err := manager.Reset(context.Background(), "alice"); err == nil {
		t.Fatal("Reset() deleted state without a running verifier/logout")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("state marker after rejected reset: %v", err)
	}
}

func TestManagerOrphanAndPurge(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	cfg := managerConfig(t, "alice")
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	stateDir := filepath.Join(cfg.Storage.TailnetStateDir, "alice")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "identity.marker"), []byte("preserve"), 0o600); err != nil {
		t.Fatalf("write state marker: %v", err)
	}

	orphan, err := manager.Orphan("alice")
	if err != nil {
		t.Fatalf("Orphan() error = %v", err)
	}
	if orphan.State != "preserved" || manager.EligibleCount() != 0 {
		t.Fatalf("orphan = %#v, eligible count = %d", orphan, manager.EligibleCount())
	}
	items, err := manager.ListOrphans()
	if err != nil || len(items) != 1 || items[0].ID != orphan.ID || items[0].State != "preserved" {
		t.Fatalf("ListOrphans() = %#v, error = %v", items, err)
	}
	if err := manager.PurgeOrphan(orphan.ID); err != nil {
		t.Fatalf("PurgeOrphan() error = %v", err)
	}
	items, err = manager.ListOrphans()
	if err != nil || len(items) != 0 {
		t.Fatalf("ListOrphans() after purge = %#v, error = %v", items, err)
	}
}

func TestManagerRequiredStatusHonorsDisabled(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	cfg := managerConfig(t, "alice")
	cfg.Tailnets[0].Required = true
	cfg.Tailnets[0].Disabled = true
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	status, err := manager.Status("alice", context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Required || status.EffectiveRequired {
		t.Fatalf("required disabled status = %#v", status)
	}
}

func TestManagerOrphansRequireConfiguredRoot(t *testing.T) {
	manager := newTestManager(t, &managerFakeFactory{})
	if _, err := manager.ListOrphans(); err == nil {
		t.Fatal("ListOrphans() succeeded before a configured root was reconciled")
	}
	if err := manager.PurgeOrphan("orphan-00000000000000000000000000000000"); err == nil {
		t.Fatal("PurgeOrphan() succeeded before a configured root was reconciled")
	}
}

func TestManagerRejectsStateReuseAcrossIdentityChange(t *testing.T) {
	factory := &managerFakeFactory{}
	cfg := managerConfig(t, "alice")
	first := NewVerifierManager(context.Background(), ManagerOptions{Pool: admission.NewPool(), Factory: factory})
	if err := first.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close initial manager: %v", err)
	}

	changed := cfg.Clone()
	changed.Tailnets[0].Auth = config.AuthConfig{Type: "auth_key", AuthKeyFile: "/run/secrets/new-key"}
	second := newTestManager(t, factory)
	if err := second.Reconcile(context.Background(), changed); err != nil {
		t.Fatalf("identity-change Reconcile() error = %v", err)
	}
	if got := second.EligibleCount(); got != 0 {
		t.Fatalf("identity-change eligible count = %d, want 0", got)
	}
	status, err := second.Status("alice", context.Background())
	if err != nil {
		t.Fatalf("identity-change Status() error = %v", err)
	}
	if status.State != verifier.StateError || status.LastError == "" {
		t.Fatalf("identity-change status = %#v, want non-retryable Error", status)
	}
}

func TestManagerEnableDisableConcurrentWithAdmission(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	cfg := managerConfig(t, "alice")
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	controller := admission.NewController(manager.Pool(), admission.Limits{
		RequestTimeout:          time.Second,
		PerVerifierTimeout:      time.Second,
		MaxConcurrentAdmissions: 32,
		MaxConcurrentQueries:    4,
		MaxQueuedJobs:           64,
	})
	defer controller.Close()

	const rounds = 24
	start := make(chan struct{})
	errs := make(chan error, rounds*16)
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 4; j++ {
				allow, err := controller.Admit(context.Background(), key.NewNode().Public(), netip.MustParseAddr("192.0.2.10"))
				if err != nil {
					errs <- fmt.Errorf("admission %d/%d: %w", i, j, err)
				}
				if allow {
					errs <- fmt.Errorf("admission %d/%d unexpectedly allowed", i, j)
				}
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			desired := cfg.Clone()
			desired.Tailnets[0].Disabled = i%2 == 0
			if err := manager.Reconcile(context.Background(), desired); err != nil {
				errs <- fmt.Errorf("reconcile %d: %w", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent manager/admission operation: %v", err)
	}

	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("final enable Reconcile() error = %v", err)
	}
	if got := manager.EligibleCount(); got != 1 {
		t.Fatalf("final eligible count = %d, want 1", got)
	}
}

func TestManagerVerifierStateUpdatesConcurrentWithStatusQueries(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	cfg := managerConfig(t, "alice")
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	factory.mu.Lock()
	v := factory.verifiers[0]
	factory.mu.Unlock()

	const workers = 24
	start := make(chan struct{})
	errs := make(chan error, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 8; j++ {
				v.mu.Lock()
				v.state = verifier.StateConnected
				v.hardened = j%2 == 0
				v.mu.Unlock()
				manager.syncEligibility(context.Background())
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 8; j++ {
				if _, err := manager.Status("alice", context.Background()); err != nil {
					errs <- err
				}
				_ = manager.List(context.Background())
				_ = manager.EligibleCount()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent status query error: %v", err)
	}

	v.mu.Lock()
	v.state = verifier.StateConnected
	v.hardened = true
	v.mu.Unlock()
	manager.syncEligibility(context.Background())
	if got := manager.EligibleCount(); got != 1 {
		t.Fatalf("final eligible count = %d, want 1", got)
	}
}

func TestManagerReconcileRetriesCompatibilityErrorExplicitly(t *testing.T) {
	factory := &managerFakeFactory{}
	manager := newTestManager(t, factory)
	cfg := managerConfig(t, "alice")
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	factory.mu.Lock()
	failed := factory.verifiers[0]
	factory.mu.Unlock()
	failed.mu.Lock()
	failed.state = verifier.StateError
	failed.hardened = false
	failed.mu.Unlock()
	manager.syncEligibility(context.Background())
	if got := manager.EligibleCount(); got != 0 {
		t.Fatalf("eligible count after compatibility failure = %d, want 0", got)
	}
	if err := manager.Reconcile(context.Background(), cfg); err != nil {
		t.Fatalf("explicit retry Reconcile() error = %v", err)
	}
	if got := manager.EligibleCount(); got != 1 {
		t.Fatalf("eligible count after explicit retry = %d, want 1", got)
	}
	factory.mu.Lock()
	created := len(factory.verifiers)
	factory.mu.Unlock()
	if created != 2 {
		t.Fatalf("factory verifier count after explicit retry = %d, want 2", created)
	}
}
