package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type localTestBackend struct {
	role   string
	writes int
}

func (b *localTestBackend) Role() string { return b.role }
func (b *localTestBackend) Status(context.Context) (LocalStatus, error) {
	return LocalStatus{Role: b.role}, nil
}
func (b *localTestBackend) SaveSettings(context.Context, LocalSettings) error { b.writes++; return nil }
func (b *localTestBackend) ApplySettings(context.Context) error               { b.writes++; return nil }
func (b *localTestBackend) UploadCertificate(context.Context, string, string) error {
	b.writes++
	return nil
}
func (b *localTestBackend) Join(context.Context, string, string) error { b.writes++; return nil }
func (b *localTestBackend) Leave(context.Context) (bool, error)        { b.writes++; return true, nil }
func (b *localTestBackend) RegisterLocal(context.Context, Actor, string) (Node, error) {
	b.writes++
	return Node{}, nil
}

func TestLocalManagementRequiresLocalAdminAndCSRF(t *testing.T) {
	s, _, _ := nodeTestStore(t)
	b := &localTestBackend{role: "controller"}
	h := NewLocalHTTPHandler(s, b)
	adminCookie, adminCSRF := loginTest(t, h, "admin")
	memberCookie, memberCSRF := loginTest(t, h, "alice")
	path := "/api/v1/local/apply"
	anonymous := httptest.NewRecorder()
	h.ServeHTTP(anonymous, httptest.NewRequest("POST", "https://controller.example.com"+path, nil))
	if anonymous.Code != 401 {
		t.Fatal(anonymous.Code)
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		csrf   string
		want   int
	}{
		{memberCookie, memberCSRF, 403}, {adminCookie, "", 403},
	} {
		if w := accountRequest(h, tc.cookie, tc.csrf, "POST", path, nil); w.Code != tc.want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	other, _, _ := nodeTestStore(t)
	foreignCookie, foreignCSRF := loginTest(t, NewHTTPHandler(other), "admin")
	if w := accountRequest(h, foreignCookie, foreignCSRF, "POST", path, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "https://controller.example.com"+path, strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || b.writes != 0 {
		t.Fatal("local boundary bypassed", w.Code, b.writes)
	}
	if w := accountRequest(h, adminCookie, adminCSRF, "POST", path, nil); w.Code != 200 || b.writes != 1 {
		t.Fatal(w.Code, b.writes)
	}
}

func TestMemberManagementDoesNotExposeControllerAPIs(t *testing.T) {
	s, admin, _ := nodeTestStore(t)
	h := NewLocalHTTPHandler(s, &localTestBackend{role: "member"})
	cookie, csrf := loginTest(t, h, "admin")
	for _, path := range []string{"/api/v1/tailnets", "/api/v1/grants", "/api/v1/users", "/api/v1/nodes", "/cluster/v1/control"} {
		if w := accountRequest(h, cookie, csrf, "GET", path, nil); w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
	for _, path := range []string{"/api/v1/session", "/api/v1/local/status", "/manage/"} {
		if w := accountRequest(h, cookie, csrf, "GET", path, nil); w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w := accountRequest(h, cookie, csrf, "POST", "/api/v1/users/"+admin.ID+"/password", map[string]string{"password": "new independent administrator password"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
