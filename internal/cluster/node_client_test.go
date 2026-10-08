package cluster

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNodeIdentityPersistsAndRejectsCorruption(t *testing.T) {
	p := filepath.Join(t.TempDir(), "node.key")
	a, err := LoadNodeIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadNodeIdentity(p)
	if err != nil || !a.Equal(b) {
		t.Fatal("key changed")
	}
	if err := os.WriteFile(p, []byte("truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNodeIdentity(p); err == nil {
		t.Fatal("replaced corrupt identity")
	}
}

func TestExpiredPendingProofObtainsFreshChallenge(t *testing.T) {
	var challenge NodeChallenge
	var calls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cluster/v1/enroll/challenge":
			var req struct {
				PublicKey  []byte `json:"public_key"`
				InstanceID string `json:"instance_id"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			calls++
			challenge = NodeChallenge{Purpose: "enroll", ClusterID: strings.Repeat("a", 64), NodeID: strings.Repeat("b", 64), Domain: "relay.example.com", PublicKey: req.PublicKey, InstanceID: req.InstanceID, Nonce: strings.Repeat("c", 64), ExpiresAt: time.Now().Add(time.Minute)}
			json.NewEncoder(w).Encode(challenge)
		case "/cluster/v1/enroll":
			var req EnrollmentRequest
			json.NewDecoder(r.Body).Decode(&req)
			if time.Now().After(req.Challenge.ExpiresAt) {
				w.WriteHeader(401)
				return
			}
			if calls == 1 {
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(NodeSession{ClusterID: challenge.ClusterID, NodeID: challenge.NodeID, InstanceID: challenge.InstanceID, Token: strings.Repeat("d", 64), ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(time.Minute)})
		}
	}))
	defer server.Close()
	c, err := NewEnrollmentClient(server.URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient.Transport = server.Client().Transport
	code := strings.Repeat("e", 64)
	if _, err := c.Enroll(t.Context(), code); err == nil {
		t.Fatal("failed response")
	}
	c.state.Pending.Challenge.ExpiresAt = time.Now().Add(-time.Minute)
	c.state.Pending.Signature = ed25519.Sign(c.privateKey, c.state.Pending.Challenge.SigningBytes())
	if err := c.saveRegistration(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enroll(t.Context(), code); err != nil || calls != 2 {
		t.Fatal("expired proof pinned", err, calls)
	}
}

func TestEnrollmentCodeRejectionIsDistinguishedFromUnavailableController(t *testing.T) {
	response := http.StatusUnauthorized
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(response) }))
	defer server.Close()
	c, err := NewEnrollmentClient(server.URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient.Transport = server.Client().Transport
	if _, err := c.Enroll(t.Context(), strings.Repeat("e", 64)); !errors.Is(err, ErrEnrollmentCodeRejected) {
		t.Fatal(err)
	}
	response = http.StatusServiceUnavailable
	if _, err := c.Enroll(t.Context(), strings.Repeat("e", 64)); err == nil || errors.Is(err, ErrEnrollmentCodeRejected) {
		t.Fatal(err)
	}
}

func TestNodeClientUsesVerifiedTLSAndBoundEnrollment(t *testing.T) {
	var c NodeChallenge
	var session NodeSession
	var handler *EnrollmentClient
	var attempts int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Error("code in query")
		}
		switch r.URL.Path {
		case "/cluster/v1/enroll/challenge":
			var body struct {
				Code       string `json:"code"`
				PublicKey  []byte `json:"public_key"`
				InstanceID string `json:"instance_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			c = NodeChallenge{Purpose: "enroll", ClusterID: strings.Repeat("a", 64), NodeID: strings.Repeat("b", 64), Domain: "relay.example.com", Nonce: strings.Repeat("c", 64), ExpiresAt: time.Now().Add(time.Minute), PublicKey: body.PublicKey, InstanceID: body.InstanceID}
			json.NewEncoder(w).Encode(c)
		case "/cluster/v1/enroll":
			var req EnrollmentRequest
			json.NewDecoder(r.Body).Decode(&req)
			if !ed25519.Verify(c.PublicKey, c.SigningBytes(), req.Signature) {
				t.Error("private key not proven")
			}
			rw := httptest.NewRecorder()
			handler.DomainHandler().ServeHTTP(rw, httptest.NewRequest("GET", "https://relay.example.com/cluster/v1/domain-challenge/"+c.Nonce, nil))
			var proof DomainResponse
			json.Unmarshal(rw.Body.Bytes(), &proof)
			if rw.Code != 200 || !ed25519.Verify(c.PublicKey, c.DomainSigningBytes(), proof.Signature) {
				t.Error("domain response not active")
			}
			attempts++
			if attempts == 1 {
				w.WriteHeader(503)
				return
			}
			session = NodeSession{ClusterID: c.ClusterID, NodeID: c.NodeID, InstanceID: c.InstanceID, Token: strings.Repeat("d", 64), ExpiresAt: time.Now().Add(time.Hour), LeaseUntil: time.Now().Add(90 * time.Second)}
			json.NewEncoder(w).Encode(session)
		default:
			t.Error("unexpected URL")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	untrusted, err := NewEnrollmentClient(server.URL, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Enroll(context.Background(), strings.Repeat("e", 64)); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	handler, err = NewEnrollmentClient(server.URL, dir)
	if err != nil {
		t.Fatal(err)
	}
	handler.httpClient.Transport = server.Client().Transport
	if _, err := handler.Enroll(t.Context(), strings.Repeat("e", 64)); err == nil {
		t.Fatal("failed enrollment returned success")
	}
	first := c
	handler, err = NewEnrollmentClient(server.URL, dir)
	if err != nil {
		t.Fatal(err)
	}
	handler.httpClient.Transport = server.Client().Transport
	got, err := handler.Enroll(t.Context(), strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeID != first.NodeID || c.Nonce != first.Nonce || c.InstanceID != first.InstanceID {
		t.Fatal("retry changed registration")
	}
	reopened, err := NewEnrollmentClient(server.URL, dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.state.Session.Token != session.Token || reopened.state.Pending != nil {
		t.Fatal("session not persisted")
	}
	if err := os.Remove(filepath.Join(dir, "node.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEnrollmentClient(server.URL, dir); err == nil {
		t.Fatal("missing enrolled identity replaced")
	}
	for _, url := range []string{"http://controller.example.com", "https://user:pass@controller.example.com", "https://controller.example.com/path", "https://controller.example.com?code=secret", "https://controller.example.com#fragment"} {
		if _, err := NewEnrollmentClient(url, t.TempDir()); err == nil {
			t.Fatal("unsafe origin accepted", url)
		}
	}
}
