package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type IdentitySnapshot struct {
	Revision    int64
	LastSuccess time.Time
	Keys        []identityKey
}

func identitySnapshots(ctx context.Context, tx *sql.Tx) (map[string]IdentitySnapshot, error) {
	rows, err := tx.QueryContext(ctx, "SELECT tailnet_id,revision,last_success,keys_json FROM identity_snapshots")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := make(map[string]IdentitySnapshot)
	for rows.Next() {
		var id string
		var last int64
		var data []byte
		var snap IdentitySnapshot
		if err := rows.Scan(&id, &snap.Revision, &last, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &snap.Keys); err != nil {
			return nil, err
		}
		snap.LastSuccess = time.Unix(last, 0).UTC()
		all[id] = snap
	}
	return all, rows.Err()
}

func conflictingKeys(all map[string]IdentitySnapshot) map[string]bool {
	owners := make(map[string]string)
	conflicts := make(map[string]bool)
	for id, snap := range all {
		for _, k := range snap.Keys {
			if owner, ok := owners[k.NodePublic]; ok && owner != id {
				conflicts[k.NodePublic] = true
			} else {
				owners[k.NodePublic] = id
			}
		}
	}
	return conflicts
}

func (s *Store) IdentitySnapshot(ctx context.Context, id string) (IdentitySnapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IdentitySnapshot{}, err
	}
	defer tx.Rollback()
	all, err := identitySnapshots(ctx, tx)
	if err != nil {
		return IdentitySnapshot{}, err
	}
	snap, ok := all[id]
	if !ok {
		return IdentitySnapshot{}, sql.ErrNoRows
	}
	conflicts := conflictingKeys(all)
	keys := make([]identityKey, 0, len(snap.Keys))
	for _, k := range snap.Keys {
		if !conflicts[k.NodePublic] {
			keys = append(keys, k)
		}
	}
	snap.Keys = keys
	return snap, nil
}

func (s *Store) publishIdentity(ctx context.Context, tx *sql.Tx, id string, keys []identityKey) error {
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	var last int64
	err = tx.QueryRowContext(ctx, "SELECT last_success FROM identity_snapshots WHERE tailnet_id=?", id).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if s.now().Unix() < last {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO identity_snapshots(tailnet_id,revision,last_success,keys_json) VALUES(?,1,?,?) ON CONFLICT(tailnet_id) DO UPDATE SET revision=revision+1,last_success=excluded.last_success,keys_json=excluded.keys_json`, id, s.now().Unix(), data); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE events SET resolved_at=? WHERE resource_id=? AND kind='identity_unavailable' AND resolved_at=0", s.now().Unix(), id); err != nil {
		return err
	}
	return s.updateIdentityConflicts(ctx, tx)
}

func (s *Store) updateIdentityConflicts(ctx context.Context, tx *sql.Tx) error {
	all, err := identitySnapshots(ctx, tx)
	if err != nil {
		return err
	}
	conflicts := conflictingKeys(all)
	for tailnet, snap := range all {
		conflict := false
		for _, k := range snap.Keys {
			if conflicts[k.NodePublic] {
				conflict = true
				break
			}
		}
		if conflict {
			if err := s.identityEvent(ctx, tx, tailnet, "identity_conflict", "conflicting identity keys are blocked"); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, "UPDATE events SET resolved_at=? WHERE resource_id=? AND kind='identity_conflict' AND resolved_at=0", s.now().Unix(), tailnet); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) identityEvent(ctx context.Context, tx *sql.Tx, id, kind, message string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events(owner_id,resource_type,resource_id,kind,message,created_at) SELECT owner_id,'tailnet',id,?,?,? FROM tailnets WHERE id=? AND NOT EXISTS(SELECT 1 FROM events WHERE resource_id=? AND kind=? AND resolved_at=0)`, kind, message, s.now().Unix(), id, id, kind)
	return err
}

func (s *Store) RefreshIdentity(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var encrypted []byte
	var apiID string
	var revision, seq int64
	if err := tx.QueryRowContext(ctx, `SELECT t.api_id,c.encrypted,c.revision,c.refresh_seq FROM credentials c JOIN tailnets t ON t.id=c.tailnet_id JOIN users u ON u.id=t.owner_id WHERE t.id=? AND t.enabled=1 AND u.enabled=1 AND c.status!='missing'`, id).Scan(&apiID, &encrypted, &revision, &seq); err != nil {
		return err
	}
	seq++
	if _, err := tx.ExecContext(ctx, "UPDATE credentials SET refresh_seq=? WHERE tailnet_id=?", seq, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	credential, fetchErr := s.openCredential(id, encrypted)
	var keys []identityKey
	if fetchErr == nil {
		keys, fetchErr = s.fetchIdentity(ctx, apiID, credential)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRevision, currentSeq int64
	if err := tx.QueryRowContext(ctx, "SELECT revision,refresh_seq FROM credentials WHERE tailnet_id=?", id).Scan(&currentRevision, &currentSeq); err != nil {
		return err
	}
	if currentRevision != revision || currentSeq != seq {
		return ErrConflict
	}
	if fetchErr != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE credentials SET status='unavailable',error=? WHERE tailnet_id=?", ErrIdentityUnavailable.Error(), id); err != nil {
			return err
		}
		if err := s.identityEvent(ctx, tx, id, "identity_unavailable", "identity source unavailable; cached identity retains its original deadline"); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, "UPDATE credentials SET status='valid',error='' WHERE tailnet_id=?", id); err != nil {
			return err
		}
		if err := s.publishIdentity(ctx, tx, id, keys); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if fetchErr != nil {
		return ErrIdentityUnavailable
	}
	return nil
}
