package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func TestNodeDomainNormalizationAndRejectsURLs(t *testing.T) {
	got, err := normalizeNodeDomain("Relay.Example.COM.")
	if err != nil || got != "relay.example.com" {
		t.Fatal("domain not canonical")
	}
	for _, name := range []string{"localhost", "127.0.0.1", "::1", "https://relay.example.com", "user@relay.example.com", "relay.example.com:443", "relay.example.com/path", "relay.example.com?target=x", "bad-.example.com", "bad..example.com"} {
		if _, err := normalizeNodeDomain(name); err == nil {
			t.Fatalf("invalid domain accepted: %s", name)
		}
	}
}

type domainTestConn struct{ net.Conn }

func (c domainTestConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 443}
}

func TestDomainChallengeRequiresTrustedTLSProofAndNoRedirect(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	c := cluster.NodeChallenge{Purpose: "enroll", ClusterID: "cluster", NodeID: "node", Domain: "example.com", Nonce: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(time.Minute), PublicKey: pub, InstanceID: "instance"}
	h := cluster.NewDomainResponder(priv)
	h.SetChallenge(c)
	var redirect atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect.Load() {
			http.Redirect(w, r, "http://127.0.0.1:80/private", 302)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer server.Close()
	v := newDomainVerifier(nil)
	v.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	v.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "1.1.1.1:443" {
			t.Error("unvalidated target")
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		if err != nil {
			return nil, err
		}
		return domainTestConn{conn}, nil
	}
	if err := v.verify(t.Context(), c); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	v.tlsRoots = roots
	if err := v.verify(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	redirect.Store(true)
	if err := v.verify(t.Context(), c); err == nil {
		t.Fatal("redirect accepted")
	}
}

func TestDomainBlocksPrivateLoopbackMetadataAndMappedIPs(t *testing.T) {
	v := newDomainVerifier(nil)
	for _, ip := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.0.0.2", "172.16.1.2", "192.168.1.2", "169.254.169.254", "fe80::1", "fc00::1", "100.100.100.200", "198.18.0.1", "0.0.0.0", "224.0.0.1"} {
		if v.allowed(netip.MustParseAddr(ip)) {
			t.Fatalf("SSRF address allowed: %s", ip)
		}
	}
	if !v.allowed(netip.MustParseAddr("1.1.1.1")) || !v.allowed(netip.MustParseAddr("2606:4700:4700::1111")) {
		t.Fatal("public addresses blocked")
	}
	v = newDomainVerifier([]netip.Prefix{netip.MustParsePrefix("10.23.0.0/16")})
	if !v.allowed(netip.MustParseAddr("10.23.1.2")) || v.allowed(netip.MustParseAddr("10.24.1.2")) {
		t.Fatal("admin CIDR not enforced")
	}
}

func TestDomainRebindingRejectedAtDial(t *testing.T) {
	v := newDomainVerifier(nil)
	lookups := 0
	dialed := false
	v.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		ip := "1.1.1.1"
		if lookups > 1 {
			ip = "10.0.0.1"
		}
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
	v.dial = func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}
	if _, err := v.resolve(t.Context(), "relay.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.dialDomain(t.Context(), "tcp", "relay.example.com:443"); err == nil || dialed {
		t.Fatal("DNS rebinding reached private address")
	}
}

func TestDomainPinsDialToValidatedLiteralIP(t *testing.T) {
	v := newDomainVerifier(nil)
	v.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	var targets []string
	v.dial = func(_ context.Context, network, address string) (net.Conn, error) {
		targets = append(targets, network+":"+address)
		return nil, errors.New("offline fixture")
	}
	if _, err := v.dialDomain(t.Context(), "tcp", "relay.example.com:443"); err == nil {
		t.Fatal("failed dial succeeded")
	}
	if !reflect.DeepEqual(targets, []string{"tcp:1.1.1.1:443"}) {
		t.Fatalf("unvalidated hostname dial: %v", targets)
	}
	if _, err := v.dialDomain(t.Context(), "tcp", "relay.example.com:80"); err == nil {
		t.Fatal("unapproved port allowed")
	}
}
