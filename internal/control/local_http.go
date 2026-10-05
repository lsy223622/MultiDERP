package control

import (
	"context"
	"net/http"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

type LocalSettings struct {
	Role         string `json:"role"`
	Hostname     string `json:"hostname"`
	DERPListen   string `json:"derp_listen"`
	STUNListen   string `json:"stun_listen"`
	TLSMode      string `json:"tls_mode"`
	CertMode     string `json:"cert_mode"`
	DERPPort     int    `json:"derp_port"`
	STUNPort     int    `json:"stun_port"`
	MaxBudgetBPS uint64 `json:"max_budget_bps"`
	LoggingLevel string `json:"logging_level"`
}

type LocalStatus struct {
	Role               string                `json:"role"`
	Joined             bool                  `json:"joined"`
	ControllerURL      string                `json:"controller_url"`
	ClusterID          string                `json:"cluster_id"`
	NodeID             string                `json:"node_id"`
	Saved              LocalSettings         `json:"saved"`
	Active             LocalSettings         `json:"active"`
	PendingApply       bool                  `json:"pending_apply"`
	ApplyError         string                `json:"apply_error"`
	Control            cluster.ControlStatus `json:"control"`
	PolicyBudgetBPS    uint64                `json:"policy_budget_bps"`
	EffectiveBudgetBPS uint64                `json:"effective_budget_bps"`
	QoS                *cluster.QoSPolicy    `json:"qos,omitempty"`
}

type LocalBackend interface {
	Role() string
	Status(context.Context) (LocalStatus, error)
	SaveSettings(context.Context, LocalSettings) error
	ApplySettings(context.Context) error
	UploadCertificate(context.Context, string, string) error
	Join(context.Context, string, string) error
	Leave(context.Context) (bool, error)
	RegisterLocal(context.Context, Actor, string) (Node, error)
}

func NewLocalHTTPHandler(s *Store, backend LocalBackend) http.Handler {
	h := newHTTPHandler(s)
	h.local = backend
	h.mountLocal()
	return h
}

func (h *httpHandler) serverRole() string {
	if h.local != nil {
		return h.local.Role()
	}
	return "controller"
}

func (h *httpHandler) mountLocal() {
	admin := func(run http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if h.actor(r).Role != "admin" {
				httpError(w, ErrForbidden)
				return
			}
			run(w, r)
		}
	}
	finish := func(w http.ResponseWriter, err error) {
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
	h.mux.HandleFunc("GET /api/v1/local/status", admin(func(w http.ResponseWriter, r *http.Request) {
		status, err := h.local.Status(r.Context())
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, status)
	}))
	h.mux.HandleFunc("POST /api/v1/local/settings", admin(func(w http.ResponseWriter, r *http.Request) {
		var body LocalSettings
		if !decodeRequest(w, r, &body) {
			return
		}
		finish(w, h.local.SaveSettings(r.Context(), body))
	}))
	h.mux.HandleFunc("POST /api/v1/local/apply", admin(func(w http.ResponseWriter, r *http.Request) { finish(w, h.local.ApplySettings(r.Context())) }))
	h.mux.HandleFunc("POST /api/v1/local/certificate", admin(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Certificate string `json:"certificate"`
			PrivateKey  string `json:"private_key"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		finish(w, h.local.UploadCertificate(r.Context(), body.Certificate, body.PrivateKey))
	}))
	h.mux.HandleFunc("POST /api/v1/local/join", admin(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ControllerURL  string `json:"controller_url"`
			EnrollmentCode string `json:"enrollment_code"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		finish(w, h.local.Join(r.Context(), body.ControllerURL, body.EnrollmentCode))
	}))
	h.mux.HandleFunc("POST /api/v1/local/leave", admin(func(w http.ResponseWriter, r *http.Request) {
		released, err := h.local.Leave(r.Context())
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true, "released": released})
	}))
	h.mux.HandleFunc("POST /api/v1/local/register", admin(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DisplayName string `json:"display_name"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		n, err := h.local.RegisterLocal(r.Context(), h.actor(r), body.DisplayName)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, n)
	}))
}
