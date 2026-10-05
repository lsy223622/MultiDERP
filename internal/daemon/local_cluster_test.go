package daemon

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebLeaveOfflineStopsRelayBeforeReturning(t *testing.T) {
	d := localTestDaemon(t, true)
	settings, _ := d.Status(t.Context())
	local := settings.Saved
	local.Role = "member"
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

func TestWebJoinUsesProofAndRetriesPendingReceipt(t *testing.T) {
	d := localTestDaemon(t, true)
	var challenge cluster.NodeChallenge
	var attempts int
	var offline bool
	controller := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
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
