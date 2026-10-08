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

type publicPortTestConn struct {
	net.Conn
	remote net.Addr
}

func (c publicPortTestConn) RemoteAddr() net.Addr { return c.remote }

func TestNodeProbeUsesSeparatePublicPortsAndClearsOldObservation(t *testing.T) {
	s, admin, _, n, _, _ := registeredTestNode(t)
	var failed atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "example.com" || r.Host != "example.com:3489" || r.URL.Path != "/derp/probe" {
			t.Error("wrong TLS/SNI or probe endpoint", r.Host, r.TLS.ServerName, r.URL)
		}
		if failed.Load() {
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			b := make([]byte, 1500)
			count, addr, err := udp.ReadFrom(b)
			if err != nil {
				return
			}
			id, err := stun.ParseBindingRequest(b[:count])
			if err != nil {
				t.Error(err)
				return
			}
			udp.WriteTo(stun.Response(id, netip.MustParseAddrPort("1.2.3.4:5678")), addr)
		}
	}()
	defer func() { udp.Close(); <-done }()
	v := newDomainVerifier(nil)
	v.tlsRoots = x509.NewCertPool()
	v.tlsRoots.AddCert(server.Certificate())
	v.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	var tcpCalls, udpCalls atomic.Int32
	var wrongRemote atomic.Bool
	v.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		var target string
		var remote net.Addr
		switch {
		case network == "tcp" && address == "1.1.1.1:3489":
			tcpCalls.Add(1)
			target = server.Listener.Addr().String()
			remote = &net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 3489}
		case network == "udp" && address == "1.1.1.1:3488":
			udpCalls.Add(1)
			target = udp.LocalAddr().String()
			remote = &net.UDPAddr{IP: net.ParseIP("1.1.1.1"), Port: 3488}
		default:
			t.Errorf("probe contacted default/old-service endpoint %s %s", network, address)
			return nil, ErrInvalid
		}
		if wrongRemote.Load() {
			remote = &net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 443}
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, network, target)
		if err != nil {
			return nil, err
		}
		return publicPortTestConn{conn, remote}, nil
	}
	if _, err := s.db.Exec("UPDATE nodes SET domain='example.com' WHERE id=?", n.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodePorts(t.Context(), admin, n.ID, 3489, 3488); err != nil {
		t.Fatal(err)
	}
	s.probeDomain = v.probe
	result, err := s.ProbeNode(t.Context(), admin, n.ID)
	if err != nil || result.DERP.State != "ok" || result.STUN.State != "ok" || tcpCalls.Load() != 1 || udpCalls.Load() != 1 {
		t.Fatal("non-default services not probed", result, err, tcpCalls.Load(), udpCalls.Load())
	}
	failed.Store(true)
	result, err = s.ProbeNode(t.Context(), admin, n.ID)
	if err != nil || result.DERP.State != "failed" || result.STUN.State != "ok" {
		t.Fatal("DERP failure hidden by another service", result, err)
	}
	wrongRemote.Store(true)
	result, err = s.ProbeNode(t.Context(), admin, n.ID)
	if err != nil || result.DERP.State != "failed" || result.STUN.State != "failed" {
		t.Fatal("changed actual remote port accepted", result, err)
	}
	if err := s.SetNodePorts(t.Context(), admin, n.ID, 443, 3478); err != nil {
		t.Fatal(err)
	}
	state, err := s.NodeStatus(t.Context(), admin, n.ID)
	if err != nil || state.Probes != nil {
		t.Fatal("old endpoint observation retained after port change", state.Probes, err)
	}
}

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
	if err := v.probeDERP(t.Context(), "example.com", 443); err == nil {
		t.Fatal("untrusted probe certificate accepted")
	}
	v.tlsRoots = x509.NewCertPool()
	v.tlsRoots.AddCert(server.Certificate())
	if err := v.probeDERP(t.Context(), "example.com", 443); err != nil {
		t.Fatal(err)
	}
	redirect.Store(true)
	if err := v.probeDERP(t.Context(), "example.com", 443); err == nil {
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
	if err := v.probeSTUN(ctx, "example.com", 3478); err != nil {
		t.Fatal(err)
	}
	mismatch.Store(true)
	if err := v.probeSTUN(ctx, "example.com", 3478); err == nil {
		t.Fatal("another transaction accepted")
	}
	v.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	if err := v.probeSTUN(ctx, "example.com", 3478); err == nil {
		t.Fatal("restricted STUN target accepted")
	}
}

func TestNodeProbePersistsSourceTimesWithoutAdvancingPolicyACK(t *testing.T) {
	s, admin, _, n, _, _ := registeredTestNode(t)
	now := time.Now().UTC()
	want := NodeProbes{DERP: Probe{State: "ok", ObservedAt: now}, STUN: Probe{State: "failed", ObservedAt: now.Add(time.Millisecond)}}
	s.probeDomain = func(_ context.Context, domain string, derpPort, stunPort int) NodeProbes {
		if domain != n.Domain || derpPort != 443 || stunPort != 3478 {
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
	pending, _, err := s.CreateNode(t.Context(), Actor{ID: n.OwnerID, Role: "admin", Enabled: true}, "pending", "pending.example.com", 443, 3478)
	if err != nil {
		t.Fatal(err)
	}
	w := accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/nodes/"+pending.ID+"/probe", map[string]any{})
	if w.Code != 409 || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatal("unverified domain probed", w.Code, w.Body.String())
	}
}
