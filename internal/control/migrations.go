package control

import "fmt"

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == 4 {
		return nil
	}
	if version < 0 || version > 4 {
		return fmt.Errorf("unsupported controller database version %d", version)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 1 {
		if _, err := tx.Exec(`ALTER TABLE credentials ADD COLUMN revision INTEGER NOT NULL DEFAULT 1; ALTER TABLE credentials ADD COLUMN refresh_seq INTEGER NOT NULL DEFAULT 0;`); err != nil {
			return err
		}
	}
	if version == 0 {
		_, err = tx.Exec(`
CREATE TABLE users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE NOCASE,
 role TEXT NOT NULL CHECK(role IN ('admin','member')), enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 password_hash BLOB NOT NULL, session_version INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE sessions (
 token_hash BLOB PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 session_version INTEGER NOT NULL, csrf_hash BLOB NOT NULL, expires_at INTEGER NOT NULL
);
CREATE TABLE tailnets (
 id TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES users(id), display_name TEXT NOT NULL,
 api_id TEXT UNIQUE, enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0,1))
);
CREATE TABLE credentials (
 tailnet_id TEXT PRIMARY KEY REFERENCES tailnets(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, encrypted BLOB NOT NULL, status TEXT NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1, refresh_seq INTEGER NOT NULL DEFAULT 0,
 updated_at INTEGER NOT NULL, error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE identity_snapshots (
 tailnet_id TEXT PRIMARY KEY REFERENCES tailnets(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL, last_success INTEGER NOT NULL, keys_json BLOB NOT NULL
);
CREATE TABLE nodes (
 id TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES users(id), domain TEXT NOT NULL UNIQUE,
 display_name TEXT NOT NULL, region_id INTEGER NOT NULL UNIQUE, public_key BLOB,
 state TEXT NOT NULL DEFAULT 'pending', instance_id TEXT NOT NULL DEFAULT '', lease_until INTEGER NOT NULL DEFAULT 0,
 last_heartbeat INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE enrollments (
 node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
 code_hash BLOB NOT NULL UNIQUE, expires_at INTEGER NOT NULL,
 consumed_at INTEGER NOT NULL DEFAULT 0, request_hash BLOB
);
CREATE TABLE node_sessions (
 token_hash BLOB PRIMARY KEY, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 instance_id TEXT NOT NULL, expires_at INTEGER NOT NULL
);
CREATE TABLE grants (
 id TEXT PRIMARY KEY, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 tailnet_id TEXT NOT NULL REFERENCES tailnets(id) ON DELETE CASCADE,
 state TEXT NOT NULL, revision INTEGER NOT NULL, explicit_until INTEGER NOT NULL DEFAULT 0,
 UNIQUE(node_id,tailnet_id)
);
CREATE TABLE node_policies (
 node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
 desired_revision INTEGER NOT NULL DEFAULT 0, received_revision INTEGER NOT NULL DEFAULT 0,
 applied_revision INTEGER NOT NULL DEFAULT 0, generated_at INTEGER NOT NULL DEFAULT 0,
 policy_json BLOB, budget_bps INTEGER NOT NULL DEFAULT 100000000,
 owner_weight INTEGER NOT NULL DEFAULT 8, shared_weight INTEGER NOT NULL DEFAULT 2,
 shared_max_bps INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE tailnet_rules (
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 tailnet_id TEXT NOT NULL REFERENCES tailnets(id) ON DELETE CASCADE,
 group_name TEXT NOT NULL CHECK(group_name IN ('owner','shared')),
 weight INTEGER NOT NULL DEFAULT 1, max_bps INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(node_id,tailnet_id)
);
CREATE TABLE settings (key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE audit (
 id INTEGER PRIMARY KEY, actor_id TEXT NOT NULL, owner_id TEXT NOT NULL,
 resource_type TEXT NOT NULL, resource_id TEXT NOT NULL, action TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE events (
 id INTEGER PRIMARY KEY, owner_id TEXT NOT NULL, resource_type TEXT NOT NULL,
 resource_id TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL,
 created_at INTEGER NOT NULL, resolved_at INTEGER NOT NULL DEFAULT 0
);

`)
		if err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err := tx.Exec(`ALTER TABLE enrollments ADD COLUMN response_encrypted BLOB;
ALTER TABLE enrollments ADD COLUMN owner_id TEXT REFERENCES users(id);
UPDATE enrollments SET owner_id=(SELECT owner_id FROM nodes WHERE nodes.id=enrollments.node_id);
CREATE TABLE node_challenges (
 nonce_hash BLOB PRIMARY KEY, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 payload BLOB NOT NULL, expires_at INTEGER NOT NULL, used_at INTEGER NOT NULL DEFAULT 0
);
PRAGMA user_version=3;`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`ALTER TABLE grants ADD COLUMN last_control_success INTEGER NOT NULL DEFAULT 0; ALTER TABLE node_policies ADD COLUMN derper_usable INTEGER NOT NULL DEFAULT 0; PRAGMA user_version=4;`); err != nil {
		return err
	}
	return tx.Commit()
}
