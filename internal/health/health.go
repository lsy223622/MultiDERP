package health

import (
	"encoding/json"
	"net/http"
)

type Snapshot struct {
	Live              bool `json:"live"`
	Ready             bool `json:"ready"`
	Startup           bool `json:"startup"`
	DerperUsable      bool `json:"derper_usable"`
	EligibleVerifiers int  `json:"eligible_verifiers"`
	RequiredFailures  int  `json:"required_failures"`
	PendingRestart    bool `json:"pending_restart"`
}

type Provider func() Snapshot

type Server struct {
	Provider Provider
}

func NewServer(provider Provider) *Server {
	return &Server{Provider: provider}
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var snapshot Snapshot
		if s.Provider != nil {
			snapshot = s.Provider()
		}
		var ok bool
		switch r.URL.Path {
		case "/health/live":
			ok = snapshot.Live
		case "/health/ready":
			ok = snapshot.Ready
		case "/health/startup":
			ok = snapshot.Startup
		default:
			http.NotFound(w, r)
			return
		}
		if ok {
			write(w, http.StatusOK, snapshot)
		} else {
			write(w, http.StatusServiceUnavailable, snapshot)
		}
	})
}

func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
