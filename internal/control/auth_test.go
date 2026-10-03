package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const testPassword = "a long test password"

func accountTestServer(t *testing.T) (*Store, http.Handler, Actor, Actor) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	admin, err := s.InitializeAdmin(t.Context(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.CreateMember(t.Context(), admin, "alice", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return s, NewHTTPHandler(s), admin, member
}

func TestLocalAdminInitializationAndRecovery(t *testing.T) {
	s, h, admin, member := accountTestServer(t)
	if _, err := s.InitializeAdmin(t.Context(), "second", testPassword); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate initialization: %v", err)
	}
	if err := s.RecoverAdmin(t.Context(), member.ID, testPassword); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member recovery became admin: %v", err)
	}
	cookie, csrf := loginTest(t, h, "admin")
	if err := s.SetUserEnabled(t.Context(), admin, admin.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverAdmin(t.Context(), admin.ID, testPassword); err != nil {
		t.Fatal(err)
	}
	if w := accountRequest(h, cookie, csrf, "GET", "/api/v1/session", nil); w.Code != 401 {
		t.Fatal("recovery retained old session")
	}
	loginTest(t, h, "admin")
	if w := accountRequest(h, cookie, csrf, "POST", "/api/v1/admin/init", nil); w.Code != 401 {
		t.Fatal("anonymous initialization exposed")
	}
}

func TestAccountResponsesEscapeStringsAndDoNotExposeSecrets(t *testing.T) {
	_, h, _, _ := accountTestServer(t)
	cookie, csrf := loginTest(t, h, "admin")
	w := accountRequest(h, cookie, csrf, "POST", "/api/v1/users", map[string]string{"username": "<script>alert(1)</script>", "password": testPassword})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = accountRequest(h, cookie, csrf, "GET", "/api/v1/users", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), testPassword) || strings.Contains(w.Body.String(), cookie.Value) || strings.Contains(w.Body.String(), "password_hash") {
		t.Fatal("unsafe account response")
	}
}

func TestLoginRateLimitAndBodyLimit(t *testing.T) {
	_, h, _, _ := accountTestServer(t)
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest("POST", "/api/v1/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 401
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/v1/login", strings.NewReader(`{"username":"`+strings.Repeat("a", 1<<20)+`","password":"x"}`))
	r.RemoteAddr = "192.0.2.2:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("oversized request: %d", w.Code)
	}
}

func TestDisabledAdminCannotWriteWithStaleActor(t *testing.T) {
	s, _, admin, _ := accountTestServer(t)
	if err := s.SetUserEnabled(t.Context(), admin, admin.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMember(t.Context(), admin, "newmember", testPassword); !errors.Is(err, ErrForbidden) {
		t.Fatalf("disabled actor wrote: %v", err)
	}
}

func loginTest(t *testing.T, h http.Handler, name string) (*http.Cookie, string) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"username": name, "password": testPassword})
	r := httptest.NewRequest("POST", "https://control.example.com/api/v1/login", bytes.NewReader(data))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("login %s: %d %s", name, w.Code, w.Body.String())
	}
	var body struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || body.CSRF == "" {
		t.Fatal("unsafe session")
	}
	return cookies[0], body.CSRF
}

func accountRequest(h http.Handler, cookie *http.Cookie, csrf, method, path string, body any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "https://control.example.com"+path, bytes.NewReader(data))
	r.AddCookie(cookie)
	r.Header.Set("X-CSRF-Token", csrf)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestMemberCannotAccessOtherOwner(t *testing.T) {
	s, h, admin, _ := accountTestServer(t)
	cookie, csrf := loginTest(t, h, "alice")
	w := accountRequest(h, cookie, csrf, "POST", "/api/v1/users/"+admin.ID+"/password", map[string]string{"password": "a new long password"})
	if w.Code != 403 {
		t.Fatalf("other owner password: %d", w.Code)
	}
	w = accountRequest(h, cookie, csrf, "POST", "/api/v1/users", map[string]string{"username": "injected", "password": testPassword})
	if w.Code != 403 {
		t.Fatalf("member created account: %d", w.Code)
	}
	var count int
	s.db.QueryRow("SELECT count(*) FROM users").Scan(&count)
	if count != 2 {
		t.Fatal("unauthorized write took effect")
	}
}

func TestAdminCanActWithOwnActorID(t *testing.T) {
	s, h, admin, member := accountTestServer(t)
	cookie, csrf := loginTest(t, h, "admin")
	w := accountRequest(h, cookie, csrf, "POST", "/api/v1/users/"+member.ID+"/password", map[string]string{"password": "a new long password"})
	if w.Code != 200 {
		t.Fatalf("admin reset: %d %s", w.Code, w.Body.String())
	}
	var actorID, ownerID string
	if err := s.db.QueryRow("SELECT actor_id,owner_id FROM audit WHERE action='user.password' ORDER BY id DESC LIMIT 1").Scan(&actorID, &ownerID); err != nil {
		t.Fatal(err)
	}
	if actorID != admin.ID || ownerID != member.ID {
		t.Fatal("audit lost actual actor")
	}
}

func TestDisabledUserSessionIsInvalid(t *testing.T) {
	_, h, _, member := accountTestServer(t)
	memberCookie, memberCSRF := loginTest(t, h, "alice")
	adminCookie, adminCSRF := loginTest(t, h, "admin")
	w := accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/users/"+member.ID+"/enabled", map[string]bool{"enabled": false})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = accountRequest(h, memberCookie, memberCSRF, "GET", "/api/v1/session", nil)
	if w.Code != 401 {
		t.Fatalf("disabled session remained valid: %d", w.Code)
	}
}

func TestPasswordResetRevokesSessions(t *testing.T) {
	_, h, _, member := accountTestServer(t)
	oldCookie, oldCSRF := loginTest(t, h, "alice")
	adminCookie, adminCSRF := loginTest(t, h, "admin")
	w := accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/users/"+member.ID+"/password", map[string]string{"password": "a new long password"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = accountRequest(h, oldCookie, oldCSRF, "GET", "/api/v1/session", nil)
	if w.Code != 401 {
		t.Fatalf("reset session remained valid: %d", w.Code)
	}
}

func TestCSRFRequiredAndLogoutRevokesSession(t *testing.T) {
	_, h, _, member := accountTestServer(t)
	cookie, csrf := loginTest(t, h, "alice")
	w := accountRequest(h, cookie, "", "POST", "/api/v1/users/"+member.ID+"/password", map[string]string{"password": "a new long password"})
	if w.Code != 403 {
		t.Fatalf("missing CSRF accepted: %d", w.Code)
	}
	w = accountRequest(h, cookie, csrf, "POST", "/api/v1/logout", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = accountRequest(h, cookie, csrf, "GET", "/api/v1/session", nil)
	if w.Code != 401 {
		t.Fatal("logout retained session")
	}
}
