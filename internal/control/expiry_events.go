package control

import (
	"bytes"
	"context"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func (s *Store) refreshExpiryEvents(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	rows, err := tx.QueryContext(ctx, "SELECT node_id,policy_json FROM node_policies WHERE policy_json IS NOT NULL")
	if err != nil {
		return err
	}
	expired := map[string]bool{}
	for rows.Next() {
		var node string
		var body []byte
		if err := rows.Scan(&node, &body); err != nil {
			rows.Close()
			return err
		}
		p, err := cluster.DecodePolicy(bytes.NewReader(body), s.clusterID, node)
		if err != nil {
			rows.Close()
			return err
		}
		for _, g := range p.Grants {
			ended := !now.Before(g.IdentityUntil) || !now.Before(g.ControlUntil) || (!g.ExplicitUntil.IsZero() && !now.Before(g.ExplicitUntil))
			if !ended && len(g.Keys) > 0 {
				ended = true
				for _, k := range g.Keys {
					if now.Before(cluster.EffectiveUntil(g, k)) {
						ended = false
						break
					}
				}
			}
			expired[g.GrantID] = ended
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	type relation struct {
		id, state, nodeOwner, tailnetOwner string
		until                              int64
	}
	rows, err = tx.QueryContext(ctx, "SELECT g.id,g.state,g.explicit_until,n.owner_id,t.owner_id FROM grants g JOIN nodes n ON n.id=g.node_id JOIN tailnets t ON t.id=g.tailnet_id")
	if err != nil {
		return err
	}
	var relations []relation
	for rows.Next() {
		var g relation
		if err := rows.Scan(&g.id, &g.state, &g.until, &g.nodeOwner, &g.tailnetOwner); err != nil {
			rows.Close()
			return err
		}
		relations = append(relations, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, g := range relations {
		ended := (g.state == "requested" || g.state == "owner_approved" || g.state == "active") && ((g.until != 0 && !now.Before(time.Unix(g.until, 0))) || (g.state == "active" && expired[g.id]))
		if !ended {
			if _, err := tx.ExecContext(ctx, "UPDATE events SET resolved_at=? WHERE resource_type='grant' AND resource_id=? AND kind='authorization_expired' AND resolved_at=0", now.Unix(), g.id); err != nil {
				return err
			}
			continue
		}
		owners := []string{g.nodeOwner}
		if g.tailnetOwner != g.nodeOwner {
			owners = append(owners, g.tailnetOwner)
		}
		for _, owner := range owners {
			if _, err := tx.ExecContext(ctx, `INSERT INTO events(owner_id,resource_type,resource_id,kind,message,created_at) SELECT ?,'grant',?,'authorization_expired','Authorization deadline reached; relay permission has expired.',? WHERE NOT EXISTS(SELECT 1 FROM events WHERE owner_id=? AND resource_type='grant' AND resource_id=? AND kind='authorization_expired' AND resolved_at=0)`, owner, g.id, now.Unix(), owner, g.id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
