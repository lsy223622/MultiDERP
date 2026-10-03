package cluster

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRenewSessionAcceptsExplicitDomainChallengeAndServesPrivateProof(t *testing.T) {
	for _, purpose := range []string{"domain_change", "session"} {
		t.Run(purpose, func(t *testing.T) {
			var c *EnrollmentClient
			var challenge NodeChallenge
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/cluster/v1/session/challenge":
					challenge = NodeChallenge{Purpose: purpose, ClusterID: c.state.Session.ClusterID, NodeID: c.state.Session.NodeID, Domain: "new.example.com", Nonce: strings.Repeat("d", 64), ExpiresAt: time.Now().Add(time.Minute), PublicKey: c.privateKey.Public().(ed25519.PublicKey), InstanceID: c.instanceID}
					json.NewEncoder(w).Encode(challenge)
				case "/cluster/v1/session":
					proof := httptest.NewRecorder()
					c.DomainHandler().ServeHTTP(proof, httptest.NewRequest("GET", "https://new.example.com/cluster/v1/domain-challenge/"+challenge.Nonce, nil))
					var body DomainResponse
					if err := json.Unmarshal(proof.Body.Bytes(), &body); err != nil || proof.Code != 200 || !ed25519.Verify(challenge.PublicKey, challenge.DomainSigningBytes(), body.Signature) {
						t.Error("domain response not primed", proof.Code, err)
					}
					json.NewEncoder(w).Encode(NodeSession{ClusterID: challenge.ClusterID, NodeID: challenge.NodeID, InstanceID: c.instanceID, Token: strings.Repeat("e", 64), ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var err error
			c, err = NewEnrollmentClient(server.URL, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c.httpClient.Transport = server.Client().Transport
			c.state.Domain = "old.example.com"
			c.state.Session = NodeSession{ClusterID: strings.Repeat("a", 64), NodeID: strings.Repeat("b", 64), InstanceID: c.instanceID}
			_, err = c.RenewSession(t.Context())
			if purpose == "domain_change" {
				if err != nil || c.state.Domain != "new.example.com" {
					t.Fatal("explicit domain validation failed", err)
				}
			} else if err == nil {
				t.Fatal("ordinary renewal silently changed domain")
			}
		})
	}
}

func TestControlClientRenewsRejectedSessionBeforeScheduledExpiry(t *testing.T) {
	p := liveControlPolicy()
	oldToken, newToken := strings.Repeat("c", 64), strings.Repeat("e", 64)
	var c *EnrollmentClient
	var renewals atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cluster/v1/control":
			if r.Header.Get("Authorization") == "Bearer "+oldToken {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(ControlMessage{Type: "policy", Policy: &p})
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/cluster/v1/session/challenge":
			json.NewEncoder(w).Encode(NodeChallenge{Purpose: "session", ClusterID: p.ClusterID, NodeID: p.NodeID, Domain: c.state.Domain, Nonce: strings.Repeat("d", 64), ExpiresAt: time.Now().Add(time.Minute), PublicKey: c.privateKey.Public().(ed25519.PublicKey), InstanceID: c.instanceID})
		case "/cluster/v1/session":
			renewals.Add(1)
			json.NewEncoder(w).Encode(NodeSession{ClusterID: p.ClusterID, NodeID: p.NodeID, InstanceID: c.instanceID, Token: newToken, ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)})
		case "/cluster/v1/heartbeat":
			json.NewEncoder(w).Encode(NodeHeartbeat{NodeID: p.NodeID, LeaseUntil: time.Now().Add(time.Minute), HeartbeatIntervalMillis: 30000})
		case "/cluster/v1/ack":
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
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
	c.state.Session = NodeSession{ClusterID: p.ClusterID, NodeID: p.NodeID, InstanceID: c.instanceID, Token: oldToken, ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)}
	ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
	defer cancel()
	var applied atomic.Bool
	err = c.RunControl(ctx, filepath.Join(t.TempDir(), "policy.json"), func(context.Context, Policy) (PolicyApplication, error) {
		applied.Store(true)
		return PolicyApplication{Revision: p.Revision, Usable: true}, nil
	}, func(context.Context) (PolicyApplication, error) { return PolicyApplication{}, nil })
	if !errors.Is(err, context.DeadlineExceeded) || renewals.Load() != 1 || !applied.Load() {
		t.Fatal("rejected session retried until expiry", renewals.Load(), applied.Load(), err)
	}
}

func liveControlPolicy() Policy {
	p := testPolicy()
	now := time.Now().UTC()
	p.GeneratedAt = now
	p.ClusterID = strings.Repeat("a", 64)
	p.NodeID = strings.Repeat("b", 64)
	p.Grants[0].LastIdentitySuccess = now
	p.Grants[0].IdentityUntil = now.Add(2 * time.Hour)
	p.Grants[0].ControlUntil = now.Add(time.Hour)
	p.Grants[0].Keys[0].NotAfter = now.Add(3 * time.Hour)
	return p
}

func TestControlHeartbeatsSendActualSampleRatherThanLastACK(t *testing.T) {
	observed := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	reports := make(chan NodeHeartbeatRequest, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request NodeHeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		reports <- request
		json.NewEncoder(w).Encode(NodeHeartbeat{NodeID: "node", LeaseUntil: observed.Add(time.Minute), HeartbeatIntervalMillis: 30000})
	}))
	defer server.Close()
	c, err := NewEnrollmentClient(server.URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient.Transport = server.Client().Transport
	c.controlStatus = ControlStatus{AppliedRevision: 1, Usable: true}
	session := NodeSession{NodeID: "node", Token: "token", ExpiresAt: observed.Add(time.Hour)}
	err = c.controlHeartbeats(ctx, session, func(context.Context) (PolicyApplication, error) {
		return PolicyApplication{Revision: 2, Usable: false, TrafficObservedAt: observed, Traffic: []TailnetTraffic{{TailnetID: "own", TXPayloadBytes: 456}}}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	request := <-reports
	if request.Report == nil || request.Report.Revision != 2 || request.Report.Usable || !request.Report.ObservedAt.Equal(observed) || len(request.Report.Traffic) != 1 || request.Report.Traffic[0].TXPayloadBytes != 456 {
		t.Fatal("heartbeat omitted actual runtime sample", request)
	}
}

func TestControlClientACKWaitsForActualApplication(t *testing.T) {
	p := liveControlPolicy()
	token := strings.Repeat("c", 64)
	acks := make(chan PolicyACK, 8)
	nacked := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || strings.Contains(r.URL.RawQuery, token) {
			t.Error("unsafe session transport")
		}
		switch r.URL.Path {
		case "/cluster/v1/control":
			json.NewEncoder(w).Encode(ControlMessage{Type: "policy", Policy: &p})
			w.(http.Flusher).Flush()
			select {
			case <-nacked:
			case <-r.Context().Done():
				return
			}
			updated := p
			updated.Revision++
			updated.QoS.BudgetBPS = 80000000
			json.NewEncoder(w).Encode(ControlMessage{Type: "policy", Policy: &updated})
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/cluster/v1/ack":
			var ack PolicyACK
			if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
				t.Error(err)
			}
			r.Body.Close()
			if ack.Error == "private application diagnostic" {
				t.Error("raw error leaked")
			}
			acks <- ack
			if ack.State == "nack" {
				nacked <- struct{}{}
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		case "/cluster/v1/heartbeat":
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			json.NewEncoder(w).Encode(NodeHeartbeat{NodeID: p.NodeID, LeaseUntil: time.Now().Add(90 * time.Second), DesiredRevision: 2, HeartbeatIntervalMillis: 30000})
		default:
			t.Error("unexpected URL")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, err := NewEnrollmentClient(server.URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient.Transport = server.Client().Transport
	c.state.Session = NodeSession{ClusterID: p.ClusterID, NodeID: p.NodeID, InstanceID: c.instanceID, Token: token, ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	path := filepath.Join(t.TempDir(), "policy.json")
	var calls atomic.Int32
	go func() {
		done <- c.RunControl(ctx, path, func(_ context.Context, p Policy) (PolicyApplication, error) {
			calls.Add(1)
			if p.Revision == 1 {
				return PolicyApplication{}, errors.New("private application diagnostic")
			}
			return PolicyApplication{Revision: p.Revision, Usable: true}, nil
		}, func(context.Context) (PolicyApplication, error) {
			return PolicyApplication{}, errors.New("no runtime sample")
		})
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	var observed []PolicyACK
	for {
		select {
		case ack := <-acks:
			observed = append(observed, ack)
			if ack.State == "applied" {
				if ack.Revision != 2 {
					t.Fatal("failed application acknowledged")
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				goto complete
			}
		case <-deadline.C:
			t.Fatal("application ACK absent")
		}
	}
complete:
	if calls.Load() != 2 || len(observed) != 4 || observed[0].State != "received" || observed[1].State != "nack" || observed[1].Error != "apply_failed" || observed[2].State != "received" {
		t.Fatal("wrong acknowledgment order", observed)
	}
	cached, err := LoadCache(path, p.ClusterID, p.NodeID, time.Now())
	if err != nil || cached.Revision != 2 {
		t.Fatal("applied snapshot not durable", err)
	}
}

func TestControlDisconnectKeepsOriginalCachedDeadlines(t *testing.T) {
	p := liveControlPolicy()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := SaveCache(path, p); err != nil {
		t.Fatal(err)
	}
	c, err := NewEnrollmentClient("https://127.0.0.1:1", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.state.Session = NodeSession{ClusterID: p.ClusterID, NodeID: p.NodeID, InstanceID: c.instanceID, Token: strings.Repeat("c", 64), ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var applied Policy
	err = c.RunControl(ctx, path, func(_ context.Context, p Policy) (PolicyApplication, error) {
		applied = p
		return PolicyApplication{Revision: p.Revision, Usable: true}, nil
	}, func(context.Context) (PolicyApplication, error) {
		return PolicyApplication{}, errors.New("no runtime sample")
	})
	if !errors.Is(err, context.DeadlineExceeded) || applied.Revision != p.Revision || !applied.Grants[0].ControlUntil.Equal(p.Grants[0].ControlUntil) {
		t.Fatal("disconnect changed cached authorization", err)
	}
}

func TestControlRevocationAppliesWhenACKIsRejected(t *testing.T) {
	p := liveControlPolicy()
	p.Grants = []GrantPolicy{}
	p.QoS.Tailnets = []TailnetQoS{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cluster/v1/control":
			json.NewEncoder(w).Encode(ControlMessage{Type: "policy", Policy: &p})
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/cluster/v1/heartbeat":
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			json.NewEncoder(w).Encode(NodeHeartbeat{NodeID: p.NodeID, LeaseUntil: time.Now().Add(90 * time.Second), DesiredRevision: p.Revision, HeartbeatIntervalMillis: 30000})
		default:
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	c, err := NewEnrollmentClient(server.URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient.Transport = server.Client().Transport
	c.state.Session = NodeSession{ClusterID: p.ClusterID, NodeID: p.NodeID, InstanceID: c.instanceID, Token: strings.Repeat("c", 64), ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	var calls int
	err = c.RunControl(ctx, filepath.Join(t.TempDir(), "policy.json"), func(ctx context.Context, got Policy) (PolicyApplication, error) {
		if err := ctx.Err(); err != nil {
			return PolicyApplication{}, err
		}
		calls++
		if len(got.Grants) != 0 {
			t.Fatal("revocation lost")
		}
		return PolicyApplication{Revision: got.Revision}, nil
	}, func(context.Context) (PolicyApplication, error) {
		return PolicyApplication{}, errors.New("no runtime sample")
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || c.ControlStatus().AppliedRevision != p.Revision {
		t.Fatal("ACK failure prevented revocation application", calls, err)
	}
}
