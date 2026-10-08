package control

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func normalizeNodeDomain(domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if len(domain) == 0 || len(domain) > 253 || !strings.Contains(domain, ".") {
		return "", ErrInvalid
	}
	if _, err := netip.ParseAddr(domain); err == nil {
		return "", ErrInvalid
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalid
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", ErrInvalid
			}
		}
	}
	return domain, nil
}

type domainVerifier struct {
	tlsRoots     *x509.CertPool
	allowedCIDRs []netip.Prefix
	lookup       func(context.Context, string, string) ([]netip.Addr, error)
	dial         func(context.Context, string, string) (net.Conn, error)
}

func (v *domainVerifier) verify(ctx context.Context, c cluster.NodeChallenge) error {
	domain, err := normalizeNodeDomain(c.Domain)
	_, nonceErr := hex.DecodeString(c.Nonce)
	if err != nil || domain != c.Domain || len(c.Nonce) != 64 || nonceErr != nil || len(c.PublicKey) != ed25519.PublicKeySize || !time.Now().Before(c.ExpiresAt) {
		return ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := v.resolve(ctx, domain); err != nil {
		return err
	}
	transport := &http.Transport{DialContext: v.dialDomain, TLSClientConfig: &tls.Config{RootCAs: v.tlsRoots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequestWithContext(ctx, "GET", "https://"+domain+"/cluster/v1/domain-challenge/"+c.Nonce, nil)
	if err != nil {
		return ErrInvalid
	}
	response, err := client.Do(r)
	if err != nil {
		return errors.New("node domain TLS connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("node domain proof unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		return errors.New("invalid node domain proof")
	}
	var proof cluster.DomainResponse
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if dec.Decode(&proof) != nil || dec.Decode(new(any)) != io.EOF || !ed25519.Verify(ed25519.PublicKey(c.PublicKey), c.DomainSigningBytes(), proof.Signature) {
		return errors.New("invalid node domain proof")
	}
	return nil
}

func newDomainVerifier(allowed []netip.Prefix) *domainVerifier {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &domainVerifier{allowedCIDRs: append([]netip.Prefix(nil), allowed...), lookup: net.DefaultResolver.LookupNetIP, dial: dialer.DialContext}
}

var restrictedNodeRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("fec0::/10"),
}

func (v *domainVerifier) allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip == netip.MustParseAddr("100.100.100.200") || ip == netip.MustParseAddr("fd00:ec2::254") {
		return false
	}
	restricted := ip.IsPrivate()
	for _, prefix := range restrictedNodeRanges {
		if prefix.Contains(ip) {
			restricted = true
			break
		}
	}
	if !restricted {
		return true
	}
	for _, prefix := range v.allowedCIDRs {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func (v *domainVerifier) resolve(ctx context.Context, domain string) ([]netip.Addr, error) {
	if _, err := normalizeNodeDomain(domain); err != nil {
		return nil, err
	}
	ips, err := v.lookup(ctx, "ip", domain)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("node domain cannot be resolved")
	}
	for _, ip := range ips {
		if !v.allowed(ip) {
			return nil, errors.New("node domain resolves to a disallowed address")
		}
	}
	return ips, nil
}

func (v *domainVerifier) dialDomain(ctx context.Context, network, address string) (net.Conn, error) {
	return v.dialDomainPort(ctx, network, address, "443")
}

func (v *domainVerifier) dialDomainPort(ctx context.Context, network, address, expectedPort string) (net.Conn, error) {
	domain, port, err := net.SplitHostPort(address)
	if err != nil || network != "tcp" || port != expectedPort {
		return nil, ErrInvalid
	}
	ips, err := v.resolve(ctx, domain)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		conn, err := v.dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err != nil {
			continue
		}
		host, remotePort, err := net.SplitHostPort(conn.RemoteAddr().String())
		remote, parseErr := netip.ParseAddr(host)
		if err != nil || parseErr != nil || remote.Unmap() != ip.Unmap() || remotePort != port || !v.allowed(remote) {
			conn.Close()
			return nil, errors.New("node connection target changed")
		}
		return conn, nil
	}
	return nil, errors.New("node domain connection failed")
}
