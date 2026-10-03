package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

type Event struct {
	ID           int64  `json:"id"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Kind         string `json:"kind"`
	Message      string `json:"message"`
	CreatedAt    int64  `json:"created_at"`
	ResolvedAt   int64  `json:"resolved_at"`
}

type Audit struct {
	ID            int64  `json:"id"`
	ActorID       string `json:"actor_id"`
	ActorUsername string `json:"actor_username"`
	ResourceType  string `json:"resource_type"`
	ResourceID    string `json:"resource_id"`
	Action        string `json:"action"`
	CreatedAt     int64  `json:"created_at"`
}

type GrantStatus struct {
	GrantID             string    `json:"grant_id"`
	TailnetID           string    `json:"tailnet_id"`
	LastIdentitySuccess time.Time `json:"last_identity_success"`
	IdentityUntil       time.Time `json:"identity_until"`
	ControlUntil        time.Time `json:"control_until"`
	ExplicitUntil       time.Time `json:"explicit_until"`
	ValidKeys           int       `json:"valid_keys"`
}

type NodeStatus struct {
	Node              Node                `json:"node"`
	DesiredRevision   uint64              `json:"desired_revision"`
	ReceivedRevision  uint64              `json:"received_revision"`
	AppliedRevision   uint64              `json:"applied_revision"`
	ReportedUsable    bool                `json:"reported_usable"`
	PolicyHasLiveKeys bool                `json:"policy_has_live_keys"`
	ControlStreamOpen bool                `json:"control_stream_open"`
	DomainVerifiedAt  int64               `json:"domain_verified_at"`
	InstanceID        string              `json:"instance_id,omitempty"`
	Grants            []GrantStatus       `json:"grants"`
	Report            *cluster.NodeReport `json:"report,omitempty"`
	ReportedAt        int64               `json:"reported_at"`
	Probes            *NodeProbes         `json:"probes,omitempty"`
	TrafficRates      []TailnetRate       `json:"traffic_rates"`
}

type TailnetRate struct {
	TailnetID       string  `json:"tailnet_id"`
	RXBitsPerSecond float64 `json:"rx_bits_per_second"`
	TXBitsPerSecond float64 `json:"tx_bits_per_second"`
	IntervalSeconds float64 `json:"interval_seconds"`
}

type RelaySummary struct {
	Node             Node              `json:"node"`
	Provider         string            `json:"provider"`
	QoS              cluster.QoSPolicy `json:"qos"`
	DomainVerifiedAt int64             `json:"domain_verified_at"`
	Probes           *NodeProbes       `json:"probes,omitempty"`
}

func (s *Store) RelaySummary(ctx context.Context, actor Actor, id string) (RelaySummary, error) {
	if err := RequireOwner(actor, actor.ID); err != nil {
		return RelaySummary{}, err
	}
	result := RelaySummary{QoS: cluster.QoSPolicy{Tailnets: []cluster.TailnetQoS{}}}
	n := &result.Node
	var lease int64
	var probes []byte
	err := s.db.QueryRowContext(ctx, `SELECT n.id,n.owner_id,n.domain,n.display_name,n.region_id,n.state,n.last_heartbeat,n.last_error,n.enabled,u.username,n.lease_until,p.budget_bps,p.owner_weight,p.shared_weight,p.shared_max_bps,n.domain_verified_at,o.probes_json FROM nodes n JOIN users u ON u.id=n.owner_id JOIN node_policies p ON p.node_id=n.id LEFT JOIN node_observations o ON o.node_id=n.id WHERE n.id=? AND n.enabled=1 AND u.enabled=1 AND n.state IN ('registered','ready','offline')`, id).Scan(&n.ID, &n.OwnerID, &n.Domain, &n.DisplayName, &n.RegionID, &n.State, &n.LastHeartbeat, &n.LastError, &n.Enabled, &result.Provider, &lease, &result.QoS.BudgetBPS, &result.QoS.OwnerWeight, &result.QoS.SharedWeight, &result.QoS.SharedMaxBPS, &result.DomainVerifiedAt, &probes)
	if err != nil {
		return result, err
	}
	if lease != 0 && !s.now().Before(time.Unix(lease, 0)) {
		n.State = "offline"
	}
	if len(probes) != 0 {
		result.Probes = new(NodeProbes)
		if err := json.Unmarshal(probes, result.Probes); err != nil {
			return result, err
		}
	}
	return result, nil
}

func saveNodeReport(ctx context.Context, tx *sql.Tx, session cluster.NodeSession, report cluster.NodeReport, desired uint64, now time.Time) error {
	if report.Revision > desired || report.ObservedAt.IsZero() || report.ObservedAt.After(now.Add(5*time.Second)) || len(report.Traffic) > 4096 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, traffic := range report.Traffic {
		if traffic.TailnetID == "" || seen[traffic.TailnetID] {
			return ErrInvalid
		}
		seen[traffic.TailnetID] = true
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM grants WHERE node_id=? AND tailnet_id=?", session.NodeID, traffic.TailnetID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return ErrInvalid
		}
	}
	var instance string
	var previous []byte
	err := tx.QueryRowContext(ctx, "SELECT report_instance,report_json FROM node_observations WHERE node_id=?", session.NodeID).Scan(&instance, &previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if instance == session.InstanceID {
		var old cluster.NodeReport
		if err := json.Unmarshal(previous, &old); err != nil {
			return err
		}
		if report.Revision < old.Revision || !report.ObservedAt.After(old.ObservedAt) {
			return nil
		}
	}
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO node_observations(node_id,report_instance,report_json,reported_at) VALUES(?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET previous_report_json=CASE WHEN report_instance=excluded.report_instance THEN report_json ELSE NULL END,report_instance=excluded.report_instance,report_json=excluded.report_json,reported_at=excluded.reported_at", session.NodeID, session.InstanceID, body, now.Unix())
	return err
}

func (s *Store) Events(ctx context.Context, actor Actor) ([]Event, error) {
	if err := RequireOwner(actor, actor.ID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,resource_type,resource_id,kind,message,created_at,resolved_at FROM events WHERE owner_id=? OR ?='admin' ORDER BY id DESC LIMIT 200", actor.ID, actor.Role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.ResourceType, &e.ResourceID, &e.Kind, &e.Message, &e.CreatedAt, &e.ResolvedAt); err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return items, rows.Err()
}

func (s *Store) Audit(ctx context.Context, actor Actor) ([]Audit, error) {
	if err := RequireOwner(actor, actor.ID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT a.id,a.actor_id,coalesce(u.username,a.actor_id),a.resource_type,a.resource_id,a.action,a.created_at FROM audit a LEFT JOIN users u ON u.id=a.actor_id WHERE a.owner_id=? OR ?='admin' ORDER BY a.id DESC LIMIT 200", actor.ID, actor.Role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Audit{}
	for rows.Next() {
		var a Audit
		if err := rows.Scan(&a.ID, &a.ActorID, &a.ActorUsername, &a.ResourceType, &a.ResourceID, &a.Action, &a.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

func (s *Store) NodeStatus(ctx context.Context, actor Actor, id string) (NodeStatus, error) {
	if err := RequireOwner(actor, actor.ID); err != nil {
		return NodeStatus{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeStatus{}, err
	}
	defer tx.Rollback()
	state := NodeStatus{Grants: []GrantStatus{}, TrafficRates: []TailnetRate{}}
	n, err := scanNode(tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id=?", id))
	if err != nil {
		return state, err
	}
	owner := RequireOwner(actor, n.OwnerID) == nil
	if !owner {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM grants g JOIN tailnets t ON t.id=g.tailnet_id WHERE g.node_id=? AND t.owner_id=? AND g.state IN ('requested','owner_approved','active')", id, actor.ID).Scan(&count); err != nil {
			return state, err
		}
		if count == 0 {
			return state, ErrForbidden
		}
	}
	var lease int64
	var instance string
	if err := tx.QueryRowContext(ctx, "SELECT lease_until,instance_id FROM nodes WHERE id=?", id).Scan(&lease, &instance); err != nil {
		return state, err
	}
	if owner {
		state.InstanceID = instance
	}
	if (n.State == "registered" || n.State == "ready") && lease != 0 && !s.now().Before(time.Unix(lease, 0)) {
		n.State = "offline"
	}
	state.Node = n
	var probesBody []byte
	err = tx.QueryRowContext(ctx, "SELECT probes_json FROM node_observations WHERE node_id=?", id).Scan(&probesBody)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	if len(probesBody) != 0 {
		state.Probes = new(NodeProbes)
		if err := json.Unmarshal(probesBody, state.Probes); err != nil {
			return state, err
		}
	}
	var reportBody []byte
	var previousBody []byte
	err = tx.QueryRowContext(ctx, "SELECT report_json,reported_at,previous_report_json FROM node_observations WHERE node_id=? AND report_instance=?", id, instance).Scan(&reportBody, &state.ReportedAt, &previousBody)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	if len(reportBody) != 0 {
		state.Report = new(cluster.NodeReport)
		if err := json.Unmarshal(reportBody, state.Report); err != nil {
			return state, err
		}
		if !owner {
			state.Report.ActiveConnections = nil
			filtered := []cluster.TailnetTraffic{}
			for _, traffic := range state.Report.Traffic {
				var tailnetOwner string
				err := tx.QueryRowContext(ctx, "SELECT owner_id FROM tailnets WHERE id=?", traffic.TailnetID).Scan(&tailnetOwner)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return state, err
				}
				if tailnetOwner == actor.ID {
					filtered = append(filtered, traffic)
				}
			}
			state.Report.Traffic = filtered
		}
		if len(previousBody) != 0 {
			var previous cluster.NodeReport
			if err := json.Unmarshal(previousBody, &previous); err != nil {
				return state, err
			}
			seconds := state.Report.ObservedAt.Sub(previous.ObservedAt).Seconds()
			if seconds > 0 {
				old := map[string]cluster.TailnetTraffic{}
				for _, traffic := range previous.Traffic {
					old[traffic.TailnetID] = traffic
				}
				for _, traffic := range state.Report.Traffic {
					before, ok := old[traffic.TailnetID]
					if ok && traffic.CounterID != 0 && traffic.CounterID == before.CounterID && traffic.RXPayloadBytes >= before.RXPayloadBytes && traffic.TXPayloadBytes >= before.TXPayloadBytes {
						state.TrafficRates = append(state.TrafficRates, TailnetRate{TailnetID: traffic.TailnetID, RXBitsPerSecond: float64(traffic.RXPayloadBytes-before.RXPayloadBytes) * 8 / seconds, TXBitsPerSecond: float64(traffic.TXPayloadBytes-before.TXPayloadBytes) * 8 / seconds, IntervalSeconds: seconds})
					}
				}
			}
		}
	}
	if err := tx.QueryRowContext(ctx, "SELECT domain_verified_at FROM nodes WHERE id=?", id).Scan(&state.DomainVerifiedAt); err != nil {
		return state, err
	}
	var body []byte
	err = tx.QueryRowContext(ctx, "SELECT desired_revision,received_revision,applied_revision,derper_usable,policy_json FROM node_policies WHERE node_id=?", id).Scan(&state.DesiredRevision, &state.ReceivedRevision, &state.AppliedRevision, &state.ReportedUsable, &body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, err
	}
	if len(body) != 0 {
		p, err := cluster.DecodePolicy(bytes.NewReader(body), s.clusterID, id)
		if err != nil {
			return state, err
		}
		rows, err := tx.QueryContext(ctx, "SELECT id FROM tailnets WHERE owner_id=? OR ?=1", actor.ID, owner)
		if err != nil {
			return state, err
		}
		allowed := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return state, err
			}
			allowed[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return state, err
		}
		for _, g := range p.Grants {
			if !allowed[g.TailnetID] {
				continue
			}
			status := GrantStatus{GrantID: g.GrantID, TailnetID: g.TailnetID, LastIdentitySuccess: g.LastIdentitySuccess, IdentityUntil: g.IdentityUntil, ControlUntil: g.ControlUntil, ExplicitUntil: g.ExplicitUntil}
			for _, k := range g.Keys {
				if s.now().Before(cluster.EffectiveUntil(g, k)) {
					status.ValidKeys++
				}
			}
			if status.ValidKeys > 0 {
				state.PolicyHasLiveKeys = true
			}
			state.Grants = append(state.Grants, status)
		}
	}
	s.policyMu.Lock()
	state.ControlStreamOpen = s.policyStreams[id] != nil
	s.policyMu.Unlock()
	return state, nil
}

func (h *httpHandler) mountObservability() {
	h.mux.HandleFunc("GET /api/v1/nodes/{id}/summary", func(w http.ResponseWriter, r *http.Request) {
		result, err := h.store.RelaySummary(r.Context(), h.actor(r), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, result)
	})
	h.mux.HandleFunc("POST /api/v1/nodes/{id}/probe", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := h.store.ProbeNode(r.Context(), h.actor(r), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, result)
	})
	h.mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		items, err := h.store.Events(r.Context(), h.actor(r))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, items)
	})
	h.mux.HandleFunc("GET /api/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		items, err := h.store.Audit(r.Context(), h.actor(r))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, items)
	})
	h.mux.HandleFunc("GET /api/v1/nodes/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		state, err := h.store.NodeStatus(r.Context(), h.actor(r), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, state)
	})
}
