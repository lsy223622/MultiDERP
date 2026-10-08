package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

type Retentions struct {
	IdentitySeconds int64 `json:"identity_seconds"`
	ControlSeconds  int64 `json:"control_seconds"`
}

func readRetentions(ctx context.Context, tx *sql.Tx) (Retentions, error) {
	var r Retentions
	var identity, control string
	if err := tx.QueryRowContext(ctx, "SELECT (SELECT value FROM settings WHERE key='identity_retention_seconds'),(SELECT value FROM settings WHERE key='control_retention_seconds')").Scan(&identity, &control); err != nil {
		return r, err
	}
	var err error
	r.IdentitySeconds, err = strconv.ParseInt(identity, 10, 64)
	if err != nil {
		return r, ErrInvalid
	}
	r.ControlSeconds, err = strconv.ParseInt(control, 10, 64)
	if err != nil || !validRetentions(r) {
		return r, ErrInvalid
	}
	return r, nil
}

func validRetentions(r Retentions) bool {
	maxSeconds := int64(math.MaxInt64) / int64(time.Second)
	return r.IdentitySeconds > 0 && r.ControlSeconds > 0 && r.IdentitySeconds <= maxSeconds && r.ControlSeconds <= maxSeconds
}

func (s *Store) Retentions(ctx context.Context) (Retentions, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Retentions{}, err
	}
	defer tx.Rollback()
	return readRetentions(ctx, tx)
}

func (s *Store) SetRetentions(ctx context.Context, actor Actor, r Retentions) error {
	if !actor.Enabled || actor.Role != "admin" {
		return ErrForbidden
	}
	if !validRetentions(r) {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE settings SET value=? WHERE key='identity_retention_seconds'", strconv.FormatInt(r.IdentitySeconds, 10)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE settings SET value=? WHERE key='control_retention_seconds'", strconv.FormatInt(r.ControlSeconds, 10)); err != nil {
		return err
	}
	if err := s.rebuildPolicies(ctx, tx, s.now()); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, actor.ID, "cluster", s.clusterID, "retention.update"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicies()
	return nil
}

func (s *Store) rebuildPolicies(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, "SELECT node_id FROM node_policies ORDER BY node_id")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.buildPolicy(ctx, tx, id, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) BuildPolicy(ctx context.Context, id string, now time.Time) (cluster.Policy, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cluster.Policy{}, err
	}
	defer tx.Rollback()
	p, err := s.buildPolicy(ctx, tx, id, now)
	if err != nil {
		return cluster.Policy{}, err
	}
	if err := tx.Commit(); err != nil {
		return cluster.Policy{}, err
	}
	s.notifyPolicy(id)
	return p, nil
}

