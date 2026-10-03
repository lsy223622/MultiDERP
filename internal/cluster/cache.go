package cluster

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var cacheMu sync.Mutex

type cacheWatermark struct {
	ClusterID  string    `json:"cluster_id"`
	NodeID     string    `json:"node_id"`
	Revision   uint64    `json:"revision"`
	Digest     string    `json:"digest"`
	ObservedAt time.Time `json:"observed_at"`
}

func cacheDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func readCacheWatermark(path string) (cacheWatermark, error) {
	var m cacheWatermark
	f, err := os.Open(path + ".watermark")
	if err != nil {
		return m, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return m, errors.New("invalid cache watermark")
	}
	if decodeNodeJSON(b, &m) != nil || m.ClusterID == "" || m.NodeID == "" || m.Revision == 0 || !validNodeToken(m.Digest) || m.ObservedAt.IsZero() {
		return m, errors.New("invalid cache watermark")
	}
	return m, nil
}

func writeCacheFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".policy-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncCacheDirectory(filepath.Dir(path))
}

func SaveCache(path string, p Policy) error { return saveCacheAt(path, p, time.Now()) }

func saveCacheAt(path string, p Policy, now time.Time) error {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if path == "" || now.IsZero() {
		return errors.New("invalid cache location or clock")
	}
	if err := ValidatePolicy(p, p.ClusterID, p.NodeID); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil || len(b) > MaxPolicyBytes {
		return errors.New("invalid cache policy")
	}
	digest := cacheDigest(b)
	m, err := readCacheWatermark(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("cache watermark missing")
		}
		m = cacheWatermark{ClusterID: p.ClusterID, NodeID: p.NodeID, ObservedAt: now}
	} else if err != nil {
		return err
	}
	if m.ClusterID != p.ClusterID || m.NodeID != p.NodeID || p.Revision < m.Revision || (p.Revision == m.Revision && digest != m.Digest) {
		return errors.New("cache identity or revision conflict")
	}
	if now.Before(m.ObservedAt.Add(-5 * time.Second)) {
		return errors.New("node clock moved backwards")
	}
	if now.After(m.ObservedAt) {
		m.ObservedAt = now
	}
	m.Revision = p.Revision
	m.Digest = digest
	mark, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Commit the guard first: a crash between replacements must reject the older snapshot.
	if err := writeCacheFile(path+".watermark", mark); err != nil {
		return err
	}
	return writeCacheFile(path, b)
}

func LoadCache(path, clusterID, nodeID string, now time.Time) (Policy, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if now.IsZero() {
		return Policy{}, errors.New("invalid cache clock")
	}
	m, err := readCacheWatermark(path)
	if err != nil {
		return Policy{}, err
	}
	if m.ClusterID != clusterID || m.NodeID != nodeID || now.Before(m.ObservedAt.Add(-5*time.Second)) {
		return Policy{}, errors.New("cache binding or clock conflict")
	}
	f, err := os.Open(path)
	if err != nil {
		return Policy{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxPolicyBytes+1))
	if err != nil || len(b) > MaxPolicyBytes || cacheDigest(b) != m.Digest {
		return Policy{}, errors.New("cache snapshot does not match watermark")
	}
	p, err := DecodePolicy(bytes.NewReader(b), clusterID, nodeID)
	if err != nil || p.Revision != m.Revision {
		return Policy{}, errors.New("invalid cached policy")
	}
	if now.After(m.ObservedAt) {
		m.ObservedAt = now
		b, err := json.Marshal(m)
		if err != nil {
			return Policy{}, err
		}
		if err := writeCacheFile(path+".watermark", b); err != nil {
			return Policy{}, err
		}
	}
	return p, nil
}
