package control

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"tailscale.com/types/key"
	"testing"
	"time"
)

func TestNodeReleaseRevokesSessionGrantsAndReceipt(t *testing.T) {
	s, admin, member, n, priv, session := registeredTestNode(t)
	tn := randomToken()
	policyTailnet(t, s, admin.ID, tn, []identityKey{{NodePublic: key.NewNode().Public().String()}}, s.now())
	g, err := s.RequestGrant(t.Context(), admin, n.ID, tn, 0, time.Time{})
	if err != nil || g.State != "active" {
		t.Fatal(g, err)
	}
	challenge, err := s.SessionChallenge(t.Context(), n.ID, "original")
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseNode(t.Context(), session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateNode(t.Context(), session.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := s.RenewNodeSession(t.Context(), challenge, ed25519.Sign(priv, challenge.SigningBytes())); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := s.IssueEnrollment(t.Context(), member, n.ID); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	got, err := s.Node(t.Context(), admin, n.ID)
	if err != nil || got.State != "pending" {
		t.Fatal(got, err)
	}
	var pub []byte
	var receipts int
	s.db.QueryRow("SELECT public_key FROM nodes WHERE id=?", n.ID).Scan(&pub)
	s.db.QueryRow("SELECT count(*) FROM enrollments WHERE node_id=?", n.ID).Scan(&receipts)
	if !bytes.Equal(pub, priv.Public().(ed25519.PublicKey)) || receipts != 0 {
		t.Fatal("identity or receipt", receipts)
	}
	revoked, err := s.Grant(t.Context(), admin, g.ID)
	if err != nil || revoked.State != "revoked" || revoked.Revision <= g.Revision {
		t.Fatal(revoked, err)
	}
	after, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil || after.Revision <= before.Revision || len(after.Grants) != 0 {
		t.Fatal(after, err)
	}
}

func TestReenrollmentRequiresOriginalKeyAndNewConsent(t *testing.T) {
	s, admin, _, n, priv, old := registeredTestNode(t)
	before, _ := s.currentPolicy(t.Context(), n.ID)
	e, err := s.IssueEnrollment(t.Context(), admin, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateNode(t.Context(), old.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	wrong, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := s.EnrollmentChallenge(t.Context(), e.Code, wrong, "new"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("other key accepted", err)
	}
	c, err := s.EnrollmentChallenge(t.Context(), e.Code, priv.Public().(ed25519.PublicKey), "new")
	if err != nil {
		t.Fatal(err)
	}
	req := cluster.EnrollmentRequest{Code: e.Code, Challenge: c, Signature: ed25519.Sign(priv, c.SigningBytes())}
	if _, err := s.EnrollNode(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	after, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil || after.Revision <= before.Revision || len(after.Grants) != 0 {
		t.Fatal(after, err)
	}
}
