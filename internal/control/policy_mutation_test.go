package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"tailscale.com/types/key"
)

func TestPolicyAccountDisableAndRestorePreserveDeadlines(t *testing.T) {
	s, provider, applicant, n, tn := grantFixture(t)
	g, err := s.RequestGrant(t.Context(), applicant, n.ID, tn, 0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	g, err = s.ApplyGrantAction(t.Context(), provider, g.ID, g.Revision, GrantApprove)
	if err != nil {
		t.Fatal(err)
	}
	g, err = s.ApplyGrantAction(t.Context(), applicant, g.ID, g.Revision, GrantConfirm)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserEnabled(t.Context(), provider, applicant.ID, false); err != nil {
		t.Fatal(err)
	}
	disabled, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil || len(disabled.Grants) != 0 || disabled.Revision <= initial.Revision {
		t.Fatal("disabled account retained service policy", err)
	}
	if err := s.SetUserEnabled(t.Context(), provider, applicant.ID, true); err != nil {
		t.Fatal(err)
	}
	restored, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil || len(restored.Grants) != 1 || !restored.Grants[0].IdentityUntil.Equal(initial.Grants[0].IdentityUntil) || !restored.Grants[0].ControlUntil.Equal(initial.Grants[0].ControlUntil) {
		t.Fatal("account restore renewed old cache", err)
	}
	if _, err := s.ApplyGrantAction(t.Context(), provider, g.ID, g.Revision, GrantRevoke); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserEnabled(t.Context(), provider, applicant.ID, true); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil || len(revoked.Grants) != 0 {
		t.Fatal("restore revived revoked grant", err)
	}
}

func TestPolicyIdentityRefreshAndCredentialDeletionPublishAtomically(t *testing.T) {
	s, stub, admin := credentialTestStore(t)
	if err := s.ConfigureNodes(nil); err != nil {
		t.Fatal(err)
	}
	s.verifyDomain = func(context.Context, cluster.NodeChallenge) error { return nil }
	n, enrollment, err := s.CreateNode(t.Context(), admin, "Relay", "relay.example.com", 443, 3478)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	challenge, err := s.EnrollmentChallenge(t.Context(), enrollment.Code, pub, "original")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollNode(t.Context(), cluster.EnrollmentRequest{Code: enrollment.Code, Challenge: challenge, Signature: ed25519.Sign(priv, challenge.SigningBytes())}); err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := key.NewNode().Public(), key.NewNode().Public()
	stub.body = `{"devices":[` + identityFixture(oldKey) + `]}`
	tn, err := s.AddTailnet(t.Context(), admin, "Own", "Tidentity123", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestGrant(t.Context(), admin, n.ID, tn.ID, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	initial, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.body = `{"devices":[` + identityFixture(newKey) + `]}`
	stub.mu.Unlock()
	if err := s.RefreshIdentity(t.Context(), tn.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil || updated.Revision <= initial.Revision || len(updated.Grants[0].Keys) != 1 || updated.Grants[0].Keys[0].NodePublic != newKey.String() {
		t.Fatal("successful refresh not published to node policy", err)
	}
	if err := s.DeleteCredential(t.Context(), admin, tn.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil || len(deleted.Grants[0].Keys) != 0 || deleted.Revision <= updated.Revision {
		t.Fatal("credential deletion not published", err)
	}
}

func TestPolicyTailnetTransferUpdatesOwnerGroupWithoutRenewal(t *testing.T) {
	s, provider, recipient, n, tn := grantFixture(t)
	if _, err := s.db.Exec("UPDATE tailnets SET owner_id=? WHERE id=?", provider.ID, tn); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestGrant(t.Context(), provider, n.ID, tn, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	q, err := s.NodeQoS(t.Context(), provider, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeQoS(t.Context(), provider, n.ID, q); err != nil {
		t.Fatal(err)
	}
	initial, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TransferTailnet(t.Context(), provider, tn, recipient.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil || len(updated.Grants) != 1 || updated.QoS.Tailnets[0].Group != "shared" || !updated.Grants[0].IdentityUntil.Equal(initial.Grants[0].IdentityUntil) || !updated.Grants[0].ControlUntil.Equal(initial.Grants[0].ControlUntil) {
		t.Fatal("transfer left foreign tailnet in owner group or renewed permission", err)
	}
}
