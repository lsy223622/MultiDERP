package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func nodeTestStore(t *testing.T) (*Store, Actor, Actor) {
	t.Helper()
	s, _, admin, member := accountTestServer(t)
	if err := s.EnableIdentity(filepath.Join(t.TempDir(), "controller.key")); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureNodes(nil); err != nil {
		t.Fatal(err)
	}
	s.verifyDomain = func(context.Context, cluster.NodeChallenge) error { return nil }
	return s, admin, member
}

func TestEnrollmentExpiryAndDisabledOwnerCannotActivate(t *testing.T) {
	s, admin, member := nodeTestStore(t)
	_, e, err := s.CreateNode(t.Context(), member, "Relay", "relay.example.com", 443, 3478)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c, err := s.EnrollmentChallenge(t.Context(), e.Code, pub, "instance")
	if err != nil {
		t.Fatal(err)
	}
	req := cluster.EnrollmentRequest{Code: e.Code, Challenge: c, Signature: ed25519.Sign(priv, c.SigningBytes())}
	s.verifyDomain = func(context.Context, cluster.NodeChallenge) error {
		return s.SetUserEnabled(t.Context(), admin, member.ID, false)
	}
	if _, err := s.EnrollNode(t.Context(), req); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("disabled owner registered", err)
	}
	if err := s.SetUserEnabled(t.Context(), admin, member.ID, true); err != nil {
		t.Fatal(err)
	}
	s.verifyDomain = func(context.Context, cluster.NodeChallenge) error { return nil }
	s.now = func() time.Time { return c.ExpiresAt.Add(time.Second) }
	if _, err := s.EnrollNode(t.Context(), req); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired proof accepted", err)
	}
	s.now = func() time.Time { return e.ExpiresAt.Add(time.Second) }
	if _, err := s.EnrollmentChallenge(t.Context(), e.Code, pub, "instance"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired code accepted", err)
	}
}

func TestEnrollmentRemainsBoundToIssuingOwner(t *testing.T) {
	s, admin, member := nodeTestStore(t)
	n, e, err := s.CreateNode(t.Context(), admin, "Relay", "relay.example.com", 443, 3478)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE nodes SET owner_id=? WHERE id=?", member.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := s.EnrollmentChallenge(t.Context(), e.Code, pub, "instance"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old owner code survived owner change", err)
	}
}

func TestNodeOwnershipCanonicalDomainAndEnrollmentRotation(t *testing.T) {
	s, admin, member := nodeTestStore(t)
	node, enrollment, err := s.CreateNode(t.Context(), admin, "Relay", "Relay.Example.COM.", 443, 3478)
	if err != nil {
		t.Fatal(err)
	}
	if node.Domain != "relay.example.com" || node.OwnerID != admin.ID || node.RegionID < 900 || node.RegionID > 999 || enrollment.Code == "" {
		t.Fatal("node not bound")
	}
	if _, _, err := s.CreateNode(t.Context(), member, "Duplicate", "relay.example.com", 443, 3478); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate domain accepted")
	}
	if _, err := s.Node(t.Context(), member, node.ID); !errors.Is(err, ErrForbidden) {
		t.Fatal("other owner read node")
	}
	if _, err := s.IssueEnrollment(t.Context(), member, node.ID); !errors.Is(err, ErrForbidden) {
		t.Fatal("other owner issued enrollment")
	}
	next, err := s.IssueEnrollment(t.Context(), admin, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := s.EnrollmentChallenge(t.Context(), enrollment.Code, pub, "instance"); err == nil {
		t.Fatal("rotated code accepted")
	}
	if _, err := s.EnrollmentChallenge(t.Context(), next.Code, pub, "instance"); err != nil {
		t.Fatal(err)
	}
	var hash []byte
	s.db.QueryRow("SELECT code_hash FROM enrollments WHERE node_id=?", node.ID).Scan(&hash)
	if string(hash) == next.Code {
		t.Fatal("plaintext code stored")
	}
}

func TestEnrollProvesKeyBindsRequestAndRetriesIdempotently(t *testing.T) {
	s, admin, _ := nodeTestStore(t)
	node, e, err := s.CreateNode(t.Context(), admin, "Relay", "relay.example.com", 443, 3478)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c, err := s.EnrollmentChallenge(t.Context(), e.Code, pub, "instance")
	if err != nil {
		t.Fatal(err)
	}
	req := cluster.EnrollmentRequest{Code: e.Code, Challenge: c, Signature: ed25519.Sign(priv, c.SigningBytes())}
	bad := req
	bad.Challenge.NodeID = "other"
	if _, err := s.EnrollNode(t.Context(), bad); err == nil {
		t.Fatal("wrong node accepted")
	}
	session, err := s.EnrollNode(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.EnrollNode(t.Context(), req)
	if err != nil || again.Token != session.Token || again.NodeID != node.ID {
		t.Fatal("retry not idempotent")
	}
	got, err := s.Node(t.Context(), admin, node.ID)
	if err != nil || got.State != "registered" {
		t.Fatal("registration status incorrect")
	}
	authenticated, err := s.AuthenticateNode(t.Context(), session.Token)
	if err != nil || authenticated.NodeID != node.ID {
		t.Fatal("node session not scoped")
	}
	if _, err := s.Authenticate(t.Context(), session.Token); err == nil {
		t.Fatal("node session became browser session")
	}
	var count int
	s.db.QueryRow("SELECT count(*) FROM nodes").Scan(&count)
	if count != 1 {
		t.Fatal("retry duplicated node")
	}
}

func TestEnrollConcurrentDifferentIdentitiesOnlyOneWins(t *testing.T) {
	s, admin, _ := nodeTestStore(t)
	_, e, err := s.CreateNode(t.Context(), admin, "Relay", "relay.example.com", 443, 3478)
	if err != nil {
		t.Fatal(err)
	}
	var requests []cluster.EnrollmentRequest
	for _, instance := range []string{"first", "second"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		c, err := s.EnrollmentChallenge(t.Context(), e.Code, pub, instance)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, cluster.EnrollmentRequest{Code: e.Code, Challenge: c, Signature: ed25519.Sign(priv, c.SigningBytes())})
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, req := range requests {
		wg.Go(func() { _, err := s.EnrollNode(t.Context(), req); results <- err })
	}
	wg.Wait()
	wins := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("wins %d", wins)
	}
}

func TestNodeSessionChallengeRejectsReplayAndKeyChange(t *testing.T) {
	s, admin, _ := nodeTestStore(t)
	node, e, err := s.CreateNode(t.Context(), admin, "Relay", "relay.example.com", 443, 3478)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c, err := s.EnrollmentChallenge(t.Context(), e.Code, pub, "instance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollNode(t.Context(), cluster.EnrollmentRequest{Code: e.Code, Challenge: c, Signature: ed25519.Sign(priv, c.SigningBytes())}); err != nil {
		t.Fatal(err)
	}
	c, err = s.SessionChallenge(t.Context(), node.ID, "instance")
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := s.RenewNodeSession(t.Context(), c, ed25519.Sign(otherPriv, c.SigningBytes())); err == nil {
		t.Fatal("changed private key accepted")
	}
	if _, err := s.RenewNodeSession(t.Context(), c, ed25519.Sign(priv, c.SigningBytes())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewNodeSession(t.Context(), c, ed25519.Sign(priv, c.SigningBytes())); err == nil {
		t.Fatal("nonce replay accepted")
	}
}
