package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const RemoveOperationVersion = 1

const (
	RemovePhasePrepared     = "prepared"
	RemovePhaseConfigCommit = "config_committed"
	RemovePhaseStateMoved   = "state_moved"
)

type RemoveOperation struct {
	Version    int            `yaml:"version"`
	Phase      string         `yaml:"phase"`
	Name       string         `yaml:"name"`
	StateRoot  string         `yaml:"state_root"`
	OrphanRoot string         `yaml:"orphan_root"`
	StateDir   string         `yaml:"state_dir"`
	OrphanDir  string         `yaml:"orphan_dir"`
	Orphan     OrphanMetadata `yaml:"orphan"`
	OldConfig  Config         `yaml:"old_config"`
	NewConfig  Config         `yaml:"new_config"`
}

func WriteRemoveOperation(path string, operation RemoveOperation) error {
	if err := validateRemoveOperation(operation); err != nil {
		return err
	}
	data, err := yaml.Marshal(operation)
	if err != nil {
		return fmt.Errorf("encode remove operation: %w", err)
	}
	return writeAtomicBytes(path, data, ".remove-operation.*.tmp")
}

func ReadRemoveOperation(path string) (RemoveOperation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RemoveOperation{}, err
	}
	var operation RemoveOperation
	if err := yaml.Unmarshal(data, &operation); err != nil {
		return RemoveOperation{}, fmt.Errorf("decode remove operation: %w", err)
	}
	if err := validateRemoveOperation(operation); err != nil {
		return RemoveOperation{}, err
	}
	return operation, nil
}

func RemoveRemoveOperation(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove pending operation: %w", err)
	}
	if dirFile, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func validateRemoveOperation(operation RemoveOperation) error {
	if operation.Version != RemoveOperationVersion {
		return fmt.Errorf("unsupported remove operation version %d; expected %d", operation.Version, RemoveOperationVersion)
	}
	switch operation.Phase {
	case RemovePhasePrepared, RemovePhaseConfigCommit, RemovePhaseStateMoved:
	default:
		return fmt.Errorf("unsupported remove operation phase %q", operation.Phase)
	}
	if operation.Name == "" || operation.StateRoot == "" || operation.OrphanRoot == "" || operation.StateDir == "" || operation.OrphanDir == "" {
		return errors.New("remove operation has incomplete paths or name")
	}
	if err := validateName(operation.Name); err != nil {
		return fmt.Errorf("remove operation name: %w", err)
	}
	if err := validateName(operation.Orphan.Name); err != nil {
		return fmt.Errorf("remove operation orphan name: %w", err)
	}
	if operation.Orphan.ID == "" || operation.Orphan.Name == "" || operation.Orphan.CreatedAt.IsZero() {
		return errors.New("remove operation has incomplete orphan metadata")
	}
	return nil
}
