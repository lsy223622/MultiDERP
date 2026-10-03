package control

import (
	"net"
	"net/http"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func (h *httpHandler) allowNodeAttempt(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, a := range h.nodeAttempts {
		if !now.Before(a.until) {
			delete(h.nodeAttempts, k)
		}
	}
	a := h.nodeAttempts[ip]
	if a.count >= 30 || (a.count == 0 && len(h.nodeAttempts) >= 1024) {
		return false
	}
	if a.count == 0 {
		a.until = now.Add(time.Minute)
	}
	a.count++
	h.nodeAttempts[ip] = a
	return true
}

func (h *httpHandler) mountNodes() {
	s := h.store
	h.mux.HandleFunc("POST /api/v1/nodes/{id}/recover", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			InstanceID string `json:"instance_id"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if err := s.RecoverNodeInstance(r.Context(), h.actor(r), r.PathValue("id"), body.InstanceID); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.ListNodes(r.Context(), h.actor(r))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, items)
	})
	h.mux.HandleFunc("GET /api/v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Node(r.Context(), h.actor(r), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, n)
	})
	h.mux.HandleFunc("POST /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DisplayName string `json:"display_name"`
			Domain      string `json:"domain"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		n, e, err := s.CreateNode(r.Context(), h.actor(r), body.DisplayName, body.Domain)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, struct {
			Node       Node       `json:"node"`
			Enrollment Enrollment `json:"enrollment"`
		}{n, e})
	})
	h.mux.HandleFunc("POST /api/v1/nodes/{id}/enrollment", func(w http.ResponseWriter, r *http.Request) {
		e, err := s.IssueEnrollment(r.Context(), h.actor(r), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, e)
	})
	h.clusterMux.HandleFunc("POST /cluster/v1/enroll/challenge", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code       string `json:"code"`
			PublicKey  []byte `json:"public_key"`
			InstanceID string `json:"instance_id"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		c, err := s.EnrollmentChallenge(r.Context(), body.Code, body.PublicKey, body.InstanceID)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, c)
	})
	h.clusterMux.HandleFunc("POST /cluster/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		var body cluster.EnrollmentRequest
		if !decodeRequest(w, r, &body) {
			return
		}
		session, err := s.EnrollNode(r.Context(), body)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, session)
	})
	h.clusterMux.HandleFunc("POST /cluster/v1/session/challenge", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NodeID     string `json:"node_id"`
			InstanceID string `json:"instance_id"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		c, err := s.SessionChallenge(r.Context(), body.NodeID, body.InstanceID)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, c)
	})
	h.clusterMux.HandleFunc("POST /cluster/v1/session", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Challenge cluster.NodeChallenge `json:"challenge"`
			Signature []byte                `json:"signature"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		session, err := s.RenewNodeSession(r.Context(), body.Challenge, body.Signature)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, session)
	})
}
