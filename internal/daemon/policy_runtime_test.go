package daemon

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/derper"
	"tailscale.com/derp/derphttp"
	"tailscale.com/net/netmon"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func TestUnregisteredControllerStartsFailClosedPublicRelay(t *testing.T) {
	binary := os.Getenv("UNIDERP_TEST_DERPER")
	if binary == "" {
		t.Skip("requires freshly built patched derper")
	}
	dir := shortTempDir(t)
	cfg := config.Default()
	cfg.Server.Hostname = "relay.example.com"
	cfg.Storage.StateDir = dir
	cfg.Node.StateDir = filepath.Join(dir, "node")
	cfg.Server.Admin.Socket = filepath.Join(dir, "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Server.DERP.Listen = freeLoopbackAddress(t)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.DERP.STUNListen = udp.LocalAddr().String()
	udp.Close()
	cfg.Controller.Listen = freeLoopbackAddress(t)
	cfg.Controller.Database = filepath.Join(dir, "controller.sqlite")
	cfg.Controller.KeyFile = filepath.Join(dir, "controller.key")
	path := filepath.Join(dir, "config.yaml")
	if err := config.WriteAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	foreign := daemonPolicy(t)
	cachePath := filepath.Join(cfg.Node.StateDir, "policy.json")
	if err := cluster.SaveCache(cachePath, foreign); err != nil {
		t.Fatal(err)
	}
	d := New(t.Context(), Options{ConfigPath: path, DerperBinary: binary, Logger: log.New(io.Discard, "", 0)})
	if err := d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer d.Shutdown()
	if !d.derper.Running() {
		t.Fatal("empty authorization prevented public management/proof routing")
	}
	c := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
	for route, want := range map[string]int{"/derp/probe": 200, "/cluster/v1/domain-challenge/unknown": 404} {
		r, err := c.Get("http://" + cfg.Server.DERP.Listen + route)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatal(route, r.StatusCode)
		}
	}
	state := d.healthSnapshot()
	if !state.Live || state.Ready || state.DerperUsable {
		t.Fatal("unregistered controller marked usable", state)
	}
	if d.nodeClient == nil {
		t.Fatal("controller relay did not initialize common node identity")
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatal("unregistered node retained foreign authorization", err)
	}
	if _, err := os.Stat(cachePath + ".watermark"); err != nil {
		t.Fatal("discard reset revision watermark", err)
	}
}

