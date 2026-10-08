package control

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStoreMigrationTransactionAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 7 {
		t.Fatalf("migration = %d, %v", version, err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO users(id,username,role,enabled,password_hash,session_version) VALUES('a','alice','member',1,'hash',1)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM users").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback = %d %v", count, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.db.QueryRow("SELECT count(*) FROM users").Scan(&count); err != nil || count != 0 {
		t.Fatalf("reopen = %d %v", count, err)
	}
}

func TestStoreMigratesExistingAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := s.InitializeAdmin(t.Context(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`ALTER TABLE nodes DROP COLUMN derp_port; ALTER TABLE nodes DROP COLUMN stun_port; DROP TABLE node_observations; ALTER TABLE nodes DROP COLUMN enabled; ALTER TABLE nodes DROP COLUMN domain_verified_at; ALTER TABLE node_policies DROP COLUMN derper_usable; ALTER TABLE grants DROP COLUMN last_control_success; DROP TABLE node_challenges; ALTER TABLE enrollments DROP COLUMN response_encrypted; ALTER TABLE enrollments DROP COLUMN owner_id; ALTER TABLE credentials DROP COLUMN revision; ALTER TABLE credentials DROP COLUMN refresh_seq; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	token, _, err := s.login(t.Context(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.Authenticate(t.Context(), token)
	if err != nil || actor.ID != admin.ID {
		t.Fatal("migration lost existing account")
	}
	if _, err := s.db.Exec("SELECT revision,refresh_seq FROM credentials"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreMigratesObservationsFromVersionFour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitializeAdmin(t.Context(), "admin", testPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ALTER TABLE nodes DROP COLUMN derp_port; ALTER TABLE nodes DROP COLUMN stun_port; DROP TABLE node_observations; ALTER TABLE nodes DROP COLUMN enabled; ALTER TABLE nodes DROP COLUMN domain_verified_at; PRAGMA user_version=4;"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.login(t.Context(), "admin", testPassword); err != nil {
		t.Fatal("schema4 migration lost account", err)
	}
	if _, err := s.db.Exec("SELECT report_json,probes_json FROM node_observations"); err != nil {
		t.Fatal(err)
	}
}

func TestNodeMigrationFailureRollsBack(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`ALTER TABLE enrollments DROP COLUMN response_encrypted; ALTER TABLE enrollments DROP COLUMN owner_id; PRAGMA user_version=2;`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err == nil {
		t.Fatal("conflicting schema migrated")
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatal("failed migration advanced version")
	}
	if _, err := s.db.Exec("SELECT response_encrypted FROM enrollments"); err == nil {
		t.Fatal("failed migration left column behind")
	}
}
