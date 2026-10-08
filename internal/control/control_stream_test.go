package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func nodeRequest(t *testing.T, client *http.Client, url, method, token string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestControlStreamIsScopedAndSameSessionReconnects(t *testing.T) {
	s, _, _, n, _, session := registeredTestNode(t)
	server := httptest.NewServer(NewHTTPHandler(s))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	bad := nodeRequest(t, client, server.URL+"/cluster/v1/control?node_id=other", "GET", session.Token, nil)
	io.Copy(io.Discard, bad.Body)
	bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatal("node selected another scope", bad.StatusCode)
	}
	first := nodeRequest(t, client, server.URL+"/cluster/v1/control?applied_revision=0", "GET", session.Token, nil)
	defer first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatal(first.StatusCode)
	}
	scanner := bufio.NewScanner(first.Body)
	scanner.Buffer(make([]byte, 4096), cluster.MaxControlMessageBytes)
	if !scanner.Scan() {
		t.Fatal(scanner.Err())
	}
	var message cluster.ControlMessage
	if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	if message.Type != "policy" || message.Policy == nil || message.Policy.NodeID != n.ID || len(message.Policy.Grants) != 0 {
		t.Fatal("wrong initial scope")
	}
	second := nodeRequest(t, client, server.URL+"/cluster/v1/control?applied_revision=0", "GET", session.Token, nil)
	defer second.Body.Close()
	if second.StatusCode != 200 {
		t.Fatal("same session reconnect failed")
	}
	if scanner.Scan() {
		t.Fatal("replaced stream still active")
	}
}

func TestNodeACKSeparatesReceivedAndApplied(t *testing.T) {
	s, admin, _, n, _, session := registeredTestNode(t)
	server := httptest.NewServer(NewHTTPHandler(s))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	send := func(ack cluster.PolicyACK) int {
		r := nodeRequest(t, client, server.URL+"/cluster/v1/ack", "POST", session.Token, ack)
		defer r.Body.Close()
		io.Copy(io.Discard, r.Body)
		return r.StatusCode
	}
	if send(cluster.PolicyACK{Revision: 1, State: "applied", Usable: true}) != 409 {
		t.Fatal("unreceived policy applied")
	}
	if send(cluster.PolicyACK{Revision: 1, State: "received"}) != 200 {
		t.Fatal("receive rejected")
	}
	if send(cluster.PolicyACK{Revision: 1, State: "nack", Error: "apply_failed"}) != 200 {
		t.Fatal("safe NACK rejected")
	}
	var received, applied int
	s.db.QueryRow("SELECT received_revision,applied_revision FROM node_policies WHERE node_id=?", n.ID).Scan(&received, &applied)
	if received != 1 || applied != 0 {
		t.Fatal("receive claimed application")
	}
	if send(cluster.PolicyACK{Revision: 1, State: "applied", Usable: false}) != 200 {
		t.Fatal("apply rejected")
	}
	node, err := s.Node(t.Context(), admin, n.ID)
	if err != nil || node.State != "registered" {
		t.Fatal("empty deny policy advertised relay usable")
	}
	if send(cluster.PolicyACK{Revision: 2, State: "applied", Usable: true}) != 409 {
		t.Fatal("unknown revision applied")
	}
	if send(cluster.PolicyACK{Revision: 1, State: "nack", Error: "secret raw diagnostic"}) != 400 {
		t.Fatal("arbitrary diagnostics accepted")
	}
	if send(cluster.PolicyACK{Revision: 1, State: "nack", Error: "apply_failed"}) != 409 {
		t.Fatal("late NACK overwrote successful application")
	}
}

func TestControlStreamCancellationReleasesRegistration(t *testing.T) {
	s, _, _, n, _, session := registeredTestNode(t)
	server := httptest.NewServer(NewHTTPHandler(s))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/cluster/v1/control", nil)
	r.Header.Set("Authorization", "Bearer "+session.Token)
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	if _, err := reader.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.policyMu.Lock()
		_, active := s.policyStreams[n.ID]
		s.policyMu.Unlock()
		if !active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("canceled stream retained registration")
}

func TestControlStreamPublishesChangedPolicyAndPreservesResend(t *testing.T) {
	s, admin, _, n, _, session := registeredTestNode(t)
	server := httptest.NewServer(NewHTTPHandler(s))
	defer server.Close()
	response := nodeRequest(t, &http.Client{Timeout: 3 * time.Second}, server.URL+"/cluster/v1/control", "GET", session.Token, nil)
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), cluster.MaxControlMessageBytes)
	if !scanner.Scan() {
		t.Fatal(scanner.Err())
	}
	var first cluster.ControlMessage
	json.Unmarshal(scanner.Bytes(), &first)
	if _, err := s.db.Exec("UPDATE node_policies SET budget_bps=80000000 WHERE node_id=?", n.ID); err != nil {
		t.Fatal(err)
	}
	p, err := s.BuildPolicy(t.Context(), n.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() {
		t.Fatal(scanner.Err())
	}
	var next cluster.ControlMessage
	json.Unmarshal(scanner.Bytes(), &next)
	if next.Policy == nil || next.Policy.Revision != p.Revision || next.Policy.Revision <= first.Policy.Revision || next.Policy.QoS.BudgetBPS != 80000000 {
		t.Fatal("changed policy absent")
	}
	if err := s.SetRetentions(t.Context(), admin, Retentions{IdentitySeconds: 3600, ControlSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	again, err := s.BuildPolicy(t.Context(), n.ID, time.Now())
	if err != nil || again.Revision != p.Revision || !again.GeneratedAt.Equal(p.GeneratedAt) {
		t.Fatal("no-grant setting change rewrote empty policy")
	}
}
