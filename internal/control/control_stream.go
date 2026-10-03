package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

type nodeControlStream struct {
	changed  chan struct{}
	done     chan struct{}
	response *http.ResponseController
}

func (s *nodeControlStream) stop() {
	close(s.done)
	s.response.SetWriteDeadline(time.Now())
}

func (s *Store) notifyPolicy(id string) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if stream := s.policyStreams[id]; stream != nil {
		select {
		case stream.changed <- struct{}{}:
		default:
		}
	}
}

func (s *Store) notifyPolicies() {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	for _, stream := range s.policyStreams {
		select {
		case stream.changed <- struct{}{}:
		default:
		}
	}
}

func nodeBearer(r *http.Request) string {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return value
}

func (s *Store) currentPolicy(ctx context.Context, id string) (cluster.Policy, error) {
	var body []byte
	if err := s.db.QueryRowContext(ctx, "SELECT policy_json FROM node_policies WHERE node_id=?", id).Scan(&body); err != nil {
		return cluster.Policy{}, err
	}
	return cluster.DecodePolicy(bytes.NewReader(body), s.clusterID, id)
}

func (h *httpHandler) mountNodeControl() {
	s := h.store
	h.clusterMux.HandleFunc("GET /cluster/v1/control", h.controlStream)
	h.clusterMux.HandleFunc("POST /cluster/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var body cluster.NodeHeartbeatRequest
		r.Body = http.MaxBytesReader(w, r.Body, cluster.MaxPolicyBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || dec.Decode(new(any)) != io.EOF {
			httpError(w, ErrInvalid)
			return
		}
		state, err := s.NodeHeartbeat(r.Context(), nodeBearer(r), body.Report)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, state)
	})
	h.clusterMux.HandleFunc("POST /cluster/v1/ack", func(w http.ResponseWriter, r *http.Request) {
		var ack cluster.PolicyACK
		if !decodeRequest(w, r, &ack) {
			return
		}
		if err := s.AcknowledgePolicy(r.Context(), nodeBearer(r), ack); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("GET /api/v1/settings/retention", func(w http.ResponseWriter, r *http.Request) {
		value, err := s.Retentions(r.Context())
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, value)
	})
	h.mux.HandleFunc("POST /api/v1/settings/retention", func(w http.ResponseWriter, r *http.Request) {
		var value Retentions
		if !decodeRequest(w, r, &value) {
			return
		}
		if err := s.SetRetentions(r.Context(), h.actor(r), value); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}

func (h *httpHandler) controlStream(w http.ResponseWriter, r *http.Request) {
	token := nodeBearer(r)
	s := h.store
	session, err := s.AuthenticateNode(r.Context(), token)
	if err != nil {
		httpError(w, err)
		return
	}
	query := r.URL.Query()
	if len(query) > 1 || (len(query) == 1 && len(query["applied_revision"]) != 1) {
		httpError(w, ErrInvalid)
		return
	}
	var applied uint64
	if value := query.Get("applied_revision"); value != "" {
		applied, err = strconv.ParseUint(value, 10, 63)
		if err != nil {
			httpError(w, ErrInvalid)
			return
		}
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		httpError(w, err)
		return
	}
	stream := &nodeControlStream{changed: make(chan struct{}, 1), done: make(chan struct{}), response: controller}
	s.policyMu.Lock()
	if previous := s.policyStreams[session.NodeID]; previous != nil {
		previous.stop()
	}
	s.policyStreams[session.NodeID] = stream
	s.policyMu.Unlock()
	defer func() {
		s.policyMu.Lock()
		if s.policyStreams[session.NodeID] == stream {
			delete(s.policyStreams, session.NodeID)
		}
		s.policyMu.Unlock()
	}()
	p, err := s.currentPolicy(r.Context(), session.NodeID)
	if err != nil {
		httpError(w, err)
		return
	}
	if applied > p.Revision {
		httpError(w, ErrConflict)
		return
	}
	current, err := s.AuthenticateNode(r.Context(), token)
	if err != nil || current.InstanceID != session.InstanceID {
		httpError(w, ErrUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(message cluster.ControlMessage) bool {
		select {
		case <-stream.done:
			return false
		default:
		}
		b, err := json.Marshal(message)
		if err != nil || len(b)+1 > cluster.MaxControlMessageBytes {
			return false
		}
		if controller.SetWriteDeadline(time.Now().Add(10*time.Second)) != nil {
			return false
		}
		select {
		case <-stream.done:
			controller.SetWriteDeadline(time.Now())
			return false
		default:
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return false
		}
		if controller.Flush() != nil {
			return false
		}
		select {
		case <-stream.done:
			return false
		default:
		}
		return controller.SetWriteDeadline(time.Time{}) == nil
	}
	if !write(cluster.ControlMessage{Type: "policy", Policy: &p}) {
		return
	}
	last := p.Revision
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stream.done:
			return
		case <-stream.changed:
			p, err := s.currentPolicy(r.Context(), session.NodeID)
			if err != nil {
				return
			}
			current, authErr := s.AuthenticateNode(r.Context(), token)
			if authErr != nil || current.InstanceID != session.InstanceID {
				if len(p.Grants) == 0 {
					write(cluster.ControlMessage{Type: "policy", Policy: &p})
				}
				return
			}
			if p.Revision != last {
				if !write(cluster.ControlMessage{Type: "policy", Policy: &p}) {
					return
				}
				last = p.Revision
			}
		case <-ticker.C:
			current, err := s.AuthenticateNode(r.Context(), token)
			if err != nil || current.InstanceID != session.InstanceID {
				return
			}
			if !write(cluster.ControlMessage{Type: "ping"}) {
				return
			}
		}
	}
}

func (s *Store) AcknowledgePolicy(ctx context.Context, token string, ack cluster.PolicyACK) error {
	if ack.Revision == 0 || ack.Revision > math.MaxInt64 {
		return ErrInvalid
	}
	switch ack.State {
	case "received", "applied":
		if ack.Error != "" || (ack.State == "received" && ack.Usable) {
			return ErrInvalid
		}
	case "nack":
		if ack.Usable {
			return ErrInvalid
		}
		switch ack.Error {
		case "host_budget", "invalid_policy", "persist_failed", "apply_failed", "clock_rollback":
		default:
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	session, err := s.nodeSession(ctx, tx, token)
	if err != nil {
		return err
	}
	var desired, received, applied uint64
	var owner string
	var body []byte
	if err := tx.QueryRowContext(ctx, "SELECT p.desired_revision,p.received_revision,p.applied_revision,n.owner_id,p.policy_json FROM node_policies p JOIN nodes n ON n.id=p.node_id WHERE p.node_id=?", session.NodeID).Scan(&desired, &received, &applied, &owner, &body); err != nil {
		return err
	}
	if ack.Revision > desired {
		return ErrConflict
	}
	switch ack.State {
	case "received":
		if ack.Revision < received {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE node_policies SET received_revision=? WHERE node_id=?", ack.Revision, session.NodeID); err != nil {
			return err
		}
	case "applied":
		if ack.Revision > received || ack.Revision < applied {
			return ErrConflict
		}
		p, err := cluster.DecodePolicy(bytes.NewReader(body), s.clusterID, session.NodeID)
		if err != nil {
			return err
		}
		usable := false
		if ack.Usable && ack.Revision == desired {
			for _, g := range p.Grants {
				for _, k := range g.Keys {
					if s.now().Before(cluster.EffectiveUntil(g, k)) {
						usable = true
						break
					}
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE node_policies SET applied_revision=?,derper_usable=? WHERE node_id=?", ack.Revision, usable, session.NodeID); err != nil {
			return err
		}
		state := "registered"
		if usable {
			state = "ready"
		}
		if _, err := tx.ExecContext(ctx, "UPDATE nodes SET state=?,last_error='' WHERE id=?", state, session.NodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE events SET resolved_at=? WHERE resource_type='node' AND resource_id=? AND kind='policy_apply_failed' AND resolved_at=0", s.now().Unix(), session.NodeID); err != nil {
			return err
		}
	case "nack":
		if ack.Revision <= applied || ack.Revision < received {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE nodes SET state='registered',last_error=? WHERE id=?", "policy application failed: "+ack.Error, session.NodeID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(owner_id,resource_type,resource_id,kind,message,created_at) SELECT ?,'node',?,'policy_apply_failed',?,? WHERE NOT EXISTS(SELECT 1 FROM events WHERE resource_type='node' AND resource_id=? AND kind='policy_apply_failed' AND resolved_at=0)`, owner, session.NodeID, "Policy application failed: "+ack.Error, s.now().Unix(), session.NodeID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
