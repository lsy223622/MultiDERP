package control

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func TestNodeDomainInvalidHostnameReturnsFieldError(t *testing.T) {
	s, _, _ := nodeTestStore(t)
	h := NewHTTPHandler(s)
	cookie, csrf := loginTest(t, h, "admin")
	w := accountRequest(h, cookie, csrf, "POST", "/api/v1/nodes", map[string]string{"display_name": "Relay", "domain": "https://derp.example.com/"})
	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 400 || body["code"] != "invalid_hostname" || body["field"] != "domain" {
		t.Fatalf("want safe hostname field error: %d %s", w.Code, w.Body)
	}
}

func clusterRequest(h http.Handler, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "https://controller.example.com"+path, bytes.NewReader(b))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNodeHTTPSeparatesOwnerAndNodeAuthentication(t *testing.T) {
	s, admin, _ := nodeTestStore(t)
	h := NewHTTPHandler(s)
	cookie, csrf := loginTest(t, h, "admin")
	w := accountRequest(h, cookie, csrf, "POST", "/api/v1/nodes", map[string]string{"display_name": "Relay", "domain": "relay.example.com"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var created struct {
		Node       Node       `json:"node"`
		Enrollment Enrollment `json:"enrollment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Node.OwnerID != admin.ID {
		t.Fatal("owner not bound")
	}
	memberCookie, memberCSRF := loginTest(t, h, "alice")
	for _, path := range []string{"/api/v1/nodes/" + created.Node.ID, "/api/v1/nodes/" + created.Node.ID + "/enrollment"} {
		method := "GET"
		if strings.HasSuffix(path, "/enrollment") {
			method = "POST"
		}
		if got := accountRequest(h, memberCookie, memberCSRF, method, path, nil); got.Code != 403 {
			t.Fatal("other owner accessed node", got.Code)
		}
	}
	if got := accountRequest(h, cookie, csrf, "POST", "/api/v1/nodes", map[string]string{"display_name": "Other", "domain": "other.example.com", "owner_id": admin.ID}); got.Code != 400 {
		t.Fatal("client supplied owner accepted")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	w = clusterRequest(h, "/cluster/v1/enroll/challenge", map[string]any{"code": created.Enrollment.Code, "public_key": pub, "instance_id": "instance"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var challenge cluster.NodeChallenge
	json.Unmarshal(w.Body.Bytes(), &challenge)
	w = clusterRequest(h, "/cluster/v1/enroll", cluster.EnrollmentRequest{Code: created.Enrollment.Code, Challenge: challenge, Signature: ed25519.Sign(priv, challenge.SigningBytes())})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var session cluster.NodeSession
	json.Unmarshal(w.Body.Bytes(), &session)
	r := httptest.NewRequest("GET", "https://controller.example.com/api/v1/users", nil)
	r.Header.Set("Authorization", "Bearer "+session.Token)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, r)
	if recorder.Code != 401 {
		t.Fatal("node gained browser privilege")
	}
	w = accountRequest(h, cookie, csrf, "GET", "/api/v1/nodes/"+created.Node.ID, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), created.Enrollment.Code) || strings.Contains(w.Body.String(), session.Token) || strings.Contains(w.Body.String(), "public_key") {
		t.Fatal("unsafe node response")
	}
	var node Node
	json.Unmarshal(w.Body.Bytes(), &node)
	if node.State != "registered" {
		t.Fatal("registration advertised readiness")
	}
}

func TestEnrollmentHTTPAttemptLimit(t *testing.T) {
	s, _, _ := nodeTestStore(t)
	h := NewHTTPHandler(s)
	for i := 0; i < 31; i++ {
		w := clusterRequest(h, "/cluster/v1/enroll/challenge", map[string]any{"code": strings.Repeat("a", 64), "public_key": make([]byte, 32), "instance_id": "instance"})
		want := 401
		if i == 30 {
			want = 429
		}
		if w.Code != want {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
}
