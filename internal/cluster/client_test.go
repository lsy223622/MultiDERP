package cluster

import (
	"context"
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
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || c.ControlStatus().AppliedRevision != p.Revision {
		t.Fatal("ACK failure prevented revocation application", calls, err)
	}
}
