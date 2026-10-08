package control

import (
	"context"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func (s *Store) NodeQoS(ctx context.Context, actor Actor, node string) (cluster.QoSPolicy, error) {
	if err := RequireOwner(actor, actor.ID); err != nil {
		return cluster.QoSPolicy{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cluster.QoSPolicy{}, err
	}
	defer tx.Rollback()
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT owner_id FROM nodes WHERE id=?", node).Scan(&owner); err != nil {
		return cluster.QoSPolicy{}, err
	}
	if RequireOwner(actor, owner) != nil {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM grants g JOIN tailnets t ON t.id=g.tailnet_id WHERE g.node_id=? AND t.owner_id=? AND g.state IN ('requested','owner_approved','active')`, node, actor.ID).Scan(&count); err != nil {
			return cluster.QoSPolicy{}, err
		}
		if count == 0 {
			return cluster.QoSPolicy{}, ErrForbidden
		}
	}
	q := cluster.QoSPolicy{Tailnets: []cluster.TailnetQoS{}}
	if err := tx.QueryRowContext(ctx, "SELECT budget_bps,owner_weight,shared_weight,shared_max_bps FROM node_policies WHERE node_id=?", node).Scan(&q.BudgetBPS, &q.OwnerWeight, &q.SharedWeight, &q.SharedMaxBPS); err != nil {
		return q, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.tailnet_id,coalesce(r.group_name,CASE WHEN t.owner_id=? THEN 'owner' ELSE 'shared' END),coalesce(r.weight,1),coalesce(r.max_bps,0) FROM grants g JOIN tailnets t ON t.id=g.tailnet_id LEFT JOIN tailnet_rules r ON r.node_id=g.node_id AND r.tailnet_id=g.tailnet_id WHERE g.node_id=? AND g.state IN ('requested','owner_approved','active') AND (t.owner_id=? OR ?=1) ORDER BY g.tailnet_id`, owner, node, actor.ID, RequireOwner(actor, owner) == nil)
	if err != nil {
		return q, err
	}
	defer rows.Close()
	for rows.Next() {
		var rule cluster.TailnetQoS
		if err := rows.Scan(&rule.TailnetID, &rule.Group, &rule.Weight, &rule.MaxBPS); err != nil {
			return q, err
		}
		q.Tailnets = append(q.Tailnets, rule)
	}
	return q, rows.Err()
}

func (s *Store) SetNodeQoS(ctx context.Context, actor Actor, node string, q cluster.QoSPolicy) error {
	if err := cluster.ValidateQoS(q); err != nil {
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
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT owner_id FROM nodes WHERE id=?", node).Scan(&owner); err != nil {
		return err
	}
	if err := RequireOwner(actor, owner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE node_policies SET budget_bps=?,owner_weight=?,shared_weight=?,shared_max_bps=? WHERE node_id=?", q.BudgetBPS, q.OwnerWeight, q.SharedWeight, q.SharedMaxBPS, node); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tailnet_rules WHERE node_id=?", node); err != nil {
		return err
	}
	for _, rule := range q.Tailnets {
		var tnOwner string
		if err := tx.QueryRowContext(ctx, `SELECT t.owner_id FROM grants g JOIN tailnets t ON t.id=g.tailnet_id WHERE g.node_id=? AND g.tailnet_id=? AND g.state IN ('requested','owner_approved','active')`, node, rule.TailnetID).Scan(&tnOwner); err != nil {
			return ErrInvalid
		}
		if rule.Group == "owner" && tnOwner != owner {
			return ErrInvalid
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO tailnet_rules(node_id,tailnet_id,group_name,weight,max_bps) VALUES(?,?,?,?,?)", node, rule.TailnetID, rule.Group, rule.Weight, rule.MaxBPS); err != nil {
			return err
		}
	}
	if _, err := s.buildPolicy(ctx, tx, node, s.now()); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, owner, "node", node, "node.qos"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicy(node)
	return nil
}
