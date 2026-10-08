package cluster

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNodeClientSessionRenewalChecksEveryBinding(t *testing.T) {
	for _, mutation := range []string{"cluster", "node", "domain", "public_key", "purpose", "instance", "session", "valid"} {
		t.Run(mutation, func(t *testing.T) {
			var c *EnrollmentClient
			var ch NodeChallenge
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/cluster/v1/session/challenge":
					ch = NodeChallenge{Purpose: "session", ClusterID: c.state.Session.ClusterID, NodeID: c.state.Session.NodeID, Domain: c.state.Domain, PublicKey: c.privateKey.Public().(ed25519.PublicKey), Nonce: strings.Repeat("a", 64), InstanceID: c.instanceID, ExpiresAt: time.Now().Add(time.Minute)}
					switch mutation {
					case "cluster":
						ch.ClusterID = strings.Repeat("b", 64)
					case "node":
						ch.NodeID = strings.Repeat("b", 64)
					case "domain":
						ch.Domain = "other.example.com"
					case "public_key":
						ch.PublicKey = make([]byte, 32)
					case "purpose":
						ch.Purpose = "enroll"
					case "instance":
						ch.InstanceID = "other"
					}
					json.NewEncoder(w).Encode(ch)
				case "/cluster/v1/session":
					var body struct {
						Challenge NodeChallenge `json:"challenge"`
						Signature []byte        `json:"signature"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					if !ed25519.Verify(ch.PublicKey, ch.SigningBytes(), body.Signature) {
						t.Error("missing private proof")
					}
					session := NodeSession{ClusterID: ch.ClusterID, NodeID: ch.NodeID, InstanceID: ch.InstanceID, Token: strings.Repeat("c", 64), ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(90 * time.Second)}
					if mutation == "session" {
						session.NodeID = strings.Repeat("d", 64)
					}
					json.NewEncoder(w).Encode(session)
				default:
					t.Error("unexpected path")
				}
			}))
			defer server.Close()
			var err error
			c, err = NewEnrollmentClient(server.URL, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c.httpClient.Transport = server.Client().Transport
			c.state.Domain = "relay.example.com"
			c.state.Session = NodeSession{ClusterID: strings.Repeat("e", 64), NodeID: strings.Repeat("f", 64), Token: strings.Repeat("9", 64)}
			session, err := c.RenewSession(t.Context())
			if mutation == "valid" {
				if err != nil || session.Token != strings.Repeat("c", 64) {
					t.Fatal("valid renewal failed", err)
				}
			} else if err == nil || c.state.Session.Token != strings.Repeat("9", 64) {
				t.Fatal("changed binding accepted")
			}
		})
	}
}
