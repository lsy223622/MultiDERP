package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

func setupStore(t *testing.T) (*Store, http.Handler) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, NewHTTPHandler(s)
}

func setupRequest(h http.Handler, username, password, origin, contentType, remote string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	r := httptest.NewRequest("POST", "https://control.example.com/api/v1/setup", bytes.NewReader(body))
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", contentType)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func setupRequired(t *testing.T, h http.Handler) bool {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://control.example.com/api/v1/setup", nil))
	if w.Code != 200 {
		t.Fatalf("setup status: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Required bool `json:"required"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Required
}

func TestWebSetupCreatesFirstAdminAndLogsIn(t *testing.T) {
	s, h := setupStore(t)
	if !setupRequired(t, h) {
		t.Fatal("fresh controller did not require setup")
	}
	w := setupRequest(h, "first-admin", testPassword, "https://control.example.com", "application/json", "192.0.2.1:1234")
	if w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || response.CSRF == "" {
		t.Fatal("setup did not create a protected authenticated session")
	}
	actor, err := s.Authenticate(t.Context(), cookies[0].Value)
	if err != nil || actor.Username != "first-admin" || actor.Role != "admin" || !actor.Enabled {
		t.Fatalf("setup session actor: %+v %v", actor, err)
	}
	if err := s.checkCSRF(t.Context(), cookies[0].Value, response.CSRF); err != nil {
		t.Fatal("setup session cannot make protected mutations")
	}
	if setupRequired(t, NewHTTPHandler(s)) {
		t.Fatal("initialized state was lost with a new handler")
	}
	w = setupRequest(h, "replacement", "another long password", "https://control.example.com", "application/json", "192.0.2.1:1234")
	if w.Code != 409 {
		t.Fatalf("setup could be repeated: %d", w.Code)
	}
	loginTest(t, h, "first-admin")
	if err := s.SetUserEnabled(t.Context(), actor, actor.ID, false); err != nil {
		t.Fatal(err)
	}
	if setupRequired(t, h) {
		t.Fatal("disabled administrator reopened setup")
	}
	if w := setupRequest(h, "replacement", testPassword, "https://control.example.com", "application/json", "192.0.2.1:1234"); w.Code != 409 {
		t.Fatal("disabled administrator could be replaced by anonymous setup")
	}
}

func TestWebSetupRejectsCrossSiteAndInvalidPasswords(t *testing.T) {
	s, h := setupStore(t)
	for _, tc := range []struct {
		name, origin, contentType, password string
		status                              int
	}{
		{"cross-site", "https://attacker.example", "application/json", testPassword, 403},
		{"missing-origin", "", "application/json", testPassword, 403},
		{"form-content", "https://control.example.com", "text/plain", testPassword, 403},
		{"short-password", "https://control.example.com", "application/json", "short", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := setupRequest(h, "admin", tc.password, tc.origin, tc.contentType, "192.0.2.1:1234")
			if w.Code != tc.status {
				t.Fatalf("status: %d want %d", w.Code, tc.status)
			}
		})
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM users").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected request created an account: %d %v", count, err)
	}
}

func TestWebSetupConcurrentRequestsCreateOnlyOneAdmin(t *testing.T) {
	s, _ := setupStore(t)
	h := NewHTTPHandler(s)
	start := make(chan struct{})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"first", "second"} {
		wg.Go(func() {
			<-start
			codes <- setupRequest(h, name, testPassword, "https://control.example.com", "application/json", "192.0.2.1:1234").Code
		})
	}
	close(start)
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("concurrent setup responses: %v", counts)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM users WHERE role='admin'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("administrator count: %d %v", count, err)
	}
}

func TestWebSetupRateLimitsPasswordWork(t *testing.T) {
	_, h := setupStore(t)
	for i := 0; i < 6; i++ {
		w := setupRequest(h, "admin", "short", "https://control.example.com", "application/json", "192.0.2.1:1234")
		want := 400
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d want %d", i, w.Code, want)
		}
	}
}
