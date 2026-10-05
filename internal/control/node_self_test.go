package control

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNodeSelfPortsAreInstanceBound(t *testing.T) {
	s, admin, _, n, _, session := registeredTestNode(t)
	if err := s.SetNodeSessionPorts(t.Context(), session.Token, 3489, 3488); err != nil {
		t.Fatal(err)
	}
	got, err := s.Node(t.Context(), admin, n.ID)
	if err != nil || got.DERPPort != 3489 || got.STUNPort != 3488 {
		t.Fatal(got, err)
	}
	if err := s.SetNodeSessionPorts(t.Context(), session.Token, 0, 3488); err == nil {
		t.Fatal("invalid port")
	}
	server := httptest.NewServer(NewHTTPHandler(s))
	defer server.Close()
	response := nodeRequest(t, http.DefaultClient, server.URL+"/cluster/v1/node/ports", "POST", session.Token, map[string]any{"node_id": "foreign", "derp_port": 443, "stun_port": 3478})
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("foreign node selector accepted", response.StatusCode)
	}
	if err := s.ReleaseNode(t.Context(), session.Token); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeSessionPorts(t.Context(), session.Token, 443, 3478); err == nil {
		t.Fatal("released token accepted")
	}
}
