package derper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"tailscale.com/types/key"
)

type keyFile struct {
	PrivateKey key.NodePrivate
}

func EnsureKey(path string) error {
	if path == "" {
		return errors.New("derper key path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create derper key directory: %w", err)
	}
	if filepath.Clean(dir) != "." {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("set derper key directory permissions: %w", err)
		}
	}
	if data, err := os.ReadFile(path); err == nil {
		var stored keyFile
		if err := json.Unmarshal(data, &stored); err != nil {
			return fmt.Errorf("decode derper key file: %w", err)
		}
		if stored.PrivateKey.IsZero() {
			return errors.New("derper key file contains a zero private key")
		}
		return os.Chmod(path, 0o600)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read derper key file: %w", err)
	}

	data, err := json.MarshalIndent(keyFile{PrivateKey: key.NewNode()}, "", "\t")
	if err != nil {
		return fmt.Errorf("encode derper key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return EnsureKey(path)
		}
		return fmt.Errorf("create derper key file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write derper key: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("sync derper key: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close derper key: %w", err)
	}
	return nil
}
