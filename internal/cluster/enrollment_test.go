package cluster

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNodeChallengeBindsPurposeClusterNodeAndInstance(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := NodeChallenge{Purpose: "enroll", ClusterID: "cluster", NodeID: "node", Domain: "relay.example.com", Nonce: "nonce", ExpiresAt: time.Now().Add(time.Minute), PublicKey: pub, InstanceID: "instance"}
	signature := ed25519.Sign(priv, c.SigningBytes())
	if !ed25519.Verify(pub, c.SigningBytes(), signature) {
		t.Fatal("valid signature rejected")
	}
	for _, change := range []func(*NodeChallenge){func(c *NodeChallenge) { c.Purpose = "session" }, func(c *NodeChallenge) { c.ClusterID = "other" }, func(c *NodeChallenge) { c.NodeID = "other" }, func(c *NodeChallenge) { c.InstanceID = "clone" }, func(c *NodeChallenge) { c.Domain = "attacker.example.com" }, func(c *NodeChallenge) { c.Nonce = "different" }} {
		modified := c
		change(&modified)
		if ed25519.Verify(pub, modified.SigningBytes(), signature) {
			t.Fatal("signature not context-bound")
		}
	}
}

func TestNodeDomainResponseOnlyServesPendingChallenge(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h := NewDomainResponder(priv)
	c := NodeChallenge{Purpose: "enroll", ClusterID: "cluster", NodeID: "node", Domain: "relay.example.com", Nonce: "abcdef", ExpiresAt: time.Now().Add(time.Minute), PublicKey: pub, InstanceID: "instance"}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/cluster/v1/domain-challenge/abcdef", nil))
	if w.Code != 404 {
		t.Fatal("no pending challenge served")
	}
	h.SetChallenge(c)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/cluster/v1/domain-challenge/abcdef", nil))
	var response DomainResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || !ed25519.Verify(pub, c.DomainSigningBytes(), response.Signature) {
		t.Fatal("missing bound domain proof")
	}
	for _, path := range []string{"/cluster/v1/domain-challenge/other", "/cluster/v1/domain-challenge/abcdef?secret=value", "/api/v1/session"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 404 {
			t.Fatal("unexpected challenge path exposed")
		}
	}
	c.ExpiresAt = time.Now().Add(-time.Minute)
	h.SetChallenge(c)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/cluster/v1/domain-challenge/abcdef", nil))
	if w.Code != 404 {
		t.Fatal("expired challenge served")
	}
}