func TestLocalConsoleLeaveClosesExistingDERPConnections(t *testing.T) {
	binary := os.Getenv("UNIDERP_TEST_DERPER")
	if binary == "" {
		t.Skip("requires freshly built patched derper")
	}
	d := localTestDaemon(t, true)
	cfg := d.desiredConfig()
	cfg.SetupRequired = false
	cfg.Controller = &config.ControllerConfig{Enabled: false}
	cfg.Server.Hostname = "relay.example.com"
	cfg.Server.DERP.Listen = freeLoopbackAddress(t)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.DERP.STUNListen = udp.LocalAddr().String()
	udp.Close()
	cfg.Node.ControllerURL = "https://127.0.0.1:1"
	p := daemonPolicy(t)
	device := key.NewNode()
	p.Grants[0].Keys = []cluster.DeviceKey{{NodePublic: device.Public().String()}}
	if _, err := cluster.LoadNodeIdentity(filepath.Join(cfg.Node.StateDir, "node.key")); err != nil {
		t.Fatal(err)
	}
	registration, _ := json.Marshal(map[string]any{"controller_url": cfg.Node.ControllerURL, "domain": cfg.Server.Hostname, "session": cluster.NodeSession{ClusterID: p.ClusterID, NodeID: p.NodeID, Token: strings.Repeat("c", 64), ExpiresAt: time.Now().Add(time.Hour)}})
	if err := os.WriteFile(filepath.Join(cfg.Node.StateDir, "registration.json"), registration, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cluster.SaveCache(filepath.Join(cfg.Node.StateDir, "policy.json"), p); err != nil {
		t.Fatal(err)
	}
	if err := d.saveLocalConfig(cfg); err != nil {
		t.Fatal(err)
	}
	d.derper = derper.NewProcess(binary, io.Discard)
	if err := d.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	client, err := derphttp.NewClient(device, "http://"+cfg.Server.DERP.Listen+"/derp", logger.Discard, netmon.NewStatic())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Recv(); err != nil {
		t.Fatal("authorized connection", err)
	}
	if _, err := d.Leave(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Recv(); err == nil {
		t.Fatal("existing connection survived leave")
	}
	if err := d.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := d.Status(t.Context())
	if after.Joined || after.Control.Usable {
		t.Fatal(after)
	}
	old, err := derphttp.NewClient(device, "http://"+cfg.Server.DERP.Listen+"/derp", logger.Discard, netmon.NewStatic())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := old.Connect(t.Context()); err == nil {
		if _, err := old.Recv(); err == nil {
			t.Fatal("old device admitted after restart")
		}
	}
	response, err := http.Get("http://" + d.managementListener.Addr().String() + "/manage/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("management unavailable", response.StatusCode)
	}
}

func daemonPolicy(t *testing.T) cluster.Policy {
	t.Helper()
	body, err := os.ReadFile("../cluster/testdata/policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var p cluster.Policy
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	p.ClusterID, p.NodeID = strings.Repeat("a", 64), strings.Repeat("b", 64)
	p.GeneratedAt = time.Now().UTC()
	p.Grants[0].LastIdentitySuccess = p.GeneratedAt
	p.Grants[0].IdentityUntil = p.GeneratedAt.Add(time.Hour)
	p.Grants[0].ControlUntil = p.GeneratedAt.Add(time.Hour)
	p.Grants[0].ExplicitUntil = time.Time{}
	for i := range p.Grants[0].Keys {
		p.Grants[0].Keys[i].NotAfter = time.Time{}
	}
	return p
}

func TestDaemonControlAppliesAndExpiresCachedPolicy(t *testing.T) {
	binary := os.Getenv("UNIDERP_TEST_DERPER")
	if binary == "" {
		t.Skip("requires freshly built patched derper")
	}
	dir := shortTempDir(t)
	cfg := config.Default()
	cfg.Controller = &config.ControllerConfig{Enabled: false}
	cfg.Server.Hostname = "relay.example.com"
	cfg.Server.Admin.Socket = filepath.Join(dir, "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Server.DERP.Listen = freeLoopbackAddress(t)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.DERP.STUNListen = udp.LocalAddr().String()
	udp.Close()
	cfg.Storage.StateDir, cfg.Node.StateDir = dir, filepath.Join(dir, "node")
	private, err := cluster.LoadNodeIdentity(filepath.Join(cfg.Node.StateDir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	p := daemonPolicy(t)
	token := strings.Repeat("c", 64)
	applied := make(chan cluster.PolicyACK, 1)
	var offline atomic.Bool
	controller := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/cluster/v1/session/challenge":
			var req struct {
				NodeID     string `json:"node_id"`
				InstanceID string `json:"instance_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NodeID != p.NodeID {
				t.Error("invalid session challenge request", err)
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(cluster.NodeChallenge{Purpose: "session", ClusterID: p.ClusterID, NodeID: p.NodeID, Domain: cfg.Server.Hostname, Nonce: strings.Repeat("d", 64), ExpiresAt: time.Now().Add(time.Minute), PublicKey: private.Public().(ed25519.PublicKey), InstanceID: req.InstanceID})
		case "/cluster/v1/session":
			var req struct {
				Challenge cluster.NodeChallenge `json:"challenge"`
				Signature []byte                `json:"signature"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !ed25519.Verify(private.Public().(ed25519.PublicKey), req.Challenge.SigningBytes(), req.Signature) {
				t.Error("session private proof invalid", err)
				w.WriteHeader(403)
				return
			}
			json.NewEncoder(w).Encode(cluster.NodeSession{ClusterID: p.ClusterID, NodeID: p.NodeID, InstanceID: req.Challenge.InstanceID, Token: token, ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)})
		case "/cluster/v1/control":
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("unbound control request")
			}
			policy := p
			policy.Grants[0].ControlUntil = time.Now().Add(20 * time.Second)
			json.NewEncoder(w).Encode(cluster.ControlMessage{Type: "policy", Policy: &policy})
			w.(http.Flusher).Flush()
			select {
			case <-applied:
				offline.Store(true)
			case <-r.Context().Done():
			}
		case "/cluster/v1/heartbeat":
			io.Copy(io.Discard, r.Body)
			json.NewEncoder(w).Encode(cluster.NodeHeartbeat{NodeID: p.NodeID, LeaseUntil: time.Now().Add(time.Minute), DesiredRevision: p.Revision, HeartbeatIntervalMillis: 30000})
		case "/cluster/v1/ack":
			var ack cluster.PolicyACK
			if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
				t.Error(err)
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			if ack.State == "applied" {
				applied <- ack
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer controller.Close()
	transport := http.DefaultTransport
	http.DefaultTransport = controller.Client().Transport
	defer func() { http.DefaultTransport = transport }()
	cfg.Node.ControllerURL = controller.URL
	cfg.Node.MaxBudgetBPS = 80000000
	registration, _ := json.Marshal(map[string]any{"controller_url": controller.URL, "domain": cfg.Server.Hostname, "session": cluster.NodeSession{ClusterID: p.ClusterID, NodeID: p.NodeID}})
	if err := os.WriteFile(filepath.Join(cfg.Node.StateDir, "registration.json"), registration, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := config.WriteAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	d := New(t.Context(), Options{ConfigPath: path, DerperBinary: binary, Logger: log.New(io.Discard, "", 0)})
	if err := d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer d.Shutdown()
	deadline := time.Now().Add(4 * time.Second)
	for !offline.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	state := d.healthSnapshot()
	if !offline.Load() || !state.Ready || !state.DerperUsable || state.Node.AppliedRevision != p.Revision || state.Node.ReceivedRevision != p.Revision {
		t.Fatal("daemon did not apply actual child policy", state)
	}
	if len(state.Node.Traffic) != 1 || state.Node.TrafficObservedAt.IsZero() {
		t.Fatal("health did not sample actual child traffic", state.Node)
	}
	before, err := os.ReadFile(d.policyClient.Path)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := d.Status(t.Context())
	if local.EffectiveBudgetBPS != 80000000 {
		t.Fatal(local)
	}
	settings := local.Saved
	settings.MaxBudgetBPS = 40000000
	if err := d.SaveSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	local, _ = d.Status(t.Context())
	after, _ := os.ReadFile(d.policyClient.Path)
	if local.EffectiveBudgetBPS != 40000000 || !bytes.Equal(before, after) || !local.Control.Usable {
		t.Fatal("offline local limit apply", local)
	}
	deadline = time.Now().Add(21 * time.Second)
	for d.healthSnapshot().Ready && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	state = d.healthSnapshot()
	if state.Ready || state.DerperUsable || !state.Live || !d.derper.Running() {
		t.Fatal("offline expiry stopped management or kept stale usability", state)
	}
}
