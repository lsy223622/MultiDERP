package control

import (
	"encoding/json"
	"testing"
)

func TestGrantHTTPEnforcesCSRFAndResourceOwnership(t *testing.T) {
	s, provider, applicant, n, tn := grantFixture(t)
	h := NewHTTPHandler(s)
	providerCookie, providerCSRF := loginTest(t, h, "admin")
	applicantCookie, applicantCSRF := loginTest(t, h, "alice")
	body := map[string]any{"node_id": n.ID, "tailnet_id": tn, "expected_revision": 0}
	if w := accountRequest(h, applicantCookie, "", "POST", "/api/v1/grants", body); w.Code != 403 {
		t.Fatal("missing CSRF allowed", w.Code)
	}
	w := accountRequest(h, providerCookie, providerCSRF, "POST", "/api/v1/grants", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var g Grant
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	if g.TailnetOwnerID != applicant.ID || g.State != "requested" {
		t.Fatal("administrator request changed applicant or bypassed approval", g)
	}
	audit, err := s.Audit(t.Context(), applicant)
	if err != nil || len(audit) == 0 || audit[0].Action != "grant.request" || audit[0].ActorID != provider.ID || audit[0].ResourceID != g.ID {
		t.Fatal("administrator request did not retain actual actor", audit, err)
	}
	path := "/api/v1/grants/" + g.ID + "/actions"
	body = map[string]any{"expected_revision": g.Revision, "action": "approve"}
	if w := accountRequest(h, applicantCookie, applicantCSRF, "POST", path, body); w.Code != 403 {
		t.Fatal("applicant approved via API", w.Code)
	}
	w = accountRequest(h, providerCookie, providerCSRF, "POST", path, body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	body = map[string]any{"expected_revision": g.Revision, "action": "confirm"}
	w = accountRequest(h, applicantCookie, applicantCSRF, "POST", path, body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := accountRequest(h, applicantCookie, applicantCSRF, "GET", "/api/v1/tailnets/"+tn+"/derpmap", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := accountRequest(h, applicantCookie, applicantCSRF, "POST", path, map[string]any{"expected_revision": 3, "state": "active"}); w.Code != 400 {
		t.Fatal("arbitrary state accepted", w.Code)
	}
	if w := accountRequest(h, applicantCookie, applicantCSRF, "POST", "/api/v1/nodes/"+n.ID+"/qos", map[string]any{"budget_bps": 100000000, "owner_weight": 8, "shared_weight": 2}); w.Code != 403 {
		t.Fatal("sharing user changed QoS", w.Code)
	}
	w = accountRequest(h, applicantCookie, applicantCSRF, "GET", "/api/v1/relays", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var relays []Node
	if err := json.Unmarshal(w.Body.Bytes(), &relays); err != nil || len(relays) != 1 || relays[0].ID != n.ID {
		t.Fatal("registered node not discoverable", err)
	}
}
