package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

type Node struct {
	ID            string `json:"id"`
	OwnerID       string `json:"owner_id"`
	Domain        string `json:"domain"`
	DERPPort      int    `json:"derp_port"`
	STUNPort      int    `json:"stun_port"`
	DisplayName   string `json:"display_name"`
	RegionID      int    `json:"region_id"`
	State         string `json:"state"`
	LastHeartbeat int64  `json:"last_heartbeat"`
	LastError     string `json:"last_error"`
	Enabled       bool   `json:"enabled"`
}

type Enrollment struct {
	NodeID    string    `json:"node_id"`
	ClusterID string    `json:"cluster_id"`
	Domain    string    `json:"domain"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Store) ConfigureNodes(allowedCIDRs []string) error {
	var prefixes []netip.Prefix
	for _, text := range allowedCIDRs {
		p, err := netip.ParsePrefix(text)
		if err != nil {
			return ErrInvalid
		}
		prefixes = append(prefixes, p)
	}
	if s.credentialAEAD == nil {
		return ErrInvalid
	}
	if _, err := s.db.Exec("INSERT INTO settings(key,value) VALUES('cluster_id',?),('identity_retention_seconds','86400'),('control_retention_seconds','86400') ON CONFLICT(key) DO NOTHING", randomToken()); err != nil {
		return err
	}
	if err := s.db.QueryRow("SELECT value FROM settings WHERE key='cluster_id'").Scan(&s.clusterID); err != nil {
		return err
	}
	verifier := newDomainVerifier(prefixes)
	s.verifyDomain = verifier.verify
	s.probeDomain = verifier.probe
	s.nodeRequests = make(chan struct{}, 4)
	return nil
}

const nodeColumns = `id,owner_id,domain,derp_port,stun_port,display_name,region_id,state,last_heartbeat,last_error,enabled`

func scanNode(row interface{ Scan(...any) error }) (Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.OwnerID, &n.Domain, &n.DERPPort, &n.STUNPort, &n.DisplayName, &n.RegionID, &n.State, &n.LastHeartbeat, &n.LastError, &n.Enabled)
	return n, err
}

func (s *Store) Node(ctx context.Context, actor Actor, id string) (Node, error) {
	n, err := scanNode(s.db.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id=?", id))
	if err != nil {
		return Node{}, err
	}
	if err := RequireOwner(actor, n.OwnerID); err != nil {
		return Node{}, err
	}
	return n, nil
}

func (s *Store) ListNodes(ctx context.Context, actor Actor) ([]Node, error) {
	if actor.Role != "admin" && actor.Role != "provider" {
		return nil, ErrForbidden
	}
	if err := RequireOwner(actor, actor.ID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE owner_id=? OR ?='admin' ORDER BY display_name", actor.ID, actor.Role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	return items, rows.Err()
}

func (s *Store) issueEnrollment(ctx context.Context, tx *sql.Tx, n Node) (Enrollment, error) {
	e := Enrollment{NodeID: n.ID, ClusterID: s.clusterID, Domain: n.Domain, Code: randomToken(), ExpiresAt: s.now().Add(30 * time.Minute).UTC().Truncate(time.Second)}
	_, err := tx.ExecContext(ctx, `INSERT INTO enrollments(node_id,owner_id,code_hash,expires_at) VALUES(?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET owner_id=excluded.owner_id,code_hash=excluded.code_hash,expires_at=excluded.expires_at,consumed_at=0,request_hash=NULL,response_encrypted=NULL`, n.ID, n.OwnerID, tokenHash(e.Code), e.ExpiresAt.Unix())
	if err == nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM node_challenges WHERE node_id=?", n.ID)
	}
	return e, err
}

func (s *Store) CreateNode(ctx context.Context, actor Actor, name, domain string, derpPort, stunPort int) (Node, Enrollment, error) {
	if actor.Role != "admin" && actor.Role != "provider" {
		return Node{}, Enrollment{}, ErrForbidden
	}
	if s.clusterID == "" || strings.TrimSpace(name) == "" || len(name) > 160 || derpPort < 1 || derpPort > 65535 || stunPort < 1 || stunPort > 65535 {
		return Node{}, Enrollment{}, ErrInvalid
	}
	domain, err := normalizeNodeDomain(domain)
	if err != nil {
		return Node{}, Enrollment{}, &InputError{Code: "invalid_hostname", Field: "domain"}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Node{}, Enrollment{}, err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return Node{}, Enrollment{}, err
	}
	if err := RequireOwner(actor, actor.ID); err != nil {
		return Node{}, Enrollment{}, err
	}
	var region int
	if err := tx.QueryRowContext(ctx, `WITH RECURSIVE ids(n) AS (VALUES(900) UNION ALL SELECT n+1 FROM ids WHERE n<999) SELECT coalesce(min(n),0) FROM ids WHERE n NOT IN (SELECT region_id FROM nodes)`).Scan(&region); err != nil {
		return Node{}, Enrollment{}, err
	}
	if region == 0 {
		return Node{}, Enrollment{}, ErrConflict
	}
	n := Node{ID: randomToken(), OwnerID: actor.ID, Domain: domain, DERPPort: derpPort, STUNPort: stunPort, DisplayName: name, RegionID: region, State: "pending", Enabled: true}
	if _, err := tx.ExecContext(ctx, "INSERT INTO nodes(id,owner_id,domain,derp_port,stun_port,display_name,region_id) VALUES(?,?,?,?,?,?,?)", n.ID, n.OwnerID, n.Domain, n.DERPPort, n.STUNPort, n.DisplayName, n.RegionID); err != nil {
		return Node{}, Enrollment{}, conflictError(err)
	}
	e, err := s.issueEnrollment(ctx, tx, n)
	if err != nil {
		return Node{}, Enrollment{}, err
	}
	if err := writeAudit(ctx, tx, actor.ID, n.OwnerID, "node", n.ID, "node.create"); err != nil {
		return Node{}, Enrollment{}, err
	}
	return n, e, tx.Commit()
}

func (s *Store) IssueEnrollment(ctx context.Context, actor Actor, id string) (Enrollment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Enrollment{}, err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return Enrollment{}, err
	}
	n, err := scanNode(tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id=?", id))
	if err != nil {
		return Enrollment{}, err
	}
	if err := RequireOwner(actor, n.OwnerID); err != nil {
		return Enrollment{}, err
	}
	if n.State != "pending" {
		if err := s.releaseNode(ctx, tx, id); err != nil {
			return Enrollment{}, err
		}
	}
	e, err := s.issueEnrollment(ctx, tx, n)
	if err != nil {
		return Enrollment{}, err
	}
	if err := writeAudit(ctx, tx, actor.ID, n.OwnerID, "node", id, "enrollment.issue"); err != nil {
		return Enrollment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Enrollment{}, err
	}
	s.closeNodeStream(id)
	return e, nil
}

func validNodeInstance(instance string) bool {
	if len(instance) == 0 || len(instance) > 128 {
		return false
	}
	for _, c := range instance {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (s *Store) newNodeChallenge(ctx context.Context, tx *sql.Tx, purpose, id, domain string, pub []byte, instance string) (cluster.NodeChallenge, error) {
	c := cluster.NodeChallenge{Purpose: purpose, ClusterID: s.clusterID, NodeID: id, Domain: domain, PublicKey: pub, InstanceID: instance, Nonce: randomToken(), ExpiresAt: s.now().Add(2 * time.Minute).UTC().Truncate(time.Second)}
	if _, err := tx.ExecContext(ctx, "DELETE FROM node_challenges WHERE node_id=? AND (expires_at<=? OR used_at!=0)", id, s.now().Unix()); err != nil {
		return cluster.NodeChallenge{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM node_challenges WHERE node_id=?", id).Scan(&count); err != nil {
		return cluster.NodeChallenge{}, err
	}
	if count >= 8 {
		return cluster.NodeChallenge{}, ErrConflict
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO node_challenges(nonce_hash,node_id,payload,expires_at) VALUES(?,?,?,?)", tokenHash(c.Nonce), id, c.SigningBytes(), c.ExpiresAt.Unix())
	return c, err
}

func (s *Store) EnrollmentChallenge(ctx context.Context, code string, pub []byte, instance string) (cluster.NodeChallenge, error) {
	if len(code) != 64 || len(pub) != ed25519.PublicKeySize || !validNodeInstance(instance) || s.clusterID == "" {
		return cluster.NodeChallenge{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cluster.NodeChallenge{}, err
	}
	defer tx.Rollback()
	var id, domain string
	if err := tx.QueryRowContext(ctx, `SELECT n.id,n.domain FROM enrollments e JOIN nodes n ON n.id=e.node_id JOIN users u ON u.id=n.owner_id WHERE e.owner_id=n.owner_id AND e.code_hash=? AND e.expires_at>? AND e.consumed_at=0 AND n.state='pending' AND (n.public_key IS NULL OR n.public_key=?) AND u.enabled=1`, tokenHash(code), s.now().Unix(), pub).Scan(&id, &domain); err != nil {
		return cluster.NodeChallenge{}, ErrUnauthorized
	}
	c, err := s.newNodeChallenge(ctx, tx, "enroll", id, domain, pub, instance)
	if err != nil {
		return cluster.NodeChallenge{}, err
	}
	return c, tx.Commit()
}

func (s *Store) checkNodeChallenge(ctx context.Context, tx *sql.Tx, c cluster.NodeChallenge, sig []byte, purpose string) error {
	if c.ClusterID != s.clusterID || c.Purpose != purpose || len(c.PublicKey) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize || len(c.Nonce) != 64 || !s.now().Before(c.ExpiresAt) {
		return ErrUnauthorized
	}
	var payload []byte
	if err := tx.QueryRowContext(ctx, "SELECT payload FROM node_challenges WHERE nonce_hash=? AND node_id=? AND expires_at>? AND used_at=0", tokenHash(c.Nonce), c.NodeID, s.now().Unix()).Scan(&payload); err != nil {
		return ErrUnauthorized
	}
	if !bytes.Equal(payload, c.SigningBytes()) || !ed25519.Verify(c.PublicKey, payload, sig) {
		return ErrUnauthorized
	}
	return nil
}

func (s *Store) mintNodeSession(ctx context.Context, tx *sql.Tx, id, instance string) (cluster.NodeSession, error) {
	session := cluster.NodeSession{ClusterID: s.clusterID, NodeID: id, InstanceID: instance, Token: randomToken(), ExpiresAt: s.now().Add(time.Hour).UTC().Truncate(time.Second), LeaseUntil: s.now().Add(90 * time.Second).UTC().Truncate(time.Second)}
	if _, err := tx.ExecContext(ctx, "DELETE FROM node_sessions WHERE node_id=?", id); err != nil {
		return cluster.NodeSession{}, err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO node_sessions(token_hash,node_id,instance_id,expires_at) VALUES(?,?,?,?)", tokenHash(session.Token), id, instance, session.ExpiresAt.Unix())
	return session, err
}

func enrollmentDigest(req cluster.EnrollmentRequest) []byte {
	b, _ := json.Marshal(req)
	h := sha256.Sum256(b)
	return h[:]
}

func (s *Store) enrollmentReceipt(ctx context.Context, tx *sql.Tx, req cluster.EnrollmentRequest) (cluster.NodeSession, bool, error) {
	var hash, data []byte
	var consumed int64
	if err := tx.QueryRowContext(ctx, `SELECT e.consumed_at,e.request_hash,e.response_encrypted FROM enrollments e JOIN nodes n ON n.id=e.node_id JOIN users u ON u.id=n.owner_id WHERE e.owner_id=n.owner_id AND e.code_hash=? AND e.node_id=? AND e.expires_at>? AND u.enabled=1`, tokenHash(req.Code), req.Challenge.NodeID, s.now().Unix()).Scan(&consumed, &hash, &data); err != nil {
		return cluster.NodeSession{}, false, ErrUnauthorized
	}
	if consumed == 0 {
		return cluster.NodeSession{}, false, nil
	}
	if !bytes.Equal(hash, enrollmentDigest(req)) || s.credentialAEAD == nil || len(data) < s.credentialAEAD.NonceSize() {
		return cluster.NodeSession{}, true, ErrUnauthorized
	}
	n := s.credentialAEAD.NonceSize()
	plain, err := s.credentialAEAD.Open(nil, data[:n], data[n:], []byte("enrollment:"+req.Challenge.NodeID))
	var session cluster.NodeSession
	if err != nil || json.Unmarshal(plain, &session) != nil || !s.now().Before(session.ExpiresAt) {
		return cluster.NodeSession{}, true, ErrUnauthorized
	}
	return session, true, nil
}

func (s *Store) EnrollNode(ctx context.Context, req cluster.EnrollmentRequest) (cluster.NodeSession, error) {
	if s.verifyDomain == nil || s.nodeRequests == nil || len(req.Code) != 64 {
		return cluster.NodeSession{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cluster.NodeSession{}, err
	}
	session, done, err := s.enrollmentReceipt(ctx, tx, req)
	if err == nil && !done {
		err = s.checkNodeChallenge(ctx, tx, req.Challenge, req.Signature, "enroll")
	}
	tx.Rollback()
	if err != nil || done {
		return session, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	select {
	case s.nodeRequests <- struct{}{}:
		defer func() { <-s.nodeRequests }()
	case <-ctx.Done():
		return cluster.NodeSession{}, ErrUnauthorized
	}
	if err := s.verifyDomain(ctx, req.Challenge); err != nil {
		return cluster.NodeSession{}, ErrUnauthorized
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return cluster.NodeSession{}, err
	}
	defer tx.Rollback()
	session, done, err = s.enrollmentReceipt(ctx, tx, req)
	if err != nil || done {
		return session, err
	}
	if err := s.checkNodeChallenge(ctx, tx, req.Challenge, req.Signature, "enroll"); err != nil {
		return cluster.NodeSession{}, err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT owner_id FROM nodes WHERE id=? AND domain=? AND state='pending' AND (public_key IS NULL OR public_key=?)", req.Challenge.NodeID, req.Challenge.Domain, req.Challenge.PublicKey).Scan(&owner); err != nil {
		return cluster.NodeSession{}, ErrUnauthorized
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET public_key=?,instance_id=?,state='registered',lease_until=?,domain_verified_at=? WHERE id=?", req.Challenge.PublicKey, req.Challenge.InstanceID, s.now().Add(90*time.Second).Unix(), s.now().Unix(), req.Challenge.NodeID); err != nil {
		return cluster.NodeSession{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO node_policies(node_id) VALUES(?) ON CONFLICT(node_id) DO NOTHING", req.Challenge.NodeID); err != nil {
		return cluster.NodeSession{}, err
	}
	if _, err := s.buildPolicy(ctx, tx, req.Challenge.NodeID, s.now()); err != nil {
		return cluster.NodeSession{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE node_challenges SET used_at=? WHERE nonce_hash=?", s.now().Unix(), tokenHash(req.Challenge.Nonce)); err != nil {
		return cluster.NodeSession{}, err
	}
	session, err = s.mintNodeSession(ctx, tx, req.Challenge.NodeID, req.Challenge.InstanceID)
	if err != nil {
		return cluster.NodeSession{}, err
	}
	plain, _ := json.Marshal(session)
	nonce := make([]byte, s.credentialAEAD.NonceSize())
	rand.Read(nonce)
	data := s.credentialAEAD.Seal(nonce, nonce, plain, []byte("enrollment:"+session.NodeID))
	if _, err := tx.ExecContext(ctx, "UPDATE enrollments SET consumed_at=?,request_hash=?,response_encrypted=? WHERE node_id=?", s.now().Unix(), enrollmentDigest(req), data, session.NodeID); err != nil {
		return cluster.NodeSession{}, err
	}
	if err := writeAudit(ctx, tx, "node:"+session.NodeID, owner, "node", session.NodeID, "node.enroll"); err != nil {
		return cluster.NodeSession{}, err
	}
	return session, tx.Commit()
}

func (s *Store) SessionChallenge(ctx context.Context, id, instance string) (cluster.NodeChallenge, error) {
	if !validNodeInstance(instance) || s.clusterID == "" {
		return cluster.NodeChallenge{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cluster.NodeChallenge{}, err
	}
	defer tx.Rollback()
	var domain, state string
	var pub []byte
	if err := tx.QueryRowContext(ctx, `SELECT n.domain,n.public_key,n.state FROM nodes n JOIN users u ON u.id=n.owner_id WHERE n.id=? AND n.state IN ('registered','ready','offline','domain_pending') AND u.enabled=1`, id).Scan(&domain, &pub, &state); err != nil || len(pub) != ed25519.PublicKeySize {
		return cluster.NodeChallenge{}, ErrUnauthorized
	}
	purpose := "session"
	if state == "domain_pending" {
		purpose = "domain_change"
	}
	c, err := s.newNodeChallenge(ctx, tx, purpose, id, domain, pub, instance)
	if err != nil {
		return cluster.NodeChallenge{}, err
	}
	return c, tx.Commit()
}

func (s *Store) RenewNodeSession(ctx context.Context, c cluster.NodeChallenge, sig []byte) (cluster.NodeSession, error) {
	if c.Purpose != "session" && c.Purpose != "domain_change" {
		return cluster.NodeSession{}, ErrUnauthorized
	}
	if c.Purpose == "domain_change" {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return cluster.NodeSession{}, err
		}
		err = s.checkNodeChallenge(ctx, tx, c, sig, "domain_change")
		if err == nil {
			var count int
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM nodes n JOIN users u ON u.id=n.owner_id WHERE n.id=? AND n.domain=? AND n.public_key=? AND n.state='domain_pending' AND u.enabled=1`, c.NodeID, c.Domain, c.PublicKey).Scan(&count)
			if err == nil && count != 1 {
				err = ErrUnauthorized
			}
		}
		tx.Rollback()
		if err != nil {
			return cluster.NodeSession{}, err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		select {
		case s.nodeRequests <- struct{}{}:
			defer func() { <-s.nodeRequests }()
		case <-probeCtx.Done():
			return cluster.NodeSession{}, ErrUnauthorized
		}
		if err := s.verifyDomain(probeCtx, c); err != nil {
			return cluster.NodeSession{}, ErrUnauthorized
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cluster.NodeSession{}, err
	}
	defer tx.Rollback()
	if err := s.checkNodeChallenge(ctx, tx, c, sig, c.Purpose); err != nil {
		return cluster.NodeSession{}, err
	}
	var pub []byte
	var instance string
	var owner string
	var lease int64
	if err := tx.QueryRowContext(ctx, `SELECT n.public_key,n.instance_id,n.lease_until,n.owner_id FROM nodes n JOIN users u ON u.id=n.owner_id WHERE n.id=? AND n.domain=? AND ((?='session' AND n.state IN ('registered','ready','offline')) OR (?='domain_change' AND n.state='domain_pending')) AND u.enabled=1`, c.NodeID, c.Domain, c.Purpose, c.Purpose).Scan(&pub, &instance, &lease, &owner); err != nil || !bytes.Equal(pub, c.PublicKey) {
		return cluster.NodeSession{}, ErrUnauthorized
	}
	if instance != c.InstanceID && lease > s.now().Unix() {
		if _, err := tx.ExecContext(ctx, "UPDATE nodes SET state='identity_conflict',last_error='node identity is active on another instance' WHERE id=?", c.NodeID); err != nil {
			return cluster.NodeSession{}, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM node_sessions WHERE node_id=?", c.NodeID); err != nil {
			return cluster.NodeSession{}, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE node_challenges SET used_at=? WHERE nonce_hash=?", s.now().Unix(), tokenHash(c.Nonce)); err != nil {
			return cluster.NodeSession{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO events(owner_id,resource_type,resource_id,kind,message,created_at) VALUES(?,'node',?,'identity_conflict','Node identity was presented by another running instance.',?)", owner, c.NodeID, s.now().Unix()); err != nil {
			return cluster.NodeSession{}, err
		}
		if err := writeAudit(ctx, tx, "node:"+c.NodeID, owner, "node", c.NodeID, "node.identity_conflict"); err != nil {
			return cluster.NodeSession{}, err
		}
		if _, err := s.buildPolicy(ctx, tx, c.NodeID, s.now()); err != nil {
			return cluster.NodeSession{}, err
		}
		if err := tx.Commit(); err != nil {
			return cluster.NodeSession{}, err
		}
		s.notifyPolicy(c.NodeID)
		return cluster.NodeSession{}, errors.Join(ErrConflict, cluster.ErrIdentityConflict)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE node_challenges SET used_at=? WHERE nonce_hash=?", s.now().Unix(), tokenHash(c.Nonce)); err != nil {
		return cluster.NodeSession{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET instance_id=?,lease_until=?,state=CASE WHEN state='domain_pending' THEN 'registered' ELSE state END WHERE id=?", c.InstanceID, s.now().Add(90*time.Second).Unix(), c.NodeID); err != nil {
		return cluster.NodeSession{}, err
	}
	if c.Purpose == "domain_change" {
		if _, err := tx.ExecContext(ctx, "UPDATE nodes SET domain_verified_at=? WHERE id=?", s.now().Unix(), c.NodeID); err != nil {
			return cluster.NodeSession{}, err
		}
		if err := writeAudit(ctx, tx, "node:"+c.NodeID, owner, "node", c.NodeID, "node.domain.verify"); err != nil {
			return cluster.NodeSession{}, err
		}
	}
	session, err := s.mintNodeSession(ctx, tx, c.NodeID, c.InstanceID)
	if err != nil {
		return cluster.NodeSession{}, err
	}
	return session, tx.Commit()
}

func (s *Store) RecoverNodeInstance(ctx context.Context, actor Actor, id, instance string) error {
	if !validNodeInstance(instance) {
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
	n, err := scanNode(tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id=?", id))
	if err != nil {
		return err
	}
	if err := RequireOwner(actor, n.OwnerID); err != nil {
		return err
	}
	if n.State != "identity_conflict" {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, "UPDATE nodes SET state='registered',instance_id=?,lease_until=?,last_error='' WHERE id=?", instance, s.now().Add(90*time.Second).Unix(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM node_sessions WHERE node_id=?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM node_challenges WHERE node_id=?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE events SET resolved_at=? WHERE resource_type='node' AND resource_id=? AND kind='identity_conflict' AND resolved_at=0", s.now().Unix(), id); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, n.OwnerID, "node", id, "node.recover"); err != nil {
		return err
	}
	if _, err := s.buildPolicy(ctx, tx, id, s.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicy(id)
	return nil
}

func (s *Store) AuthenticateNode(ctx context.Context, token string) (cluster.NodeSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cluster.NodeSession{}, err
	}
	defer tx.Rollback()
	return s.nodeSession(ctx, tx, token)
}

func (s *Store) nodeSession(ctx context.Context, tx *sql.Tx, token string) (cluster.NodeSession, error) {
	var session cluster.NodeSession
	var expires, lease int64
	if len(token) != 64 {
		return session, ErrUnauthorized
	}
	if _, err := hex.DecodeString(token); err != nil {
		return session, ErrUnauthorized
	}
	err := tx.QueryRowContext(ctx, `SELECT n.id,n.instance_id,s.expires_at,n.lease_until FROM node_sessions s JOIN nodes n ON n.id=s.node_id JOIN users u ON u.id=n.owner_id WHERE s.token_hash=? AND s.expires_at>? AND s.instance_id=n.instance_id AND n.state IN ('registered','ready','offline') AND u.enabled=1`, tokenHash(token), s.now().Unix()).Scan(&session.NodeID, &session.InstanceID, &expires, &lease)
	if err != nil {
		return cluster.NodeSession{}, ErrUnauthorized
	}
	session.ClusterID = s.clusterID
	session.ExpiresAt = time.Unix(expires, 0).UTC()
	session.LeaseUntil = time.Unix(lease, 0).UTC()
	return session, nil
}
