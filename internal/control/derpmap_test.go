package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"tailscale.com/tailcfg"
)

func TestDERPMapContainsOnlyEffectiveConfirmedGrants(t *testing.T) {
	s, provider, applicant, n, tn := grantFixture(t)
	g, err := s.RequestGrant(t.Context(), applicant, n.ID, tn, 0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		m, err := s.BuildDERPMap(t.Context(), applicant, tn)
		if err != nil || len(m.Regions) != want || m.OmitDefaultRegions {
			t.Fatal("wrong map", m, err)
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var decoded tailcfg.DERPMap
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		if want == 1 {
			r := decoded.Regions[n.RegionID]
			if r == nil || len(r.Nodes) != 1 || r.Nodes[0].RegionID != n.RegionID || r.Nodes[0].HostName != n.Domain || r.Nodes[0].DERPPort != 443 || r.Nodes[0].STUNPort != 3478 || r.Nodes[0].InsecureForTests {
				t.Fatal("invalid upstream map", decoded)
			}
		}
	}
	check(0)
	g, err = s.ApplyGrantAction(t.Context(), provider, g.ID, g.Revision, GrantApprove)
	if err != nil {
		t.Fatal(err)
	}
	check(0)
	g, err = s.ApplyGrantAction(t.Context(), applicant, g.ID, g.Revision, GrantConfirm)
	if err != nil {
		t.Fatal(err)
	}
	check(1)
	if _, err := s.db.Exec("UPDATE nodes SET state='offline' WHERE id=?", n.ID); err != nil {
		t.Fatal(err)
	}
	check(1)
	if _, err := s.ApplyGrantAction(t.Context(), provider, g.ID, g.Revision, GrantRevoke); err != nil {
		t.Fatal(err)
	}
	check(0)
}

func TestDERPMapForeignOwnerAndIdentityExpiry(t *testing.T) {
	s, admin, member, n, tn := grantFixture(t)
	if _, err := s.db.Exec("UPDATE tailnets SET owner_id=? WHERE id=?", admin.ID, tn); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestGrant(t.Context(), admin, n.ID, tn, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildDERPMap(t.Context(), member, tn); !errors.Is(err, ErrForbidden) {
		t.Fatal("foreign map returned", err)
	}
	now := s.now().Add(48 * time.Hour)
	s.now = func() time.Time { return now }
	m, err := s.BuildDERPMap(t.Context(), admin, tn)
	if err != nil || len(m.Regions) != 0 {
		t.Fatal("expired identity advertised as effective", err)
	}
}

func TestDERPMapRegionCapacityAndDuplicateDomain(t *testing.T) {
	s, admin, _ := nodeTestStore(t)
	for i := 0; i < 100; i++ {
		n, _, err := s.CreateNode(t.Context(), admin, "Relay", fmt.Sprintf("r%d.example.com", i))
		if err != nil || n.RegionID != 900+i {
			t.Fatal(i, n.RegionID, err)
		}
	}
	if _, _, err := s.CreateNode(t.Context(), admin, "Full", "full.example.com"); !errors.Is(err, ErrConflict) {
		t.Fatal("region range extended", err)
	}
	if _, _, err := s.CreateNode(t.Context(), admin, "Duplicate", "R0.EXAMPLE.COM."); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate canonical domain accepted", err)
	}
}
