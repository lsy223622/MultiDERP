package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsy223622/MultiDERP/internal/config"
)

func TestRecoverPreparedRemoveWithOldConfigDiscardsOperation(t *testing.T) {
	oldConfig, newConfig, operation := recoveryOperation(t)
	configPath := filepath.Join(filepath.Dir(operation.StateRoot), "config.yaml")
	if err := config.WriteAtomic(configPath, oldConfig); err != nil {
		t.Fatalf("write old config: %v", err)
	}
	if err := os.MkdirAll(operation.StateDir, 0o700); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	if err := config.WriteRemoveOperation(filepath.Join(filepath.Dir(configPath), removeOperationFile), operation); err != nil {
		t.Fatalf("write operation: %v", err)
	}
	d := &Daemon{configPath: configPath, logf: func(string, ...any) {}}
	if err := d.recoverPendingOperationLocked(context.Background()); err != nil {
		t.Fatalf("recoverPendingOperationLocked() error = %v", err)
	}
	if _, err := os.Stat(operation.StateDir); err != nil {
		t.Fatalf("state directory after discarded operation: %v", err)
	}
	if _, err := os.Stat(d.removeOperationPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operation after discarded recovery: %v, want not-exist", err)
	}
	_ = newConfig
}

func TestRecoverPreparedRemoveWithNewConfigCompletesMove(t *testing.T) {
	oldConfig, newConfig, operation := recoveryOperation(t)
	configPath := filepath.Join(filepath.Dir(operation.StateRoot), "config.yaml")
	if err := config.WriteAtomic(configPath, newConfig); err != nil {
		t.Fatalf("write new config: %v", err)
	}
	if err := os.MkdirAll(operation.StateDir, 0o700); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(operation.StateDir, "marker"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("write state marker: %v", err)
	}
	if err := config.WriteRemoveOperation(dOperationPath(configPath), operation); err != nil {
		t.Fatalf("write operation: %v", err)
	}
	d := &Daemon{configPath: configPath, logf: func(string, ...any) {}}
	if err := d.recoverPendingOperationLocked(context.Background()); err != nil {
		t.Fatalf("recoverPendingOperationLocked() error = %v", err)
	}
	if _, err := os.Stat(operation.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state directory after recovery: %v, want not-exist", err)
	}
	if _, err := os.Stat(filepath.Join(operation.OrphanDir, "marker")); err != nil {
		t.Fatalf("moved state marker: %v", err)
	}
	metadata, err := config.ReadOrphanMetadata(operation.OrphanDir)
	if err != nil || metadata.ID != operation.Orphan.ID || metadata.Name != operation.Name {
		t.Fatalf("recovered orphan metadata = %#v, error = %v", metadata, err)
	}
	if _, err := os.Stat(d.removeOperationPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operation after recovery: %v, want not-exist", err)
	}
	_ = oldConfig
}

func TestRecoverConfigCommittedRemoveIsIdempotentAfterMove(t *testing.T) {
	oldConfig, newConfig, operation := recoveryOperation(t)
	operation.Phase = config.RemovePhaseConfigCommit
	configPath := filepath.Join(filepath.Dir(operation.StateRoot), "config.yaml")
	if err := config.WriteAtomic(configPath, newConfig); err != nil {
		t.Fatalf("write new config: %v", err)
	}
	if err := os.MkdirAll(operation.OrphanDir, 0o700); err != nil {
		t.Fatalf("create orphan directory: %v", err)
	}
	metadata := config.OrphanMetadata{ID: operation.Orphan.ID, Name: operation.Orphan.Name, CreatedAt: operation.Orphan.CreatedAt}
	if err := config.WriteOrphanMetadata(operation.OrphanDir, metadata); err != nil {
		t.Fatalf("write orphan metadata: %v", err)
	}
	if err := config.WriteRemoveOperation(dOperationPath(configPath), operation); err != nil {
		t.Fatalf("write operation: %v", err)
	}
	d := &Daemon{configPath: configPath, logf: func(string, ...any) {}}
	if err := d.recoverPendingOperationLocked(context.Background()); err != nil {
		t.Fatalf("recoverPendingOperationLocked() error = %v", err)
	}
	if _, err := os.Stat(operation.OrphanDir); err != nil {
		t.Fatalf("orphan directory after idempotent recovery: %v", err)
	}
	_ = oldConfig
}

func TestRecoverRemoveRejectsAmbiguousOrphanState(t *testing.T) {
	oldConfig, newConfig, operation := recoveryOperation(t)
	configPath := filepath.Join(filepath.Dir(operation.StateRoot), "config.yaml")
	if err := config.WriteAtomic(configPath, newConfig); err != nil {
		t.Fatalf("write new config: %v", err)
	}
	if err := os.MkdirAll(operation.StateDir, 0o700); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	if err := os.MkdirAll(operation.OrphanDir, 0o700); err != nil {
		t.Fatalf("create orphan directory: %v", err)
	}
	if err := config.WriteRemoveOperation(dOperationPath(configPath), operation); err != nil {
		t.Fatalf("write operation: %v", err)
	}
	d := &Daemon{configPath: configPath, logf: func(string, ...any) {}}
	err := d.recoverPendingOperationLocked(context.Background())
	if err == nil || !strings.Contains(err.Error(), "both verifier state and orphan directory exist") {
		t.Fatalf("ambiguous recovery error = %v", err)
	}
	if _, err := os.Stat(d.removeOperationPath()); err != nil {
		t.Fatalf("pending operation after ambiguous recovery: %v", err)
	}
	_ = oldConfig
}

func recoveryOperation(t *testing.T) (config.Config, config.Config, config.RemoveOperation) {
	t.Helper()
	root := t.TempDir()
	oldConfig := config.Default()
	oldConfig.Server.Hostname = "derp.example.com"
	oldConfig.Storage.StateDir = filepath.Join(root, "data")
	oldConfig.Storage.TailnetStateDir = filepath.Join(root, "tailnets")
	oldConfig.Storage.OrphanStateDir = filepath.Join(root, "orphans")
	oldConfig.Tailnets = []config.TailnetConfig{{Name: "alice", Auth: config.AuthConfig{Type: "web"}}}
	oldConfig.Normalize()
	newConfig := oldConfig.Clone()
	newConfig.Tailnets = nil
	id := "orphan-0123456789abcdef0123456789abcdef"
	created := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	return oldConfig, newConfig, config.RemoveOperation{
		Version:    config.RemoveOperationVersion,
		Phase:      config.RemovePhasePrepared,
		Name:       "alice",
		StateRoot:  oldConfig.Storage.TailnetStateDir,
		OrphanRoot: oldConfig.Storage.OrphanStateDir,
		StateDir:   filepath.Join(oldConfig.Storage.TailnetStateDir, "alice"),
		OrphanDir:  filepath.Join(oldConfig.Storage.OrphanStateDir, id),
		Orphan:     config.OrphanMetadata{ID: id, Name: "alice", CreatedAt: created},
		OldConfig:  oldConfig,
		NewConfig:  newConfig,
	}
}

func dOperationPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), removeOperationFile)
}
