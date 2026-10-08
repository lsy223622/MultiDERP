package control

import (
	"errors"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func TestQoSOnlyProviderCanChangeRulesWithoutNewConsent(t *testing.T) {
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
	p, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	q := cluster.QoSPolicy{BudgetBPS: 80000000, OwnerWeight: 8, SharedWeight: 2, SharedMaxBPS: 40000000, Tailnets: []cluster.TailnetQoS{{TailnetID: tn, Group: "shared", Weight: 3, MaxBPS: 20000000}}}
	if err := s.SetNodeQoS(t.Context(), applicant, n.ID, q); !errors.Is(err, ErrForbidden) {
		t.Fatal("shared user changed provider rules", err)
	}
	if err := s.SetNodeQoS(t.Context(), provider, n.ID, q); err != nil {
		t.Fatal(err)
	}
	current, err := s.Grant(t.Context(), applicant, g.ID)
	if err != nil || current.State != "active" || current.Revision != g.Revision {
		t.Fatal("QoS reopened consent", err)
	}
	updated, err := s.currentPolicy(t.Context(), n.ID)
	if err != nil || updated.Revision <= p.Revision || updated.QoS.Tailnets[0].Weight != 3 || updated.QoS.BudgetBPS != q.BudgetBPS || !updated.Grants[0].IdentityUntil.Equal(p.Grants[0].IdentityUntil) || !updated.Grants[0].ControlUntil.Equal(p.Grants[0].ControlUntil) {
		t.Fatal("QoS changed authorization deadlines", err)
	}
	if _, err := s.NodeQoS(t.Context(), applicant, n.ID); err != nil {
		t.Fatal("shared user cannot view rules", err)
	}
	q.Tailnets[0].Group = "owner"
	if err := s.SetNodeQoS(t.Context(), provider, n.ID, q); !errors.Is(err, ErrInvalid) {
		t.Fatal("foreign tailnet assigned owner group", err)
	}
	q.Tailnets[0].Group = "shared"
	q.Tailnets[0].Weight = 0
	if err := s.SetNodeQoS(t.Context(), provider, n.ID, q); !errors.Is(err, ErrInvalid) {
		t.Fatal("zero weight accepted", err)
	}
	q.Tailnets[0].Weight = 1
	q.BudgetBPS = 1
	if err := s.SetNodeQoS(t.Context(), provider, n.ID, q); !errors.Is(err, ErrInvalid) {
		t.Fatal("sub-byte budget accepted", err)
	}
	stored, err := s.NodeQoS(t.Context(), provider, n.ID)
	if err != nil || stored.BudgetBPS != 80000000 || stored.Tailnets[0].Group != "shared" || stored.Tailnets[0].Weight != 3 {
		t.Fatal("rejected rule edit partially changed persisted rules", err)
	}
}
