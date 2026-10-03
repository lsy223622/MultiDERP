package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"

	"tailscale.com/types/key"
)

func TestOnlineNodeDeletionWaitsForEmptyPolicyApplication(t *testing.T) {
	s, admin, _, n, _, session := registeredTestNode(t)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	s.now = func() time.Time { return now }
	policyTailnet(t, s, admin.ID, "own", []identityKey{{NodePublic: key.NewNode().Public().String(), NotAfter: now.Add(time.Hour)}}, now)
	policyGrant(t, s, n.ID, "own", "active", now)
	if _, err := s.BuildPolicy(t.Context(), n.ID, now); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(NewHTTPHandler(s))
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	response := nodeRequest(t, client, server.URL+"/cluster/v1/control", "GET", session.Token, nil)
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if _, err := reader.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeEnabled(t.Context(), admin, n.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(t.Context(), admin, n.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("online identity deleted before revoke ACK", err)
	}
	body, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var message cluster.ControlMessage
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatal(err)
	}
	if message.Policy == nil || len(message.Policy.Grants) != 0 {
		t.Fatal("pause did not publish empty policy")
	}
	for _, state := range []string{"received", "applied"} {
		ack := nodeRequest(t, client, server.URL+"/cluster/v1/ack", "POST", session.Token, cluster.PolicyACK{Revision: message.Policy.Revision, State: state})
		ack.Body.Close()
		if ack.StatusCode != http.StatusOK {
			t.Fatal("empty policy ACK rejected", ack.StatusCode)
		}
	}
	if err := s.DeleteNode(t.Context(), admin, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateNode(t.Context(), session.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted session remained valid")
	}
}

func TestNodeDomainChangeKeepsKeyAndRequiresNewTrustedDomainProof(t *testing.T) {
	s, admin, _, n, priv, oldSession := registeredTestNode(t)
	h := NewHTTPHandler(s)
	cookie, csrf := loginTest(t, h, "admin")
	path := "/api/v1/nodes/" + n.ID + "/domain"
	w := accountRequest(h, cookie, csrf, "POST", path, map[string]any{"domain": "example.com"})
	if w.Code != 409 {
		t.Fatal("domain changed before pause", w.Code, w.Body.String())
	}
	if err := s.SetNodeEnabled(t.Context(), admin, n.ID, false); err != nil {
		t.Fatal(err)
	}
	w = accountRequest(h, cookie, csrf, "POST", path, map[string]any{"domain": "example.com"})
	if w.Code != 200 {
		t.Fatal("domain change missing", w.Code, w.Body.String())
	}
	if _, err := s.AuthenticateNode(t.Context(), oldSession.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old session survived domain change", err)
	}
	state, err := s.NodeStatus(t.Context(), admin, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Node.State != "domain_pending" || state.DomainVerifiedAt != 0 || state.Node.Enabled {
		t.Fatal("unverified new domain appeared active", state)
	}
	challenge, err := s.SessionChallenge(t.Context(), n.ID, oldSession.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.Purpose != "domain_change" || challenge.Domain != "example.com" || !bytes.Equal(challenge.PublicKey, priv.Public().(ed25519.PublicKey)) {
		t.Fatal("domain change replaced node identity", challenge)
	}
	responder := cluster.NewDomainResponder(priv)
	responder.SetChallenge(challenge)
	server := httptest.NewTLSServer(responder)
	defer server.Close()
	v := newDomainVerifier(nil)
	v.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	v.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		return domainTestConn{conn}, err
	}
	s.verifyDomain = v.verify
	signature := ed25519.Sign(priv, challenge.SigningBytes())
	if _, err := s.RenewNodeSession(t.Context(), challenge, signature); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("untrusted new domain accepted", err)
	}
	v.tlsRoots = x509.NewCertPool()
	v.tlsRoots.AddCert(server.Certificate())
	session, err := s.RenewNodeSession(t.Context(), challenge, signature)
	if err != nil {
		t.Fatal(err)
	}
	if session.NodeID != n.ID || session.ClusterID != oldSession.ClusterID {
		t.Fatal("domain change created new identity")
	}
	state, err = s.NodeStatus(t.Context(), admin, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Node.State != "registered" || state.DomainVerifiedAt == 0 || state.Node.Enabled {
		t.Fatal("domain validation implicitly resumed node", state)
	}
}

func TestResourcePauseAndDeletionRebuildAuthorizationAndCheckOwner(t *testing.T) {
	s, _, member, n, _, session := registeredTestNode(t)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	s.now = func() time.Time { return now }
	policyTailnet(t, s, member.ID, "shared", []identityKey{{NodePublic: key.NewNode().Public().String(), NotAfter: now.Add(time.Hour)}}, now)
	policyGrant(t, s, n.ID, "shared", "active", now)
	p, err := s.BuildPolicy(t.Context(), n.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	original := p.Grants[0]
	h := NewHTTPHandler(s)
	memberCookie, memberCSRF := loginTest(t, h, "alice")
	adminCookie, adminCSRF := loginTest(t, h, "admin")
	for _, path := range []string{"/api/v1/nodes/" + n.ID + "/enabled", "/api/v1/nodes/" + n.ID} {
		method := "POST"
		if path == "/api/v1/nodes/"+n.ID {
			method = "DELETE"
		}
		if w := accountRequest(h, memberCookie, memberCSRF, method, path, map[string]any{"enabled": false}); w.Code != 403 {
			t.Fatal("foreign owner mutation allowed", method, w.Code, w.Body.String())
		}
	}
	for _, enabled := range []bool{false, true} {
		w := accountRequest(h, memberCookie, memberCSRF, "POST", "/api/v1/tailnets/shared/enabled", map[string]any{"enabled": enabled})
		if w.Code != 200 {
			t.Fatal("tailnet pause missing", w.Code, w.Body.String())
		}
		p, err = s.BuildPolicy(t.Context(), n.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		if !enabled && len(p.Grants) != 0 {
			t.Fatal("paused tailnet retained authorization")
		}
		if enabled && (len(p.Grants) != 1 || !p.Grants[0].IdentityUntil.Equal(original.IdentityUntil) || !p.Grants[0].ControlUntil.Equal(original.ControlUntil)) {
			t.Fatal("restoring tailnet renewed permission")
		}
	}
	w := accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/nodes/"+n.ID+"/enabled", map[string]any{"enabled": false})
	if w.Code != 200 {
		t.Fatal("node pause missing", w.Code, w.Body.String())
	}
	p, err = s.BuildPolicy(t.Context(), n.ID, now)
	if err != nil || len(p.Grants) != 0 {
		t.Fatal("paused node retained authorization", err)
	}
	if _, err := s.NodeHeartbeat(t.Context(), session.Token, nil); err != nil {
		t.Fatal("paused node could not receive control", err)
	}
	w = accountRequest(h, memberCookie, memberCSRF, "DELETE", "/api/v1/tailnets/shared", nil)
	if w.Code != 200 {
		t.Fatal("tailnet deletion missing", w.Code, w.Body.String())
	}
	if _, err := s.Tailnet(t.Context(), member, "shared"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deleted tailnet still present", err)
	}
	w = accountRequest(h, adminCookie, adminCSRF, "DELETE", "/api/v1/nodes/"+n.ID, nil)
	if w.Code != 200 {
		t.Fatal("paused offline node deletion missing", w.Code, w.Body.String())
	}
	if _, err := s.AuthenticateNode(t.Context(), session.Token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted identity still authenticated", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM grants").Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted resource relationships retained", count, err)
	}
}
