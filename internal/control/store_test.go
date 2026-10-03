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
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
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
