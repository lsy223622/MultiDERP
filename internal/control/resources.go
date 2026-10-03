package control

import (
	"context"
	"database/sql"
)

func (s *Store) SetTailnetEnabled(ctx context.Context, actor Actor, id string, enabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	owner, err := tailnetOwner(ctx, tx, actor, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tailnets SET enabled=? WHERE id=?", enabled, id); err != nil {
		return err
	}
	if err := s.rebuildPolicies(ctx, tx, s.now()); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, owner, "tailnet", id, "tailnet.enabled"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicies()
	return nil
}

func (s *Store) DeleteTailnet(ctx context.Context, actor Actor, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	owner, err := tailnetOwner(ctx, tx, actor, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tailnets WHERE id=?", id); err != nil {
		return err
	}
	if err := s.rebuildPolicies(ctx, tx, s.now()); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, owner, "tailnet", id, "tailnet.delete"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicies()
	return nil
}

func (s *Store) SetNodeEnabled(ctx context.Context, actor Actor, id string, enabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return err
	}
	n, err := scanNode(tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id=?", id))
	if err != nil {
		return err
	}
	if err := RequireOwner(actor, n.OwnerID); err != nil {
		return err
	}
	if enabled && n.State == "domain_pending" {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET enabled=? WHERE id=?", enabled, id); err != nil {
		return err
	}
	if n.State != "pending" {
		if _, err := s.buildPolicy(ctx, tx, id, s.now()); err != nil {
			return err
		}
	}
	if err := writeAudit(ctx, tx, actor.ID, n.OwnerID, "node", id, "node.enabled"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicy(id)
	return nil
}

func (s *Store) DeleteNode(ctx context.Context, actor Actor, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return err
	}
	n, err := scanNode(tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id=?", id))
	if err != nil {
		return err
	}
	if err := RequireOwner(actor, n.OwnerID); err != nil {
		return err
	}
	if n.State != "pending" {
		if n.Enabled {
			return ErrConflict
		}
		var desired, applied uint64
		err := tx.QueryRowContext(ctx, "SELECT desired_revision,applied_revision FROM node_policies WHERE node_id=?", id).Scan(&desired, &applied)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		s.policyMu.Lock()
		connected := s.policyStreams[id] != nil
		s.policyMu.Unlock()
		if connected && applied != desired {
			return ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM nodes WHERE id=?", id); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, n.OwnerID, "node", id, "node.delete"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicy(id)
	return nil
}

func (s *Store) ChangeNodeDomain(ctx context.Context, actor Actor, id, domain string) error {
	domain, err := normalizeNodeDomain(domain)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return err
	}
	n, err := scanNode(tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id=?", id))
	if err != nil {
		return err
	}
	if err := RequireOwner(actor, n.OwnerID); err != nil {
		return err
	}
	if n.Enabled || (n.State != "registered" && n.State != "ready" && n.State != "offline" && n.State != "domain_pending") {
		return ErrConflict
	}
	if domain == n.Domain {
		return nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM nodes WHERE domain=? AND id<>?", domain, id).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrConflict
	}
	var desired, applied uint64
	if err := tx.QueryRowContext(ctx, "SELECT desired_revision,applied_revision FROM node_policies WHERE node_id=?", id).Scan(&desired, &applied); err != nil {
		return err
	}
	s.policyMu.Lock()
	connected := s.policyStreams[id] != nil
	s.policyMu.Unlock()
	if connected && desired != applied {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET domain=?,state='domain_pending',domain_verified_at=0,last_error='' WHERE id=?", domain, id); err != nil {
		return err
	}
	for _, table := range []string{"node_sessions", "node_challenges", "enrollments", "node_observations"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE node_id=?", id); err != nil {
			return err
		}
	}
	if err := writeAudit(ctx, tx, actor.ID, n.OwnerID, "node", id, "node.domain.change"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicy(id)
	return nil
}
