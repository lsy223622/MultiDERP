package control

import (
	"context"
	"crypto/cipher"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"

	_ "modernc.org/sqlite"
)

type Store struct {
	db               *sql.DB
	credentialAEAD   cipher.AEAD
	apiClient        *http.Client
	apiBase          string
	identityRequests chan struct{}
	now              func() time.Time
	clusterID        string
	verifyDomain     func(context.Context, cluster.NodeChallenge) error
	nodeRequests     chan struct{}
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("controller database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	if filepath.VolumeName(abs) != "" {
		u.Path = "/" + u.Path
	}
	q := u.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now, identityRequests: make(chan struct{}, 4)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
