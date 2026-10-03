package control

import (
	"errors"
	"sync"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func grantFixture(t *testing.T) (*Store, Actor, Actor, Node, string) {
	t.Helper()
	s, admin, member, n, _, _ := registeredTestNode(t)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	s.now = func() time.Time { return now }
	tailnet := randomToken()
	policyTailnet(t, s, member.ID, tailnet, []identityKey{{NodePublic: key.NewNode().Public().String()}}, now)
	return s, admin, member, n, tailnet
}

func TestGrantRequiresOwnerApprovalAndApplicantConfirmation(t *testing.T) {
	s, provider, applicant, n, tn := grantFixture(t)
	g, err := s.RequestGrant(t.Context(), applicant, n.ID, tn, 0, time.Time{})
	if err != nil || g.State != "requested" || g.Revision != 1 {
		t.Fatal(g, err)
	}
	retry, err := s.RequestGrant(t.Context(), applicant, n.ID, tn, 0, time.Time{})
	if err != nil || retry.ID != g.ID || retry.Revision != g.Revision {
		t.Fatal("request retry not idempotent", err)
	}
	assertKeys := func(want int) {
		t.Helper()
		p, err := s.currentPolicy(t.Context(), n.ID)
		if err != nil || len(p.Grants) != want {
			t.Fatal("wrong effective grant count", len(p.Grants), err)
		}
	}
	assertKeys(0)
	if _, err := s.ApplyGrantAction(t.Context(), applicant, g.ID, g.Revision, GrantApprove); !errors.Is(err, ErrForbidden) {
		t.Fatal("applicant approved provider's node", err)
	}
	g, err = s.ApplyGrantAction(t.Context(), provider, g.ID, g.Revision, GrantApprove)
	if err != nil || g.State != "owner_approved" {
		t.Fatal(g, err)
	}
	assertKeys(0)
	// A provider who is an ordinary member cannot confirm for the applicant.
	ordinaryProvider := provider
	ordinaryProvider.Role = "member"
	if _, err := s.db.Exec("UPDATE users SET role='member' WHERE id=?", provider.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyGrantAction(t.Context(), ordinaryProvider, g.ID, g.Revision, GrantConfirm); !errors.Is(err, ErrForbidden) {
		t.Fatal("provider confirmed for applicant", err)
	}
	g, err = s.ApplyGrantAction(t.Context(), applicant, g.ID, g.Revision, GrantConfirm)
	if err != nil || g.State != "active" {
		t.Fatal(g, err)
	}
	assertKeys(1)
	retry, err = s.ApplyGrantAction(t.Context(), applicant, g.ID, g.Revision-1, GrantConfirm)
	if err != nil || retry.Revision != g.Revision {
		t.Fatal("confirmation retry changed revision", err)
	}
	g, err = s.ApplyGrantAction(t.Context(), ordinaryProvider, g.ID, g.Revision, GrantRevoke)
	if err != nil || g.State != "revoked" {
		t.Fatal(g, err)
	}
	assertKeys(0)
	if _, err := s.ApplyGrantAction(t.Context(), applicant, g.ID, g.Revision-2, GrantConfirm); !errors.Is(err, ErrConflict) {
		t.Fatal("old confirmation revived revocation", err)
	}
	newRound, err := s.RequestGrant(t.Context(), applicant, n.ID, tn, g.Revision, time.Time{})
	if err != nil || newRound.ID != g.ID || newRound.Revision <= g.Revision || newRound.State != "requested" {
		t.Fatal("new request did not begin a new round", err)
	}
}

func TestGrantConcurrentConfirmAndRevokeCannotResurrect(t *testing.T) {
	s, provider, applicant, n, tn := grantFixture(t)
	g, err := s.RequestGrant(t.Context(), applicant, n.ID, tn, 0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	g, err = s.ApplyGrantAction(t.Context(), provider, g.ID, g.Revision, GrantApprove)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, op := range []struct {
		actor  Actor
		action GrantAction
	}{{applicant, GrantConfirm}, {provider, GrantRevoke}} {
		wg.Go(func() {
			<-start
			_, err := s.ApplyGrantAction(t.Context(), op.actor, g.ID, g.Revision, op.action)
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var success, conflicts int
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal("both concurrent actions committed", success, conflicts)
	}
	current, err := s.Grant(t.Context(), provider, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State == "active" {
		current, err = s.ApplyGrantAction(t.Context(), provider, current.ID, current.Revision, GrantRevoke)
		if err != nil {
			t.Fatal(err)
		}
	}
	if current.State != "revoked" {
		t.Fatal("revocation did not prevail")
	}
	if _, err := s.ApplyGrantAction(t.Context(), applicant, g.ID, g.Revision, GrantConfirm); !errors.Is(err, ErrConflict) {
		t.Fatal("stale confirm accepted", err)
	}
}

func TestGrantSelfUseExpiryAndAdministratorAudit(t *testing.T) {
	s, admin, member, n, tn := grantFixture(t)
	if _, err := s.db.Exec("UPDATE tailnets SET owner_id=? WHERE id=?", admin.ID, tn); err != nil {
		t.Fatal(err)
	}
	until := s.now().Add(time.Hour)
	g, err := s.RequestGrant(t.Context(), admin, n.ID, tn, 0, until)
	if err != nil || g.State != "active" {
		t.Fatal("self-use was not activated", err)
	}
	if _, err := s.RequestGrant(t.Context(), member, n.ID, tn, 0, until); !errors.Is(err, ErrForbidden) {
		t.Fatal("foreign tailnet requested", err)
	}
	if _, err := s.ApplyGrantAction(t.Context(), admin, g.ID, g.Revision, GrantLeave); err != nil {
		t.Fatal(err)
	}
	var actorID string
	if err := s.db.QueryRow("SELECT actor_id FROM audit WHERE resource_id=? AND action='grant.leave'", g.ID).Scan(&actorID); err != nil || actorID != admin.ID {
		t.Fatal("actual administrator actor missing", err)
	}
	g, err = s.RequestGrant(t.Context(), admin, n.ID, tn, g.Revision+1, until)
	if err != nil {
		t.Fatal(err)
	}
	now := until.Add(time.Second)
	s.now = func() time.Time { return now }
	expired, err := s.Grant(t.Context(), admin, g.ID)
	if err != nil || expired.State != "expired" {
		t.Fatal("explicit expiry not visible", err)
	}
	if _, err := s.ApplyGrantAction(t.Context(), admin, g.ID, g.Revision, GrantConfirm); !errors.Is(err, ErrConflict) {
		t.Fatal("expired relationship revived", err)
	}
}

func TestGrantPendingTerminationAndForeignActions(t *testing.T) {
	for _, action := range []GrantAction{GrantCancel, GrantReject} {
		t.Run(string(action), func(t *testing.T) {
			s, provider, applicant, n, tn := grantFixture(t)
			other, err := s.CreateMember(t.Context(), provider, "outsider", "outsider-password")
			if err != nil {
				t.Fatal(err)
			}
			g, err := s.RequestGrant(t.Context(), applicant, n.ID, tn, 0, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Grant(t.Context(), other, g.ID); !errors.Is(err, ErrForbidden) {
				t.Fatal("unrelated member read grant", err)
			}
			if _, err := s.ApplyGrantAction(t.Context(), other, g.ID, g.Revision, action); !errors.Is(err, ErrForbidden) {
				t.Fatal("unrelated member changed grant", err)
			}
			actor := applicant
			if action == GrantReject {
				actor = provider
			}
			g, err = s.ApplyGrantAction(t.Context(), actor, g.ID, g.Revision, action)
			if err != nil || (g.State != "cancelled" && g.State != "rejected") {
				t.Fatal(g, err)
			}
		})
	}
}
