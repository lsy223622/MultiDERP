package control

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func registeredTestNode(t *testing.T) (*Store, Actor, Actor, Node, ed25519.PrivateKey, cluster.NodeSession) {
	t.Helper()
	s, admin, member := nodeTestStore(t)
	n, e, err := s.CreateNode(t.Context(), admin, "Relay", "relay.example.com")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c, err := s.EnrollmentChallenge(t.Context(), e.Code, pub, "original")
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.EnrollNode(t.Context(), cluster.EnrollmentRequest{Code: e.Code, Challenge: c, Signature: ed25519.Sign(priv, c.SigningBytes())})
	if err != nil {
		t.Fatal(err)
	}
	return s, admin, member, n, priv, session
}

func TestCloneDoesNotReplaceActiveInstance(t *testing.T) {
	s, admin, member, n, priv, session := registeredTestNode(t)
	c, err := s.SessionChallenge(t.Context(), n.ID, "clone")
	if err != nil {
		t.Fatal(err)
	}
	_, wrongKey, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := s.RenewNodeSession(t.Context(), c, ed25519.Sign(wrongKey, c.SigningBytes())); err == nil {
		t.Fatal("false proof accepted")
	}
	got, err := s.Node(t.Context(), admin, n.ID)
	if err != nil || got.State != "registered" {
		t.Fatal("unproved clone caused conflict")
	}
	if _, err := s.RenewNodeSession(t.Context(), c, ed25519.Sign(priv, c.SigningBytes())); !errors.Is(err, ErrConflict) {
		t.Fatal("clone not rejected", err)
	}
	got, err = s.Node(t.Context(), admin, n.ID)
	if err != nil || got.State != "identity_conflict" {
		t.Fatal("clone conflict not recorded")
	}
	var instance string
	s.db.QueryRow("SELECT instance_id FROM nodes WHERE id=?", n.ID).Scan(&instance)
	if instance != "original" {
		t.Fatal("clone replaced identity")
	}
	if _, err := s.AuthenticateNode(t.Context(), session.Token); err == nil {
		t.Fatal("conflicted identity still issues leases")
	}
	var count int
	s.db.QueryRow("SELECT count(*) FROM events WHERE resource_type='node' AND resource_id=? AND kind='identity_conflict' AND resolved_at=0", n.ID).Scan(&count)
	if count != 1 {
		t.Fatal("owner event absent")
	}
	if err := s.RecoverNodeInstance(t.Context(), member, n.ID, "clone"); !errors.Is(err, ErrForbidden) {
		t.Fatal("other owner recovered node")
	}
	if err := s.RecoverNodeInstance(t.Context(), admin, n.ID, "clone"); err != nil {
		t.Fatal(err)
	}
	c, err = s.SessionChallenge(t.Context(), n.ID, "clone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewNodeSession(t.Context(), c, ed25519.Sign(priv, c.SigningBytes())); err != nil {
		t.Fatal("selected instance not restored", err)
	}
}

func TestReconnectWithSameSessionIsAllowed(t *testing.T) {
	s, _, _, n, priv, session := registeredTestNode(t)
	for range 2 {
		if got, err := s.AuthenticateNode(t.Context(), session.Token); err != nil || got.NodeID != n.ID {
			t.Fatal("same session reconnect rejected")
		}
	}
	c, err := s.SessionChallenge(t.Context(), n.ID, "original")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewNodeSession(t.Context(), c, ed25519.Sign(priv, c.SigningBytes())); err != nil {
		t.Fatal("same instance renew rejected", err)
	}
}
