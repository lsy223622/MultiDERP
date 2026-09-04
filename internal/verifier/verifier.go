package verifier

import (
	"context"

	"github.com/lsy223622/MultiDERP/internal/config"
	"tailscale.com/types/key"
)

type State string

const (
	StateDisabled        State = "Disabled"
	StateConfigured      State = "Configured"
	StateStarting        State = "Starting"
	StateWaitingForLogin State = "WaitingForLogin"
	StateHardening       State = "Hardening"
	StateConnected       State = "Connected"
	StateDegraded        State = "Degraded"
	StateError           State = "Error"
	StateStopping        State = "Stopping"
)

func (s State) String() string {
	switch s {
	case StateWaitingForLogin:
		return "waiting-login"
	case StateConnected:
		return "connected"
	case StateDisabled:
		return "disabled"
	case StateConfigured:
		return "configured"
	case StateStarting:
		return "starting"
	case StateHardening:
		return "hardening"
	case StateDegraded:
		return "degraded"
	case StateError:
		return "error"
	case StateStopping:
		return "stopping"
	default:
		return string(s)
	}
}

type Verifier interface {
	Name() string
	State() State
	ContainsNode(context.Context, key.NodePublic) (bool, error)
	Status(context.Context) Status
	Close() error
}

type Starter interface {
	Start(context.Context) error
}

type LoginController interface {
	Login(context.Context) (string, error)
}

type LogoutController interface {
	Logout(context.Context) error
}

type IneligibleCallbackSetter interface {
	SetIneligibleCallback(func())
}

type Factory interface {
	New(context.Context, config.TailnetConfig, string, func(string, ...any)) (Verifier, error)
}

type Status struct {
	Name              string   `json:"name"`
	Authentication    string   `json:"authentication"`
	State             State    `json:"state"`
	HardeningVerified bool     `json:"hardening_verified"`
	Admission         bool     `json:"admission"`
	Required          bool     `json:"required"`
	EffectiveRequired bool     `json:"effective_required"`
	AuthURL           string   `json:"auth_url,omitempty"`
	Tailnet           string   `json:"tailnet,omitempty"`
	Node              string   `json:"node,omitempty"`
	NodeKey           string   `json:"node_key,omitempty"`
	TailscaleIPs      []string `json:"tailscale_ips,omitempty"`
	StateDirectory    string   `json:"state_directory"`
	LastError         string   `json:"last_error,omitempty"`
}

type VerboseStatusProvider interface {
	VerboseStatus(context.Context) Status
}

func Eligible(s Status) bool {
	return s.State == StateConnected && s.HardeningVerified
}

func EffectiveRequired(required, disabled bool) bool {
	return required && !disabled
}
