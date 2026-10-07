package daemon

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/control"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebLeaveOfflineStopsRelayBeforeReturning(t *testing.T) {
	d := localTestDaemon(t, true)
	settings, _ := d.Status(t.Context())
	local := settings.Saved
	local.Role = "member"
	local.TLSMode, local.CertMode = "external", "none"
	if err := d.SaveSettings(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	dir := d.current.Node.StateDir
	key, _ := os.ReadFile(filepath.Join(dir, "node.key"))
	if _, err := d.Leave(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := d.Status(t.Context())
	if after.Joined || after.Control.Usable {
		t.Fatal(after)
	}
	reopened, err := config.LoadFile(d.configPath)
	if err != nil || reopened.Config.Node.ControllerURL != "" {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "node.key"))
	if !bytes.Equal(key, got) {
		t.Fatal("node key changed")
	}
}

func TestWebRegisterLocalReplacesRejectedPendingCode(t *testing.T) {
	d := localTestDaemon(t, false)
	d.derper = &daemonFakeProcess{}
	actor, err := d.controllerStore.InitializeAdmin(t.Context(), "localadmin", "long-password-123")
	if err != nil {
		t.Fatal(err)
	}
	cfg := d.current.Clone()
	cfg.Server.Hostname = "relay.example.com"
	d.current, d.desired = cfg, cfg
	node, _, err := d.controllerStore.CreateNode(t.Context(), actor, "Local relay", cfg.Server.Hostname, 443, 3478)
	if err != nil {
		t.Fatal(err)
	}
	key, err := cluster.LoadNodeIdentity(filepath.Join(cfg.Node.StateDir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	oldCode := strings.Repeat("e", 64)
	challenge := cluster.NodeChallenge{Purpose: "enroll", ClusterID: strings.Repeat("a", 64), NodeID: node.ID, Domain: cfg.Server.Hostname, PublicKey: key.Public().(ed25519.PublicKey), InstanceID: strings.Repeat("f", 64), Nonce: strings.Repeat("c", 64), ExpiresAt: time.Now().Add(-time.Minute)}
	old := cluster.EnrollmentRequest{Code: oldCode, Challenge: challenge, Signature: ed25519.Sign(key, challenge.SigningBytes())}
	var freshCode string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cluster/v1/enroll/challenge":
			var req struct {
				Code       string `json:"code"`
				PublicKey  []byte `json:"public_key"`
				InstanceID string `json:"instance_id"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Code == oldCode {
				w.WriteHeader(401)
				return
			}
			freshCode = req.Code
			ch, err := d.controllerStore.EnrollmentChallenge(r.Context(), req.Code, req.PublicKey, req.InstanceID)
			if err != nil {
				t.Error(err)
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(ch)
		case "/cluster/v1/enroll":
			var req cluster.EnrollmentRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.Code == oldCode {
				w.WriteHeader(401)
				return
			}
			if !ed25519.Verify(key.Public().(ed25519.PublicKey), req.Challenge.SigningBytes(), req.Signature) {
				t.Error("original identity not proven")
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(cluster.NodeSession{ClusterID: req.Challenge.ClusterID, NodeID: node.ID, InstanceID: req.Challenge.InstanceID, Token: strings.Repeat("d", 64), ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	registration, _ := json.Marshal(map[string]any{"controller_url": server.URL, "domain": cfg.Server.Hostname, "pending": old})
	if err := os.WriteFile(filepath.Join(cfg.Node.StateDir, "registration.json"), registration, 0600); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = previous }()
	d.nodeClient, err = cluster.NewEnrollmentClient(server.URL, cfg.Node.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RegisterLocal(t.Context(), control.Actor{Role: "member"}, "Local relay"); !errors.Is(err, control.ErrForbidden) {
		t.Fatal(err)
	}
	registered, err := d.RegisterLocal(t.Context(), actor, "Local relay")
	if err != nil || registered.ID != node.ID || freshCode == "" || freshCode == oldCode {
		t.Fatal(registered, err)
	}
	if _, err := d.RegisterLocal(t.Context(), actor, "Local relay"); err != nil {
		t.Fatal(err)
	}
	nodes, err := d.controllerStore.ListNodes(t.Context(), actor)
	if err != nil || len(nodes) != 1 {
		t.Fatal("retry duplicated resource", nodes, err)
	}
	if _, err := os.Stat(d.policyClient.Path); !os.IsNotExist(err) {
		t.Fatal("registration granted device permissions", err)
	}
}

func TestWebJoinUsesProofAndRetriesPendingReceipt(t *testing.T) {
	d := localTestDaemon(t, true)
	var challenge cluster.NodeChallenge
	var attempts int
	var offline bool
	var portWrites atomic.Int32
	controlOpened := make(chan struct{}, 1)
	controller := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/cluster/v1/node/ports":
			portWrites.Add(1)
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		case "/cluster/v1/control":
			select {
			case controlOpened <- struct{}{}:
			default:
			}
			http.NotFound(w, r)
		case "/cluster/v1/enroll/challenge":
			var req struct {
				PublicKey  []byte `json:"public_key"`
				InstanceID string `json:"instance_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			challenge = cluster.NodeChallenge{Purpose: "enroll", ClusterID: strings.Repeat("a", 64), NodeID: strings.Repeat("b", 64), Domain: "relay.example.com", PublicKey: req.PublicKey, InstanceID: req.InstanceID, Nonce: strings.Repeat("c", 64), ExpiresAt: time.Now().Add(time.Minute)}
			json.NewEncoder(w).Encode(challenge)
		case "/cluster/v1/enroll":
			var req cluster.EnrollmentRequest
			json.NewDecoder(r.Body).Decode(&req)
			if !ed25519.Verify(challenge.PublicKey, challenge.SigningBytes(), req.Signature) {
				t.Error("private key proof failed")
			}
			response, err := http.Get("http://" + d.managementListener.Addr().String() + "/cluster/v1/domain-challenge/" + challenge.Nonce)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			var proof cluster.DomainResponse
			json.NewDecoder(response.Body).Decode(&proof)
			response.Body.Close()
			if !ed25519.Verify(challenge.PublicKey, challenge.DomainSigningBytes(), proof.Signature) {
				t.Error("public domain proof failed")
			}
			attempts++
			if attempts == 1 {
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(cluster.NodeSession{ClusterID: challenge.ClusterID, NodeID: challenge.NodeID, InstanceID: challenge.InstanceID, Token: strings.Repeat("d", 64), ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)})
		case "/cluster/v1/node/leave":
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer controller.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = controller.Client().Transport
	defer func() { http.DefaultTransport = previous }()
	settings, _ := d.Status(t.Context())
	local := settings.Saved
	local.Role = "member"
	local.TLSMode, local.CertMode = "external", "none"
	if err := d.SaveSettings(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	code := strings.Repeat("e", 64)
	if err := d.Join(t.Context(), controller.URL, code); err == nil {
		t.Fatal("lost response accepted")
	}
	first := challenge
	if err := d.Join(t.Context(), controller.URL, code); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.PublicKey, challenge.PublicKey) || first.Nonce != challenge.Nonce {
		t.Fatal("pending proof replaced")
	}
	if portWrites.Load() != 1 {
		t.Fatal("join did not explicitly publish ports", portWrites.Load())
	}
	portWrites.Store(0)
	d.startNodeControl(d.runCtx)
	select {
	case <-controlOpened:
	case <-time.After(2 * time.Second):
		t.Fatal("control did not open")
	}
	d.stopNodeControl()
	if portWrites.Load() != 0 {
		t.Fatal("normal control startup overwrote resource ports", portWrites.Load())
	}
	for _, isOffline := range []bool{false, true} {
		if isOffline {
			if err := d.Join(t.Context(), controller.URL, code); err != nil {
				t.Fatal(err)
			}
		}
		offline = isOffline
		released, err := d.Leave(t.Context())
		if err != nil || released == isOffline {
			t.Fatal(released, err)
		}
		after, _ := d.Status(t.Context())
		reopened, err := cluster.NewEnrollmentClient("", d.current.Node.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		_, id := reopened.PolicyBinding()
		if after.Joined || id != "" {
			t.Fatal("exit not durable", after, id)
		}
	}
}
