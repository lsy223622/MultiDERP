package control

import (
	"encoding/json"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func policyTailnet(t *testing.T, s *Store, owner, id string, keys []identityKey, last time.Time) {
	t.Helper()
	if _, err := s.db.Exec("INSERT INTO tailnets(id,owner_id,display_name) VALUES(?,?,?)", id, owner, id); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO identity_snapshots(tailnet_id,revision,last_success,keys_json) VALUES(?,1,?,?)", id, last.Unix(), b); err != nil {
		t.Fatal(err)
	}
}

func policyGrant(t *testing.T, s *Store, node, tailnet, state string, last time.Time) {
	t.Helper()
	if _, err := s.db.Exec("INSERT INTO grants(id,node_id,tailnet_id,state,revision,last_control_success) VALUES(?,?,?,?,1,?)", randomToken(), node, tailnet, state, last.Unix()); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyContainsOnlyEligibleGrantsAndFiltersConflictingKeys(t *testing.T) {
	s, admin, member, n, _, _ := registeredTestNode(t)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	s.now = func() time.Time { return now }
	unique, duplicate := key.NewNode().Public().String(), key.NewNode().Public().String()
	policyTailnet(t, s, admin.ID, "own", []identityKey{{NodePublic: unique}, {NodePublic: duplicate}}, now)
	policyTailnet(t, s, member.ID, "shared", []identityKey{{NodePublic: duplicate}}, now)
	policyTailnet(t, s, admin.ID, "requested", []identityKey{{NodePublic: key.NewNode().Public().String()}}, now)
	policyGrant(t, s, n.ID, "own", "active", now)
	policyGrant(t, s, n.ID, "shared", "active", now)
	policyGrant(t, s, n.ID, "requested", "requested", now)
	p, err := s.BuildPolicy(t.Context(), n.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Grants) != 2 || len(p.QoS.Tailnets) != 2 {
		t.Fatal("inactive grant published")
	}
	for _, g := range p.Grants {
		for _, k := range g.Keys {
			if k.NodePublic == duplicate {
				t.Fatal("conflicting key published")
			}
		}
	}
	if p.Grants[0].TailnetID != "own" || len(p.Grants[0].Keys) != 1 || p.Grants[0].Keys[0].NodePublic != unique {
		t.Fatal("eligible key omitted")
	}
	if p.QoS.Tailnets[0].Group != "owner" || p.QoS.Tailnets[1].Group != "shared" {
		t.Fatal("wrong service groups")
	}
	again, err := s.BuildPolicy(t.Context(), n.ID, now.Add(time.Minute))
	if err != nil || again.Revision != p.Revision || !again.GeneratedAt.Equal(p.GeneratedAt) {
		t.Fatal("resend renewed policy")
	}
	if _, err := s.db.Exec("UPDATE tailnets SET enabled=0 WHERE id='own'"); err != nil {
		t.Fatal(err)
	}
	updated, err := s.BuildPolicy(t.Context(), n.ID, now.Add(time.Minute))
	if err != nil || len(updated.Grants) != 1 || updated.Revision <= p.Revision {
		t.Fatal("disabled tailnet remained eligible", err)
	}
}

func TestPolicyRetentionChangesAreExplicitAndIdentityDoesNotRenew(t *testing.T) {
	s, admin, member, n, _, session := registeredTestNode(t)
	initial := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	now := initial
	s.now = func() time.Time { return now }
	policyTailnet(t, s, admin.ID, "own", []identityKey{{NodePublic: key.NewNode().Public().String()}}, initial)
	policyGrant(t, s, n.ID, "own", "active", initial)
	if err := s.SetRetentions(t.Context(), member, Retentions{IdentitySeconds: 7200, ControlSeconds: 3600}); err != ErrForbidden {
		t.Fatal("member changed retention")
	}
	if err := s.SetRetentions(t.Context(), admin, Retentions{IdentitySeconds: 7200, ControlSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	p, err := s.BuildPolicy(t.Context(), n.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Grants[0].IdentityUntil.Equal(initial.Add(2*time.Hour)) || !p.Grants[0].ControlUntil.Equal(initial.Add(time.Hour)) {
		t.Fatal("retention deadlines wrong")
	}
	now = initial.Add(20 * time.Minute)
	if _, err := s.NodeHeartbeat(t.Context(), session.Token); err != nil {
		t.Fatal(err)
	}
	renewed, err := s.BuildPolicy(t.Context(), n.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.Grants[0].IdentityUntil.Equal(p.Grants[0].IdentityUntil) || !renewed.Grants[0].ControlUntil.Equal(now.Add(time.Hour)) || renewed.Revision <= p.Revision {
		t.Fatal("control lease renewed identity")
	}
	before := renewed.Revision
	if err := s.SetRetentions(t.Context(), admin, Retentions{IdentitySeconds: 3600, ControlSeconds: 1800}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.BuildPolicy(t.Context(), n.ID, now)
	if err != nil || changed.Revision <= before || !changed.Grants[0].IdentityUntil.Equal(initial.Add(time.Hour)) {
		t.Fatal("setting did not change desired snapshot", err)
	}
}
