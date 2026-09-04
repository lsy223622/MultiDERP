package tsnet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsy223622/MultiDERP/internal/config"
	"github.com/lsy223622/MultiDERP/internal/verifier"
	"tailscale.com/client/local"
	"tailscale.com/drive"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
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
	if concrete.pollInterval != 2*time.Second {
		t.Fatalf("default lifecycle poll interval = %v, want %v", concrete.pollInterval, 2*time.Second)
	}
	if concrete.hardeningInterval != DefaultHardeningValidationInterval {
		t.Fatalf("default hardening interval = %v, want %v", concrete.hardeningInterval, DefaultHardeningValidationInterval)
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

func TestFactoryPassesAuthKeyTagsToTSNet(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "tailscaled.state"), []byte("existing state"), 0o600); err != nil {
		t.Fatalf("write existing state: %v", err)
	}
	v, err := (Factory{}).New(context.Background(), config.TailnetConfig{
		Name: "lab",
		Auth: config.AuthConfig{Type: "auth_key", AuthKeyFile: filepath.Join(stateDir, "missing-auth-key"), Tags: []string{"tag:one", "tag:two"}},
	}, stateDir, nil)
	if err != nil {
		t.Fatalf("Factory.New() error = %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	concrete := v.(*TSNetVerifier)
	if want := []string{"tag:one", "tag:two"}; !reflect.DeepEqual(concrete.server.AdvertiseTags, want) {
		t.Fatalf("AdvertiseTags = %#v, want %#v", concrete.server.AdvertiseTags, want)
	}
}

type hardeningRoundTripper struct {
	mu       sync.Mutex
	requests []string
	respond  func(method, path string) (int, string)
}

func (r *hardeningRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	status, body := r.respond(req.Method, req.URL.Path)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func TestPeriodicHardeningValidationReadsOnlyOnHealthyPath(t *testing.T) {
	prefsJSON, err := json.Marshal(BaselinePrefs().Prefs)
	if err != nil {
		t.Fatalf("marshal baseline prefs: %v", err)
	}
	transport := &hardeningRoundTripper{respond: func(method, path string) (int, string) {
		if method != http.MethodGet {
			return http.StatusMethodNotAllowed, "unexpected write"
		}
		switch path {
		case "/localapi/v0/prefs":
			return http.StatusOK, string(prefsJSON)
		case "/localapi/v0/serve-config":
			return http.StatusOK, "{}"
		default:
			return http.StatusNotFound, "unexpected path"
		}
	}}
	v := &TSNetVerifier{lc: &local.Client{Transport: transport, OmitAuth: true}, state: verifier.StateConnected, hardening: true, logf: func(string, ...any) {}}
	if err := v.validateHardening(context.Background()); err != nil {
		t.Fatalf("validateHardening() error = %v", err)
	}
	transport.mu.Lock()
	got := append([]string(nil), transport.requests...)
	transport.mu.Unlock()
	want := []string{"GET /localapi/v0/prefs", "GET /localapi/v0/serve-config"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("healthy validation requests = %#v, want %#v", got, want)
	}
}

func TestLoginCompletionAppliesHardeningImmediately(t *testing.T) {
	prefsJSON, err := json.Marshal(BaselinePrefs().Prefs)
	if err != nil {
		t.Fatalf("marshal baseline prefs: %v", err)
	}
	statusJSON, err := json.Marshal(ipnstate.Status{BackendState: ipn.Running.String(), HaveNodeKey: true})
	if err != nil {
		t.Fatalf("marshal running status: %v", err)
	}
	transport := &hardeningRoundTripper{respond: func(method, path string) (int, string) {
		switch method + " " + path {
		case "POST /localapi/v0/login-interactive":
			return http.StatusNoContent, ""
		case "GET /localapi/v0/status":
			return http.StatusOK, string(statusJSON)
		case "PATCH /localapi/v0/prefs":
			return http.StatusOK, string(prefsJSON)
		case "GET /localapi/v0/prefs":
			return http.StatusOK, string(prefsJSON)
		case "POST /localapi/v0/serve-config":
			return http.StatusOK, "{}"
		case "GET /localapi/v0/serve-config":
			return http.StatusOK, "{}"
		default:
			return http.StatusNotFound, "unexpected path"
		}
	}}
	v := &TSNetVerifier{
		authType: "web",
		lc:       &local.Client{Transport: transport, OmitAuth: true},
		logf:     func(string, ...any) {},
	}
	if _, err := v.Login(context.Background()); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if v.State() != verifier.StateConnected || !v.statusSnapshot(false).HardeningVerified {
		t.Fatalf("state after completed login = %s, status = %#v, want connected and hardened", v.State(), v.statusSnapshot(false))
	}
	transport.mu.Lock()
	got := append([]string(nil), transport.requests...)
	transport.mu.Unlock()
	want := []string{
		"POST /localapi/v0/login-interactive",
		"GET /localapi/v0/status",
		"GET /localapi/v0/status",
		"PATCH /localapi/v0/prefs",
		"GET /localapi/v0/prefs",
		"POST /localapi/v0/serve-config",
		"GET /localapi/v0/serve-config",
		"GET /localapi/v0/status",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("completed-login requests = %#v, want immediate hardening requests %#v", got, want)
	}
}

func TestPeriodicHardeningDriftFailsClosedBeforeRepair(t *testing.T) {
	prefsJSON, err := json.Marshal(BaselinePrefs().Prefs)
	if err != nil {
		t.Fatalf("marshal baseline prefs: %v", err)
	}
	unsafe := BaselinePrefs().Prefs
	unsafe.RouteAll = true
	unsafeJSON, err := json.Marshal(unsafe)
	if err != nil {
		t.Fatalf("marshal unsafe prefs: %v", err)
	}
	transport := &hardeningRoundTripper{}
	transport.respond = func(method, path string) (int, string) {
		switch method + " " + path {
		case "GET /localapi/v0/prefs":
			transport.mu.Lock()
			count := len(transport.requests)
			transport.mu.Unlock()
			if count == 1 {
				return http.StatusOK, string(unsafeJSON)
			}
			return http.StatusOK, string(prefsJSON)
		case "PATCH /localapi/v0/prefs":
			return http.StatusOK, string(prefsJSON)
		case "POST /localapi/v0/serve-config":
			return http.StatusOK, "{}"
		case "GET /localapi/v0/serve-config":
			return http.StatusOK, "{}"
		case "GET /localapi/v0/status":
			status, marshalErr := json.Marshal(ipnstate.Status{BackendState: ipn.Running.String(), HaveNodeKey: true})
			if marshalErr != nil {
				return http.StatusInternalServerError, marshalErr.Error()
			}
			return http.StatusOK, string(status)
		default:
			return http.StatusNotFound, "unexpected path"
		}
	}
	callbackAt := -1
	v := &TSNetVerifier{lc: &local.Client{Transport: transport, OmitAuth: true}, state: verifier.StateConnected, hardening: true, logf: func(string, ...any) {}}
	v.onIneligible = func() {
		transport.mu.Lock()
		callbackAt = len(transport.requests)
		transport.mu.Unlock()
	}
	if err := v.validateHardening(context.Background()); err != nil {
		t.Fatalf("validateHardening() error = %v", err)
	}
	if v.State() != verifier.StateConnected || !v.statusSnapshot(false).HardeningVerified {
		t.Fatalf("state after repair = %s, status = %#v, want connected and hardened", v.State(), v.statusSnapshot(false))
	}
	transport.mu.Lock()
	got := append([]string(nil), transport.requests...)
	transport.mu.Unlock()
	if callbackAt != 1 {
		t.Fatalf("ineligible callback request index = %d, want 1", callbackAt)
	}
	if len(got) != 6 || got[0] != "GET /localapi/v0/prefs" || got[1] != "PATCH /localapi/v0/prefs" {
		t.Fatalf("drift repair requests = %#v, want read followed by repair writes", got)
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
