package cluster

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

type NodeChallenge struct {
	Purpose    string    `json:"purpose"`
	ClusterID  string    `json:"cluster_id"`
	NodeID     string    `json:"node_id"`
	Domain     string    `json:"domain"`
	Nonce      string    `json:"nonce"`
	ExpiresAt  time.Time `json:"expires_at"`
	PublicKey  []byte    `json:"public_key"`
	InstanceID string    `json:"instance_id"`
}

func (c NodeChallenge) SigningBytes() []byte {
	b, _ := json.Marshal(c)
	return b
}

func (c NodeChallenge) DomainSigningBytes() []byte {
	c.Purpose = "domain"
	return c.SigningBytes()
}

type EnrollmentRequest struct {
	Code      string        `json:"code"`
	Challenge NodeChallenge `json:"challenge"`
	Signature []byte        `json:"signature"`
}

type NodeSession struct {
	ClusterID  string    `json:"cluster_id"`
	NodeID     string    `json:"node_id"`
	InstanceID string    `json:"instance_id"`
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type DomainResponse struct {
	Signature []byte `json:"signature"`
}

type DomainResponder struct {
	mu         sync.RWMutex
	privateKey ed25519.PrivateKey
	challenge  NodeChallenge
}

func NewDomainResponder(privateKey ed25519.PrivateKey) *DomainResponder {
	return &DomainResponder{privateKey: append(ed25519.PrivateKey(nil), privateKey...)}
}

func (h *DomainResponder) SetChallenge(c NodeChallenge) {
	h.mu.Lock()
	h.challenge = c
	h.mu.Unlock()
}

func (h *DomainResponder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	c := h.challenge
	h.mu.RUnlock()
	if r.Method != "GET" || r.URL.RawQuery != "" || c.Nonce == "" || r.URL.Path != "/cluster/v1/domain-challenge/"+c.Nonce || !time.Now().Before(c.ExpiresAt) || strings.Contains(c.Nonce, "/") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(DomainResponse{Signature: ed25519.Sign(h.privateKey, c.DomainSigningBytes())})
}
