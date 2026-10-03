package control

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/net/stun"
)

func TestDERPProbeUsesTrustedTLSFixedPathAndRejectsRedirect(t *testing.T) {
	var redirect atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/derp/probe" || r.URL.RawQuery != "" {
			t.Error("unexpected probe target", r.URL)
		}
		if redirect.Load() {
			http.Redirect(w, r, "http://127.0.0.1/private", 302)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	v := newDomainVerifier(nil)
	v.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	v.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "1.1.1.1:443" {
			t.Error("unvalidated probe target", network, address)
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		return domainTestConn{conn}, err
	}
	if err := v.probeDERP(t.Context(), "example.com"); err == nil {
		t.Fatal("untrusted probe certificate accepted")
	}
	v.tlsRoots = x509.NewCertPool()
	v.tlsRoots.AddCert(server.Certificate())
	if err := v.probeDERP(t.Context(), "example.com"); err != nil {
		t.Fatal(err)
	}
	redirect.Store(true)
	if err := v.probeDERP(t.Context(), "example.com"); err == nil {
		t.Fatal("redirect accepted")
	}
}

type stunTestConn struct{ net.Conn }

func (c stunTestConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("1.1.1.1"), Port: 3478}
}

func TestSTUNProbeRequiresMatchingTransactionAndValidatedTarget(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var mismatch atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			buffer := make([]byte, 1500)
			n, addr, err := server.ReadFrom(buffer)
			if err != nil {
				return
			}
			id, err := stun.ParseBindingRequest(buffer[:n])
			if err != nil {
				t.Error(err)
				return
			}
			if mismatch.Load() {
				id = stun.NewTxID()
			}
			server.WriteTo(stun.Response(id, netip.MustParseAddrPort("1.2.3.4:5678")), addr)
		}
	}()
	defer func() { server.Close(); <-done }()
	v := newDomainVerifier(nil)
	v.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	v.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "udp" || address != "1.1.1.1:3478" {
			t.Error("unvalidated STUN target", network, address)
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, network, server.LocalAddr().String())
		return stunTestConn{conn}, err
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := v.probeSTUN(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	mismatch.Store(true)
	if err := v.probeSTUN(ctx, "example.com"); err == nil {
		t.Fatal("another transaction accepted")
	}
	v.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	if err := v.probeSTUN(ctx, "example.com"); err == nil {
		t.Fatal("restricted STUN target accepted")
	}
}

func TestNodeProbePersistsSourceTimesWithoutAdvancingPolicyACK(t *testing.T) {
	s, admin, _, n, _, _ := registeredTestNode(t)
	now := time.Now().UTC()
	want := NodeProbes{DERP: Probe{State: "ok", ObservedAt: now}, STUN: Probe{State: "failed", ObservedAt: now.Add(time.Millisecond)}}
	s.probeDomain = func(_ context.Context, domain string) NodeProbes {
		if domain != n.Domain {
			t.Fatal("probe changed domain")
		}
		return want
	}
	if _, err := s.ProbeNode(t.Context(), admin, n.ID); err != nil {
		t.Fatal(err)
	}
	state, err := s.NodeStatus(t.Context(), admin, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Probes == nil || *state.Probes != want || state.AppliedRevision != 0 || state.Report != nil || state.ReportedAt != 0 {
		t.Fatal("probe confused with runtime/ACK", state)
	}
	if _, err := s.NodeHeartbeat(t.Context(), "invalid", nil); err == nil {
		t.Fatal("invalid heartbeat authenticated")
	}
}

func TestIndependentNodeProbeRequiresResourceOwnerAndCSRF(t *testing.T) {
	s, _, _, n, _, _ := registeredTestNode(t)
	h := NewHTTPHandler(s)
	memberCookie, memberCSRF := loginTest(t, h, "alice")
	adminCookie, adminCSRF := loginTest(t, h, "admin")
	path := "/api/v1/nodes/" + n.ID + "/probe"
	if w := accountRequest(h, memberCookie, memberCSRF, "POST", path, map[string]any{}); w.Code != 403 {
		t.Fatal("foreign member triggered probe", w.Code)
	}
	if w := accountRequest(h, adminCookie, "", "POST", path, map[string]any{}); w.Code != 403 {
		t.Fatal("probe bypassed CSRF", w.Code)
	}
	// A pending node has no verified domain to probe.
	pending, _, err := s.CreateNode(t.Context(), Actor{ID: n.OwnerID, Role: "admin", Enabled: true}, "pending", "pending.example.com")
	if err != nil {
		t.Fatal(err)
	}
	w := accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/nodes/"+pending.ID+"/probe", map[string]any{})
	if w.Code != 409 || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatal("unverified domain probed", w.Code, w.Body.String())
	}
}
