package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"multiderp/internal/config"
)

const removeOperationFile = ".multiderp-remove-operation.yaml"

func (d *Daemon) removeOperationPath() string {
	return filepath.Join(filepath.Dir(d.configPath), removeOperationFile)
}

func (d *Daemon) recoverPendingOperationLocked(ctx context.Context) error {
	path := d.removeOperationPath()
	operation, err := config.ReadRemoveOperation(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pending remove operation: %w", err)
	}
	if err := validateRecoveryPaths(operation); err != nil {
		return err
	}
	current, err := config.LoadFile(d.configPath)
	if err != nil {
		return fmt.Errorf("load config while recovering remove operation: %w", err)
	}
	operation.OldConfig.Normalize()
	operation.NewConfig.Normalize()
	if err := operation.OldConfig.Validate(); err != nil {
		return fmt.Errorf("validate pending remove old configuration: %w", err)
	}
	if err := operation.NewConfig.Validate(); err != nil {
		return fmt.Errorf("validate pending remove new configuration: %w", err)
	}
	switch operation.Phase {
	case config.RemovePhasePrepared:
		switch {
		case config.ConfigsEqual(current.Config, operation.OldConfig):
			if err := config.RemoveRemoveOperation(path); err != nil {
				return err
			}
			return nil
		case config.ConfigsEqual(current.Config, operation.NewConfig):
			operation.Phase = config.RemovePhaseConfigCommit
			if err := config.WriteRemoveOperation(path, operation); err != nil {
				return fmt.Errorf("advance pending remove operation: %w", err)
			}
		default:
			return errors.New("pending remove operation does not match the current configuration")
		}
	case config.RemovePhaseConfigCommit, config.RemovePhaseStateMoved:
		if !config.ConfigsEqual(current.Config, operation.NewConfig) {
			return errors.New("pending remove operation has an unexpected current configuration")
		}
	}

	if err := completeRemoveFiles(operation); err != nil {
		return fmt.Errorf("complete pending remove operation: %w", err)
	}
	if operation.Phase != config.RemovePhaseStateMoved {
		operation.Phase = config.RemovePhaseStateMoved
		if err := config.WriteRemoveOperation(path, operation); err != nil {
			return fmt.Errorf("record completed state move: %w", err)
		}
	}

	d.mu.RLock()
	started := d.started && !d.stopping
	d.mu.RUnlock()
	if started {
		if err := d.applyCommittedConfig(ctx, operation.NewConfig, nil); err != nil {
			return fmt.Errorf("reconcile recovered remove operation: %w", err)
		}
	}
	if err := config.RemoveRemoveOperation(path); err != nil {
		return err
	}
	d.logf("INFO recovered pending remove operation for %s", operation.Name)
	return nil
}

func validateRecoveryPaths(operation config.RemoveOperation) error {
	if !orphanIDPattern.MatchString(operation.Orphan.ID) {
		return errors.New("pending remove operation has an invalid orphan id")
	}
	if operation.Orphan.ID != filepath.Base(filepath.Clean(operation.OrphanDir)) {
		return errors.New("pending remove operation orphan id does not match its path")
	}
	if operation.Orphan.Name != operation.Name {
		return errors.New("pending remove operation name does not match orphan metadata")
	}
	expectedStateDir := filepath.Join(operation.StateRoot, operation.Name)
	expectedOrphanDir := filepath.Join(operation.OrphanRoot, operation.Orphan.ID)
	if filepath.Clean(operation.StateDir) != filepath.Clean(expectedStateDir) || filepath.Clean(operation.OrphanDir) != filepath.Clean(expectedOrphanDir) {
		return errors.New("pending remove operation paths do not match configured roots")
	}
	if !config.IsWithin(operation.StateRoot, operation.StateDir) || !config.IsWithin(operation.OrphanRoot, operation.OrphanDir) {
		return errors.New("pending remove operation path escaped configured roots")
	}
	return nil
}

func completeRemoveFiles(operation config.RemoveOperation) error {
	if err := ensurePrivateDir(operation.OrphanRoot); err != nil {
		return fmt.Errorf("protect orphan root: %w", err)
	}
	stateInfo, stateErr := os.Stat(operation.StateDir)
	orphanInfo, orphanErr := os.Stat(operation.OrphanDir)
	switch {
	case stateErr == nil && orphanErr == nil:
		return errors.New("both verifier state and orphan directory exist")
	case stateErr == nil:
		if !stateInfo.IsDir() {
			return errors.New("verifier state path is not a directory")
		}
		if orphanErr != nil && !errors.Is(orphanErr, os.ErrNotExist) {
			return fmt.Errorf("inspect orphan directory: %w", orphanErr)
		}
		if err := os.Rename(operation.StateDir, operation.OrphanDir); err != nil {
			return fmt.Errorf("move verifier state to orphan: %w", err)
		}
		orphanInfo, orphanErr = os.Stat(operation.OrphanDir)
		if orphanErr != nil {
			return fmt.Errorf("inspect moved orphan directory: %w", orphanErr)
		}
	case errors.Is(stateErr, os.ErrNotExist):
		if orphanErr != nil && !errors.Is(orphanErr, os.ErrNotExist) {
			return fmt.Errorf("inspect orphan directory: %w", orphanErr)
		}
		if errors.Is(orphanErr, os.ErrNotExist) {
			if err := os.MkdirAll(operation.OrphanDir, 0o700); err != nil {
				return fmt.Errorf("create empty orphan directory: %w", err)
			}
			orphanInfo, orphanErr = os.Stat(operation.OrphanDir)
			if orphanErr != nil {
				return fmt.Errorf("inspect empty orphan directory: %w", orphanErr)
			}
		}
	default:
		return fmt.Errorf("inspect verifier state: %w", stateErr)
	}
	if !orphanInfo.IsDir() {
		return errors.New("orphan path is not a directory")
	}
	if err := os.Chmod(operation.OrphanDir, 0o700); err != nil {
		return fmt.Errorf("protect orphan directory: %w", err)
	}
	want := config.OrphanMetadata{ID: operation.Orphan.ID, Name: operation.Orphan.Name, CreatedAt: operation.Orphan.CreatedAt}
	metadata, err := config.ReadOrphanMetadata(operation.OrphanDir)
	if errors.Is(err, os.ErrNotExist) {
		if err := config.WriteOrphanMetadata(operation.OrphanDir, want); err != nil {
			return fmt.Errorf("write orphan metadata: %w", err)
		}
		metadata = want
	} else if err != nil {
		return fmt.Errorf("read orphan metadata: %w", err)
	}
	if metadata.ID != want.ID || metadata.Name != want.Name || !metadata.CreatedAt.Equal(want.CreatedAt) {
		return errors.New("orphan metadata does not match pending remove operation")
	}
	if err := os.Chmod(config.OrphanMetadataPath(operation.OrphanDir), 0o600); err != nil {
		return fmt.Errorf("protect orphan metadata: %w", err)
	}
	return nil
}