func (s *Store) buildPolicy(ctx context.Context, tx *sql.Tx, id string, now time.Time) (cluster.Policy, error) {
	var owner, state string
	var enabled bool
	if err := tx.QueryRowContext(ctx, "SELECT n.owner_id,n.state,n.enabled=1 AND u.enabled=1 FROM nodes n JOIN users u ON u.id=n.owner_id WHERE n.id=?", id).Scan(&owner, &state, &enabled); err != nil {
		return cluster.Policy{}, err
	}
	p := cluster.Policy{ClusterID: s.clusterID, NodeID: id, Grants: []cluster.GrantPolicy{}, QoS: cluster.QoSPolicy{Tailnets: []cluster.TailnetQoS{}}}
	var revision int64
	var previous []byte
	if err := tx.QueryRowContext(ctx, "SELECT desired_revision,policy_json,budget_bps,owner_weight,shared_weight,shared_max_bps FROM node_policies WHERE node_id=?", id).Scan(&revision, &previous, &p.QoS.BudgetBPS, &p.QoS.OwnerWeight, &p.QoS.SharedWeight, &p.QoS.SharedMaxBPS); err != nil {
		return cluster.Policy{}, err
	}
	retention, err := readRetentions(ctx, tx)
	if err != nil {
		return cluster.Policy{}, err
	}
	all, err := identitySnapshots(ctx, tx)
	if err != nil {
		return cluster.Policy{}, err
	}
	conflicts := conflictingKeys(all)
	if enabled && (state == "registered" || state == "ready" || state == "offline") {
		rows, err := tx.QueryContext(ctx, `SELECT g.id,g.revision,g.tailnet_id,g.explicit_until,g.last_control_success,t.owner_id,r.group_name,coalesce(r.weight,1),coalesce(r.max_bps,0) FROM grants g JOIN tailnets t ON t.id=g.tailnet_id JOIN users u ON u.id=t.owner_id LEFT JOIN tailnet_rules r ON r.node_id=g.node_id AND r.tailnet_id=g.tailnet_id WHERE g.node_id=? AND g.state='active' AND t.enabled=1 AND u.enabled=1 ORDER BY g.tailnet_id`, id)
		if err != nil {
			return cluster.Policy{}, err
		}
		for rows.Next() {
			var g cluster.GrantPolicy
			var explicit, lastControl int64
			var tnOwner string
			var group sql.NullString
			var weight uint32
			var ceiling uint64
			if err := rows.Scan(&g.GrantID, &g.GrantRevision, &g.TailnetID, &explicit, &lastControl, &tnOwner, &group, &weight, &ceiling); err != nil {
				rows.Close()
				return cluster.Policy{}, err
			}
			snap, ok := all[g.TailnetID]
			if !ok || lastControl <= 0 {
				continue
			}
			g.LastIdentitySuccess = snap.LastSuccess
			g.IdentityUntil = snap.LastSuccess.Add(time.Duration(retention.IdentitySeconds) * time.Second)
			g.ControlUntil = time.Unix(lastControl, 0).UTC().Add(time.Duration(retention.ControlSeconds) * time.Second)
			if explicit != 0 {
				g.ExplicitUntil = time.Unix(explicit, 0).UTC()
			}
			g.Keys = []cluster.DeviceKey{}
			for _, k := range snap.Keys {
				if !conflicts[k.NodePublic] {
					g.Keys = append(g.Keys, cluster.DeviceKey{NodePublic: k.NodePublic, NotAfter: k.NotAfter})
				}
			}
			groupName := "shared"
			if tnOwner == owner {
				groupName = "owner"
			}
			if group.Valid {
				groupName = group.String
			}
			if groupName == "owner" && tnOwner != owner {
				rows.Close()
				return cluster.Policy{}, ErrInvalid
			}
			p.Grants = append(p.Grants, g)
			p.QoS.Tailnets = append(p.QoS.Tailnets, cluster.TailnetQoS{TailnetID: g.TailnetID, Group: groupName, Weight: weight, MaxBPS: ceiling})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return cluster.Policy{}, err
		}
	}
	if revision < 0 || revision == math.MaxInt64 {
		return cluster.Policy{}, ErrConflict
	}
	if len(previous) != 0 {
		old, err := cluster.DecodePolicy(bytes.NewReader(previous), s.clusterID, id)
		if err != nil || old.Revision != uint64(revision) {
			return cluster.Policy{}, ErrInvalid
		}
		p.Revision = old.Revision
		p.GeneratedAt = old.GeneratedAt
		candidate, err := json.Marshal(p)
		if err != nil {
			return cluster.Policy{}, err
		}
		if bytes.Equal(candidate, previous) {
			return old, nil
		}
		if now.Before(old.GeneratedAt) {
			return cluster.Policy{}, ErrConflict
		}
	}
	p.Revision = uint64(revision + 1)
	p.GeneratedAt = now.UTC()
	if err := cluster.ValidatePolicy(p, s.clusterID, id); err != nil {
		return cluster.Policy{}, err
	}
	body, err := json.Marshal(p)
	if err != nil || len(body) > cluster.MaxPolicyBytes {
		return cluster.Policy{}, ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, "UPDATE node_policies SET desired_revision=?,generated_at=?,policy_json=? WHERE node_id=?", p.Revision, p.GeneratedAt.Unix(), body, id); err != nil {
		return cluster.Policy{}, err
	}
	return p, nil
}

func (s *Store) NodeHeartbeat(ctx context.Context, token string, report *cluster.NodeReport) (cluster.NodeHeartbeat, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cluster.NodeHeartbeat{}, err
	}
	defer tx.Rollback()
	session, err := s.nodeSession(ctx, tx, token)
	if err != nil {
		return cluster.NodeHeartbeat{}, err
	}
	now := s.now().UTC()
	lease := now.Add(90 * time.Second)
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET lease_until=?,last_heartbeat=? WHERE id=?", lease.Unix(), now.Unix(), session.NodeID); err != nil {
		return cluster.NodeHeartbeat{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE grants SET last_control_success=? WHERE node_id=? AND state='active'", now.Unix(), session.NodeID); err != nil {
		return cluster.NodeHeartbeat{}, err
	}
	p, err := s.buildPolicy(ctx, tx, session.NodeID, now)
	if err != nil {
		return cluster.NodeHeartbeat{}, err
	}
	if report != nil {
		if err := saveNodeReport(ctx, tx, session, *report, p.Revision, now); err != nil {
			return cluster.NodeHeartbeat{}, err
		}
	}
	retention, err := readRetentions(ctx, tx)
	if err != nil {
		return cluster.NodeHeartbeat{}, err
	}
	interval := max(int64(250), min(int64(30000), retention.ControlSeconds*1000/3))
	if err := tx.Commit(); err != nil {
		return cluster.NodeHeartbeat{}, err
	}
	s.notifyPolicy(session.NodeID)
	return cluster.NodeHeartbeat{NodeID: session.NodeID, LeaseUntil: lease, DesiredRevision: p.Revision, HeartbeatIntervalMillis: interval}, nil
}
