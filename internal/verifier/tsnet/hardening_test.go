package tsnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"multiderp/internal/config"
	"multiderp/internal/verifier"
	"tailscale.com/drive"
	"tailscale.com/ipn"
	"tailscale.com/types/opt"
)

func TestBaselinePrefsSatisfyVerifierInvariant(t *testing.T) {
	baseline := BaselinePrefs()
	if baseline == nil {
		t.Fatal("BaselinePrefs() returned nil")
	}
	if err := ValidateVerifierPrefs(&baseline.Prefs); err != nil {
		t.Fatalf("ValidateVerifierPrefs(baseline) error = %v", err)
	}
	for name, enabled := range map[string]bool{
		"RouteAllSet":             baseline.RouteAllSet,
		"RemoteConfigSet":         baseline.RemoteConfigSet,
		"ShieldsUpSet":            baseline.ShieldsUpSet,
		"AdvertiseRoutesSet":      baseline.AdvertiseRoutesSet,
		"AdvertiseServicesSet":    baseline.AdvertiseServicesSet,
		"RunSSHSet":               baseline.RunSSHSet,
		"RunWebClientSet":         baseline.RunWebClientSet,
		"AppConnectorSet":         baseline.AppConnectorSet,
		"DriveSharesSet":          baseline.DriveSharesSet,
		"RelayServerPortSet":      baseline.RelayServerPortSet,
		"AutoUpdateApplySet":      baseline.AutoUpdateSet.ApplySet,
		"AutoUpdateCheckSet":      baseline.AutoUpdateSet.CheckSet,
		"PostureCheckingSet":      baseline.PostureCheckingSet,
		"RelayStaticEndpointsSet": baseline.RelayServerStaticEndpointsSet,
	} {
		if !enabled {
			t.Errorf("baseline mask %s is not set", name)
		}
	}
}

func TestValidateVerifierPrefsRejectsUnsafeValues(t *testing.T) {
	tests := map[string]func(*ipn.Prefs){
		"shields up":         func(p *ipn.Prefs) { p.ShieldsUp = false },
		"remote config":      func(p *ipn.Prefs) { p.RemoteConfig = true },
		"route all":          func(p *ipn.Prefs) { p.RouteAll = true },
		"run ssh":            func(p *ipn.Prefs) { p.RunSSH = true },
		"run web":            func(p *ipn.Prefs) { p.RunWebClient = true },
		"app connector":      func(p *ipn.Prefs) { p.AppConnector.Advertise = true },
		"posture checking":   func(p *ipn.Prefs) { p.PostureChecking = true },
		"auto update":        func(p *ipn.Prefs) { p.AutoUpdate.Apply = opt.NewBool(true) },
		"drive shares":       func(p *ipn.Prefs) { p.DriveShares = []*drive.Share{{Name: "docs"}} },
		"logged out":         func(p *ipn.Prefs) { p.LoggedOut = true },
		"not wanted running": func(p *ipn.Prefs) { p.WantRunning = false },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			prefs := BaselinePrefs().Prefs
			mutate(&prefs)
			if err := ValidateVerifierPrefs(&prefs); err == nil {
				t.Fatal("ValidateVerifierPrefs() accepted unsafe prefs")
			} else if strings.TrimSpace(err.Error()) == "" {
				t.Fatal("ValidateVerifierPrefs() returned an empty error")
			}
		})
	}
}

func TestServeConfigEmpty(t *testing.T) {
	if !serveConfigEmpty(nil) {
		t.Fatal("nil serve config is not empty")
	}
	if serveConfigEmpty(&ipn.ServeConfig{TCP: map[uint16]*ipn.TCPPortHandler{443: nil}}) {
		t.Fatal("serve config with TCP entry is empty")
	}
}

func TestFactoryDoesNotReconsumeAuthKeyForExistingState(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "tailscaled.state")
	if err := os.WriteFile(statePath, []byte("existing state"), 0o600); err != nil {
		t.Fatalf("write existing state: %v", err)
	}
	v, err := (Factory{}).New(context.Background(), config.TailnetConfig{
		Name: "lab",
		Auth: config.AuthConfig{Type: "auth_key", AuthKeyFile: filepath.Join(stateDir, "missing-auth-key")},
	}, stateDir, nil)
	if err != nil {
		t.Fatalf("Factory.New() with existing state error = %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	concrete, ok := v.(*TSNetVerifier)
	if !ok {
		t.Fatalf("Factory.New() type = %T, want *TSNetVerifier", v)
	}
	if concrete.server.AuthKey != "" {
		t.Fatal("Factory.New() loaded an auth key despite existing enrollment state")
	}
}

func TestFactoryRequiresAuthKeyWithoutExistingState(t *testing.T) {
	stateDir := t.TempDir()
	_, err := (Factory{}).New(context.Background(), config.TailnetConfig{
		Name: "lab",
		Auth: config.AuthConfig{Type: "auth_key", AuthKeyFile: filepath.Join(stateDir, "missing-auth-key")},
	}, stateDir, nil)
	if err == nil || !strings.Contains(err.Error(), "authentication secret metadata") {
		t.Fatalf("Factory.New() error = %v, want missing-auth-key error", err)
	}
}

func TestHardeningCompatibilityErrorsDoNotBecomeTransientRetries(t *testing.T) {
	compatibility := &CompatibilityError{Operation: "GetServeConfig", Err: errors.New("404 Not Found")}
	if !errors.Is(compatibility, ErrHardeningCompatibility) {
		t.Fatal("CompatibilityError does not expose ErrHardeningCompatibility")
	}
	v := &TSNetVerifier{logf: func(string, ...any) {}}
	if err := v.failureForHardening(verifier.StateDegraded, compatibility); !errors.Is(err, ErrHardeningCompatibility) {
		t.Fatalf("failureForHardening() error = %v, want compatibility error", err)
	}
	if got := v.State(); got != verifier.StateError {
		t.Fatalf("verifier state after compatibility error = %s, want Error", got)
	}
	v.mu.RLock()
	retryAt := v.retryAt
	v.mu.RUnlock()
	if !retryAt.IsZero() {
		t.Fatalf("compatibility error scheduled retry at %v", retryAt)
	}
}

func TestWrapCompatibilityErrorOnlyClassifiesUnsupportedResponses(t *testing.T) {
	if err := wrapCompatibilityError("GetServeConfig", errors.New("404 Not Found: missing endpoint"), "read serve"); !errors.Is(err, ErrHardeningCompatibility) {
		t.Fatalf("unsupported response error = %v, want compatibility classification", err)
	}
	if err := wrapCompatibilityError("GetServeConfig", context.DeadlineExceeded, "read serve"); errors.Is(err, ErrHardeningCompatibility) {
		t.Fatalf("deadline error = %v, should remain transient", err)
	}
}
