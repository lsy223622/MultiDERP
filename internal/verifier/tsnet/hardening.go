package tsnet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/types/opt"
)

var ErrHardeningCompatibility = errors.New("hardening compatibility error")

type CompatibilityError struct {
	Operation string
	Err       error
}

func (e *CompatibilityError) Error() string {
	if e == nil {
		return ErrHardeningCompatibility.Error()
	}
	return fmt.Sprintf("%s: %s: %v", ErrHardeningCompatibility, e.Operation, e.Err)
}

func (e *CompatibilityError) Unwrap() error {
	if e == nil {
		return ErrHardeningCompatibility
	}
	return errors.Join(ErrHardeningCompatibility, e.Err)
}

func BaselinePrefs() *ipn.MaskedPrefs {
	return &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			RouteAll:                   false,
			ExitNodeID:                 "",
			ExitNodeIP:                 netip.Addr{},
			AutoExitNode:               "",
			ExitNodeAllowLANAccess:     false,
			RunSSH:                     false,
			RunWebClient:               false,
			WantRunning:                true,
			LoggedOut:                  false,
			ShieldsUp:                  true,
			AdvertiseRoutes:            nil,
			AdvertiseServices:          []string{},
			AutoUpdate:                 ipn.AutoUpdatePrefs{Check: false, Apply: opt.NewBool(false)},
			AppConnector:               ipn.AppConnectorPrefs{Advertise: false},
			PostureChecking:            false,
			RemoteConfig:               false,
			DriveShares:                nil,
			RelayServerPort:            nil,
			RelayServerStaticEndpoints: nil,
		},
		RouteAllSet:                   true,
		ExitNodeIDSet:                 true,
		ExitNodeIPSet:                 true,
		AutoExitNodeSet:               true,
		ExitNodeAllowLANAccessSet:     true,
		RunSSHSet:                     true,
		RunWebClientSet:               true,
		WantRunningSet:                true,
		LoggedOutSet:                  true,
		ShieldsUpSet:                  true,
		AdvertiseRoutesSet:            true,
		AdvertiseServicesSet:          true,
		AutoUpdateSet:                 ipn.AutoUpdatePrefsMask{CheckSet: true, ApplySet: true},
		AppConnectorSet:               true,
		PostureCheckingSet:            true,
		RemoteConfigSet:               true,
		DriveSharesSet:                true,
		RelayServerPortSet:            true,
		RelayServerStaticEndpointsSet: true,
	}
}

func ValidateVerifierPrefs(p *ipn.Prefs) error {
	if p == nil {
		return errors.New("prefs are nil")
	}
	if !p.ShieldsUp {
		return errors.New("ShieldsUp is not enabled")
	}
	if p.RemoteConfig {
		return errors.New("RemoteConfig is enabled")
	}
	if p.RouteAll {
		return errors.New("RouteAll is enabled")
	}
	if !p.ExitNodeID.IsZero() {
		return errors.New("ExitNodeID is set")
	}
	if p.ExitNodeIP.IsValid() {
		return errors.New("ExitNodeIP is set")
	}
	if p.AutoExitNode != "" {
		return errors.New("AutoExitNode is set")
	}
	if p.ExitNodeAllowLANAccess {
		return errors.New("ExitNodeAllowLANAccess is enabled")
	}
	if len(p.AdvertiseRoutes) != 0 {
		return errors.New("AdvertiseRoutes is not empty")
	}
	if len(p.AdvertiseServices) != 0 {
		return errors.New("AdvertiseServices is not empty")
	}
	if p.RunSSH {
		return errors.New("RunSSH is enabled")
	}
	if p.RunWebClient {
		return errors.New("RunWebClient is enabled")
	}
	if p.AppConnector.Advertise {
		return errors.New("AppConnector is enabled")
	}
	if p.PostureChecking {
		return errors.New("PostureChecking is enabled")
	}
	if p.AutoUpdate.Check {
		return errors.New("AutoUpdate checks are enabled")
	}
	if apply, ok := p.AutoUpdate.Apply.Get(); ok && apply {
		return errors.New("AutoUpdate apply is enabled")
	}
	if p.LoggedOut {
		return errors.New("LoggedOut is enabled")
	}
	if !p.WantRunning {
		return errors.New("WantRunning is disabled")
	}
	if p.DriveShares != nil && len(p.DriveShares) != 0 {
		return errors.New("DriveShares is not empty")
	}
	if p.RelayServerPort != nil {
		return errors.New("RelayServerPort is enabled")
	}
	if len(p.RelayServerStaticEndpoints) != 0 {
		return errors.New("RelayServerStaticEndpoints is not empty")
	}
	return nil
}

func ApplyAndValidate(ctx context.Context, lc *local.Client) error {
	if lc == nil {
		return errors.New("local client is nil")
	}
	if _, err := lc.EditPrefs(ctx, BaselinePrefs()); err != nil {
		return wrapCompatibilityError("EditPrefs", err, "apply verifier prefs")
	}
	prefs, err := lc.GetPrefs(ctx)
	if err != nil {
		return wrapCompatibilityError("GetPrefs", err, "read verifier prefs")
	}
	if err := ValidateVerifierPrefs(prefs); err != nil {
		return fmt.Errorf("verify verifier prefs: %w", err)
	}

	if err := lc.SetServeConfig(ctx, nil); err != nil {
		return wrapCompatibilityError("SetServeConfig", err, "clear serve and funnel config")
	}
	serve, err := lc.GetServeConfig(ctx)
	if err != nil {
		return wrapCompatibilityError("GetServeConfig", err, "read serve and funnel config")
	}
	if !serveConfigEmpty(serve) {
		return errors.New("serve and funnel config is not empty after clearing")
	}
	// The drive/shares LocalAPI is gated by the drive:share node capability.
	// The baseline clears DriveShares through EditPrefs above and verifies the
	// resulting prefs, so a verifier never needs permission to share files.
	return nil
}

func wrapCompatibilityError(operation string, err error, message string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrHardeningCompatibility) {
		return &CompatibilityError{Operation: operation, Err: fmt.Errorf("%s: %w", message, err)}
	}
	lower := strings.ToLower(err.Error())
	unsupported := strings.HasPrefix(lower, "404 ") || strings.HasPrefix(lower, "405 ") || strings.HasPrefix(lower, "501 ") || strings.Contains(lower, "not implemented") || strings.Contains(lower, "unsupported")
	if unsupported {
		return &CompatibilityError{Operation: operation, Err: fmt.Errorf("%s: %w", message, err)}
	}
	return fmt.Errorf("%s: %w", message, err)
}

func serveConfigEmpty(config *ipn.ServeConfig) bool {
	return config == nil || (len(config.TCP) == 0 && len(config.Web) == 0 && len(config.Services) == 0 && len(config.AllowFunnel) == 0 && len(config.Foreground) == 0)
}
