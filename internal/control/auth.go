package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrUnauthorized = errors.New("authentication required")
	ErrRateLimited  = errors.New("too many requests")
	ErrForbidden    = errors.New("permission denied")
	ErrInvalid      = errors.New("invalid input")
	ErrConflict     = errors.New("conflicting operation")
)

type Actor struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Enabled  bool   `json:"enabled"`
}

func randomToken() string {
	var b [32]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func tokenHash(token string) []byte { h := sha256.Sum256([]byte(token)); return h[:] }

func RequireOwner(actor Actor, ownerID string) error {
	if !actor.Enabled || actor.ID == "" || (actor.Role != "admin" && actor.ID != ownerID) {
		return ErrForbidden
	}
	return nil
}

func passwordHash(password string) ([]byte, error) {
	if len(password) < 12 || len(password) > 72 {
		return nil, ErrInvalid
	}
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
}

func writeAudit(ctx context.Context, tx *sql.Tx, actorID, ownerID, kind, id, action string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO audit(actor_id,owner_id,resource_type,resource_id,action,created_at) VALUES(?,?,?,?,?,?)", actorID, ownerID, kind, id, action, time.Now().Unix())
	return err
}

func activeActor(ctx context.Context, tx *sql.Tx, actor Actor) error {
	if actor.ID == "local" {
		return nil
	}
	var role string
	if err := tx.QueryRowContext(ctx, "SELECT role FROM users WHERE id=? AND enabled=1", actor.ID).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrForbidden
		}
		return err
	}
	if role != actor.Role {
		return ErrForbidden
	}
	return nil
}

func (s *Store) InitializeAdmin(ctx context.Context, username, password string) (Actor, error) {
	return s.createUser(ctx, Actor{ID: "local", Role: "admin", Enabled: true}, username, password, "admin", true)
}

func (s *Store) hasAdmin(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE role='admin')").Scan(&exists)
	return exists, err
}

func (s *Store) CreateMember(ctx context.Context, actor Actor, username, password string) (Actor, error) {
	return s.CreateUser(ctx, actor, username, password, "member")
}

func (s *Store) CreateUser(ctx context.Context, actor Actor, username, password, role string) (Actor, error) {
	if !actor.Enabled || actor.Role != "admin" {
		return Actor{}, ErrForbidden
	}
	if role != "provider" && role != "member" {
		return Actor{}, ErrInvalid
	}
	return s.createUser(ctx, actor, username, password, role, false)
}

func (s *Store) SetUserRole(ctx context.Context, actor Actor, userID, role string) error {
	if !actor.Enabled || actor.Role != "admin" {
		return ErrForbidden
	}
	if role != "provider" && role != "member" {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return err
	}
	var current string
	if err := tx.QueryRowContext(ctx, "SELECT role FROM users WHERE id=?", userID).Scan(&current); err != nil {
		return err
	}
	if current == "admin" {
		return ErrForbidden
	}
	if current == role {
		return nil
	}
	if role == "member" {
		var ownsNodes bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM nodes WHERE owner_id=?)", userID).Scan(&ownsNodes); err != nil {
			return err
		}
		if ownsNodes {
			return ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET role=?,session_version=session_version+1 WHERE id=?", role, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=?", userID); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, userID, "user", userID, "user.role"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) createUser(ctx context.Context, actor Actor, username, password, role string, initial bool) (Actor, error) {
	if username == "" || len(username) > 80 || strings.TrimSpace(username) != username {
		return Actor{}, ErrInvalid
	}
	hash, err := passwordHash(password)
	if err != nil {
		return Actor{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Actor{}, err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return Actor{}, err
	}
	if initial {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE role='admin'").Scan(&count); err != nil {
			return Actor{}, err
		}
		if count != 0 {
			return Actor{}, ErrConflict
		}
	}
	a := Actor{ID: randomToken(), Username: username, Role: role, Enabled: true}
	if _, err := tx.ExecContext(ctx, "INSERT INTO users(id,username,role,enabled,password_hash) VALUES(?,?,?,1,?)", a.ID, a.Username, a.Role, hash); err != nil {
		return Actor{}, err
	}
	if err := writeAudit(ctx, tx, actor.ID, a.ID, "user", a.ID, "user.create"); err != nil {
		return Actor{}, err
	}
	return a, tx.Commit()
}

func (s *Store) Authenticate(ctx context.Context, token string) (Actor, error) {
	var a Actor
	if len(token) != 64 {
		return a, ErrUnauthorized
	}
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.role,u.enabled FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>? AND s.session_version=u.session_version AND u.enabled=1`, tokenHash(token), time.Now().Unix()).Scan(&a.ID, &a.Username, &a.Role, &a.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Actor{}, ErrUnauthorized
	}
	return a, err
}

func (s *Store) login(ctx context.Context, username, password string) (token, csrf string, err error) {
	var id string
	var hash []byte
	var version int64
	var enabled bool
	err = s.db.QueryRowContext(ctx, "SELECT id,password_hash,session_version,enabled FROM users WHERE username=?", username).Scan(&id, &hash, &version, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		bcrypt.CompareHashAndPassword([]byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"), []byte(password))
		return "", "", ErrUnauthorized
	}
	if err != nil {
		return "", "", err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || !enabled {
		return "", "", ErrUnauthorized
	}
	token = randomToken()
	csrf = hex.EncodeToString(tokenHash("uniderp-csrf:" + token))
	result, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,session_version,csrf_hash,expires_at) SELECT ?,id,session_version,?,? FROM users WHERE id=? AND enabled=1 AND session_version=?`, tokenHash(token), tokenHash(csrf), time.Now().Add(12*time.Hour).Unix(), id, version)
	if err != nil {
		return "", "", err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return "", "", ErrUnauthorized
	}
	return token, csrf, nil
}

func (s *Store) checkCSRF(ctx context.Context, token, csrf string) error {
	var hash []byte
	if csrf == "" {
		return ErrForbidden
	}
	if err := s.db.QueryRowContext(ctx, "SELECT csrf_hash FROM sessions WHERE token_hash=?", tokenHash(token)).Scan(&hash); err != nil {
		return ErrForbidden
	}
	if subtle.ConstantTimeCompare(hash, tokenHash(csrf)) != 1 {
		return ErrForbidden
	}
	return nil
}

func (s *Store) ChangePassword(ctx context.Context, actor Actor, userID, password string) error {
	if err := RequireOwner(actor, userID); err != nil {
		return err
	}
	hash, err := passwordHash(password)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE users SET password_hash=?,session_version=session_version+1 WHERE id=?", hash, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=?", userID); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, userID, "user", userID, "user.password"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetUserEnabled(ctx context.Context, actor Actor, userID string, enabled bool) error {
	if !actor.Enabled || actor.Role != "admin" {
		return ErrForbidden
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE users SET enabled=?,session_version=session_version+1 WHERE id=?", enabled, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=?", userID); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, userID, "user", userID, "user.enabled"); err != nil {
		return err
	}
	if err := s.rebuildPolicies(ctx, tx, s.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicies()
	return nil
}

func (s *Store) RecoverAdmin(ctx context.Context, userID, password string) error {
	var role string
	if err := s.db.QueryRowContext(ctx, "SELECT role FROM users WHERE id=?", userID).Scan(&role); err != nil {
		return err
	}
	if role != "admin" {
		return ErrForbidden
	}
	if err := s.ChangePassword(ctx, Actor{ID: "local", Role: "admin", Enabled: true}, userID, password); err != nil {
		return err
	}
	return s.SetUserEnabled(ctx, Actor{ID: "local", Role: "admin", Enabled: true}, userID, true)
}
