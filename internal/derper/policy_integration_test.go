package derper

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"tailscale.com/derp"
	"tailscale.com/derp/derphttp"
	"tailscale.com/net/netmon"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func TestProcessWaitReadyManualTLS(t *testing.T) {
	binary := os.Getenv("UNIDERP_TEST_DERPER")
	if binary == "" {
		t.Skip("requires freshly built patched derper")
	}
	dir, err := os.MkdirTemp("", "ud-tls-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	server := testServer("passthrough")
	server.DERP.CertDir = dir
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: server.Hostname}, DNSNames: []string{server.Hostname}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		server.Hostname + ".crt": {Type: "CERTIFICATE", Bytes: der},
		server.Hostname + ".key": {Type: "PRIVATE KEY", Bytes: keyBytes},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.DERP.Listen = listener.Addr().String()
	listener.Close()
	stun, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.DERP.STUNListen = stun.LocalAddr().String()
	stun.Close()
	process := NewProcess(binary, io.Discard)
	process.Policy = PolicyClient{Path: filepath.Join(dir, "policy.json"), SocketPath: filepath.Join(dir, "policy.sock")}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	if err := process.Start(ctx, server, "127.0.0.1:9", filepath.Join(dir, "derper.key")); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := process.Stop(stop); err != nil {
			t.Error(err)
		}
	}()
	if err := process.WaitReady(ctx, server); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyApplicationOnPatchedDerper(t *testing.T) {
	binary := os.Getenv("UNIDERP_TEST_DERPER")
	if binary == "" {
		t.Skip("requires freshly built patched derper")
	}
	dir, err := os.MkdirTemp("", "ud-real-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	stun, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stunAddress := stun.LocalAddr().String()
	stun.Close()
	server := testServer("external")
	server.DERP.Listen, server.DERP.STUNListen = address, stunAddress
	process := NewProcess(binary, io.Discard)
	process.Policy = PolicyClient{Path: filepath.Join(dir, "policy.json"), SocketPath: filepath.Join(dir, "policy.sock"), MaxBudgetBPS: 80000000}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := process.Start(ctx, server, "127.0.0.1:9", filepath.Join(dir, "derper.key")); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := process.Stop(stop); err != nil {
			t.Error(err)
		}
	}()
	if err := process.WaitReady(ctx, server); err != nil {
		t.Fatal(err)
	}
	status, err := process.Policy.Status(ctx)
	if err != nil || status.Revision != 0 || status.Usable {
		t.Fatal("unregistered child provided usable relay", status, err)
	}
	a, b := key.NewNode(), key.NewNode()
	p := applicationPolicy(t)
	p.Grants[0].Keys = []cluster.DeviceKey{{NodePublic: a.Public().String()}, {NodePublic: b.Public().String()}}
	applied, err := process.Policy.ApplyPolicy(ctx, p)
	if err != nil || applied.Revision != p.Revision || !applied.Usable || applied.EffectiveBudgetBPS != 80000000 {
		t.Fatal("real child did not acknowledge application", applied, err)
	}
	ca, err := derphttp.NewClient(a, "http://"+address+"/derp", logger.Discard, netmon.NewStatic())
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	cb, err := derphttp.NewClient(b, "http://"+address+"/derp", logger.Discard, netmon.NewStatic())
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	for _, c := range []*derphttp.Client{ca, cb} {
		if err := c.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	if err := ca.Send(b.Public(), []byte("actual patched relay")); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := cb.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if packet, ok := msg.(derp.ReceivedPacket); ok {
			if string(packet.Data) != "actual patched relay" || packet.Source != a.Public() {
				t.Fatal("relay payload mismatch")
			}
			break
		}
	}
	p.Revision++
	p.QoS.OwnerWeight = 9
	p.QoS.BudgetBPS = 200000000
	if result, err := process.Policy.ApplyPolicy(ctx, p); err != nil || result.EffectiveBudgetBPS != 80000000 {
		t.Fatal(result, err)
	}
	if err := ca.Send(b.Public(), []byte("hot policy")); err != nil {
		t.Fatal(err)
	}
	for {
		msg, err := cb.Recv()
		if err != nil {
			t.Fatal("rule update disconnected client", err)
		}
		if packet, ok := msg.(derp.ReceivedPacket); ok {
			if string(packet.Data) != "hot policy" {
				t.Fatal("hot update payload mismatch")
			}
			break
		}
	}
	status, err = process.Policy.Status(ctx)
	if err != nil || len(status.Traffic) != 1 || status.TrafficObservedAt.IsZero() || status.ActiveConnections != 2 {
		t.Fatal("actual child traffic snapshot missing", status, err)
	}
	traffic := status.Traffic[0]
	bytes := uint64(len("actual patched relay") + len("hot policy"))
	if traffic.CounterID == 0 || traffic.TailnetID != p.Grants[0].TailnetID || traffic.RXPayloadBytes != bytes || traffic.TXPayloadBytes != bytes || traffic.RelayedPayloadBytes != bytes || traffic.QueuedPayloadBytes != 0 {
		t.Fatal("actual child payload counters have wrong scope", traffic)
	}
	p.Revision++
	p.QoS.BudgetBPS = 40000000
	if result, err := process.Policy.ApplyPolicy(ctx, p); err != nil || result.EffectiveBudgetBPS != 40000000 {
		t.Fatal(result, err)
	}
	p.Revision++
	p.Grants[0].ControlUntil = time.Now().Add(300 * time.Millisecond)
	if _, err := process.Policy.ApplyPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		for {
			if _, err := cb.Recv(); err != nil {
				closed <- err
				return
			}
		}
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("child did not close expired connection without another update")
	}
	status, err = process.Policy.Status(ctx)
	if err != nil || status.Revision != p.Revision || status.Usable {
		t.Fatal("expired child still advertised usable policy", status, err)
	}
	stop, stopCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	err = process.Stop(stop)
	stopCancel()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(ctx, server, "127.0.0.1:9", filepath.Join(dir, "derper.key")); err != nil {
		t.Fatal(err)
	}
	if err := process.WaitReady(ctx, server); err != nil {
		t.Fatal(err)
	}
	status, err = process.Policy.Status(ctx)
	if err != nil || status.Revision != p.Revision || status.Usable {
		t.Fatal("child restart renewed expired cache", status, err)
	}
}
