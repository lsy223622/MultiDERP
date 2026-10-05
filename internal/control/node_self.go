package control

import (
	"context"
	"database/sql"
)

func (s *Store) SetNodeSessionPorts(ctx context.Context, token string, derpPort, stunPort int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	session, err := s.nodeSession(ctx, tx, token)
	if err != nil {
		return err
	}
	n, err := scanNode(tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id=?", session.NodeID))
	if err != nil {
		return err
	}
	if err := s.setNodePorts(ctx, tx, n, "node:"+n.ID, derpPort, stunPort); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) releaseNode(ctx context.Context, tx *sql.Tx, id string) error {
	for _, table := range []string{"node_sessions", "node_challenges", "enrollments", "node_observations"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE node_id=?", id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET state='pending',instance_id='',lease_until=0,domain_verified_at=0,last_heartbeat=0,last_error='' WHERE id=?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE grants SET state='revoked',revision=revision+1,last_control_success=0 WHERE node_id=? AND state!='revoked'", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE node_policies SET received_revision=0,applied_revision=0,derper_usable=0,policy_json=NULL WHERE node_id=?", id); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM node_policies WHERE node_id=?", id).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		_, err := s.buildPolicy(ctx, tx, id, s.now())
		return err
	}
	return nil
}

func (s *Store) closeNodeStream(id string) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if stream := s.policyStreams[id]; stream != nil {
		stream.stop()
		delete(s.policyStreams, id)
	}
}

func (s *Store) ReleaseNode(ctx context.Context, token string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	session, err := s.nodeSession(ctx, tx, token)
	if err != nil {
		return err
	}
	if err := s.releaseNode(ctx, tx, session.NodeID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.closeNodeStream(session.NodeID)
	return nil
}
