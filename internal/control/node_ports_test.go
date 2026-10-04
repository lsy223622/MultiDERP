package control

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestNodePublicPortsOwnerAdminAndSharedMap(t *testing.T) {
	s, provider, applicant, n, tn := grantFixture(t)
	h := NewHTTPHandler(s)
	ownerCookie, ownerCSRF := loginTest(t, h, "admin")
	sharedCookie, sharedCSRF := loginTest(t, h, "alice")
	path := "/api/v1/nodes/" + n.ID + "/ports"
	body := map[string]int{"derp_port": 3489, "stun_port": 3488}
	if w := accountRequest(h, sharedCookie, sharedCSRF, "POST", path, body); w.Code != 403 {
		t.Fatal("shared member changed provider ports", w.Code)
	}
	if w := accountRequest(h, ownerCookie, "", "POST", path, body); w.Code != 403 {
		t.Fatal("port change bypassed CSRF", w.Code)
	}
	if w := accountRequest(h, ownerCookie, ownerCSRF, "POST", path, body); w.Code != 200 {
		t.Fatal("owner could not set public ports", w.Code, w.Body.String())
	}
	g, err := s.RequestGrant(t.Context(), applicant, n.ID, tn, 0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	g, err = s.ApplyGrantAction(t.Context(), provider, g.ID, g.Revision, GrantApprove)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyGrantAction(t.Context(), applicant, g.ID, g.Revision, GrantConfirm); err != nil {
		t.Fatal(err)
	}
	state := accountRequest(h, sharedCookie, sharedCSRF, "GET", "/api/v1/nodes/"+n.ID+"/status", nil)
	var decoded struct {
		Node map[string]any `json:"node"`
	}
	if state.Code != 200 || json.Unmarshal(state.Body.Bytes(), &decoded) != nil || decoded.Node["derp_port"] != float64(3489) || decoded.Node["stun_port"] != float64(3488) {
		t.Fatal("shared view lost public ports", state.Code, state.Body.String())
	}
	m, err := s.BuildDERPMap(t.Context(), applicant, tn)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := m.Regions[n.RegionID].Nodes[0]
	if endpoint.DERPPort != 3489 || endpoint.STUNPort != 3488 || endpoint.HostName != n.Domain || endpoint.InsecureForTests {
		t.Fatal("map does not advertise configured public endpoint", endpoint)
	}
	for _, ports := range []map[string]int{{"derp_port": 0, "stun_port": 3488}, {"derp_port": 443, "stun_port": 65536}, {"derp_port": -1, "stun_port": 3478}} {
		if w := accountRequest(h, ownerCookie, ownerCSRF, "POST", path, ports); w.Code != 400 {
			t.Fatal("invalid ports accepted", ports, w.Code)
		}
	}
	// The administrator can configure a member-owned resource.
	if _, err := s.db.Exec("UPDATE nodes SET owner_id=? WHERE id=?", applicant.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	if w := accountRequest(h, sharedCookie, sharedCSRF, "POST", path, body); w.Code != 200 {
		t.Fatal("member owner denied", w.Code)
	}
	if w := accountRequest(h, ownerCookie, ownerCSRF, "POST", path, map[string]int{"derp_port": 443, "stun_port": 3478}); w.Code != 200 {
		t.Fatal("administrator denied", w.Code)
	}
}

func TestNodeCreationDefaultsAndExplicitPublicPorts(t *testing.T) {
	s, _, _ := nodeTestStore(t)
	h := NewHTTPHandler(s)
	cookie, csrf := loginTest(t, h, "admin")
	for _, test := range []struct {
		domain     string
		ports      map[string]any
		derp, stun float64
	}{
		{"default.example.com", nil, 443, 3478},
		{"custom.example.com", map[string]any{"derp_port": 3489, "stun_port": 3488}, 3489, 3488},
	} {
		body := map[string]any{"display_name": "Relay", "domain": test.domain}
		for k, v := range test.ports {
			body[k] = v
		}
		w := accountRequest(h, cookie, csrf, "POST", "/api/v1/nodes", body)
		var result struct {
			Node map[string]any `json:"node"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Node["derp_port"] != test.derp || result.Node["stun_port"] != test.stun {
			t.Fatal("creation ports lost", w.Code, w.Body.String())
		}
	}
}

func TestVersionFiveNodesMigrateDefaultPublicPorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE nodes(id TEXT PRIMARY KEY,owner_id TEXT,domain TEXT,display_name TEXT,region_id INTEGER,state TEXT,last_heartbeat INTEGER,last_error TEXT,enabled INTEGER);
INSERT INTO nodes VALUES('node','owner','relay.example.com','Relay',902,'registered',123,'',1);
PRAGMA user_version=5;`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := s.Node(t.Context(), Actor{ID: "owner", Role: "member", Enabled: true}, "node")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(n)
	var view map[string]any
	json.Unmarshal(encoded, &view)
	if view["derp_port"] != float64(443) || view["stun_port"] != float64(3478) || n.RegionID != 902 || n.LastHeartbeat != 123 || n.State != "registered" {
		t.Fatal("migration changed resource or omitted default ports", view)
	}
}
