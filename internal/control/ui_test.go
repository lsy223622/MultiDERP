package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagementShellLoadsBeforeLoginWithoutOpeningAPIs(t *testing.T) {
	s, _, _ := nodeTestStore(t)
	h := NewHTTPHandler(s)
	for path, contentType := range map[string]string{
		"/manage/":         "text/html",
		"/manage/app.js":   "text/javascript",
		"/manage/theme.js": "text/javascript",
		"/manage/app.css":  "text/css",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "https://controller.example.com"+path, nil))
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), contentType) {
			t.Fatal("login assets unavailable", path, w.Code, w.Header())
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatal("missing management CSP", path, w.Header())
		}
		if strings.Contains(w.Body.String(), "__Host-uniderp_session=") {
			t.Fatal("session in static asset")
		}
	}
	for _, path := range []string{"/api/v1/session", "/api/v1/users", "/api/v1/nodes", "/api/v1/tailnets", "/api/v1/grants"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "https://controller.example.com"+path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatal("anonymous management assets opened API", path, w.Code)
		}
	}
}

func TestManagementRolesUseSharedShellAndLocalAuthority(t *testing.T) {
	s, _, _ := nodeTestStore(t)
	for _, role := range []string{"setup", "member", "controller"} {
		h := NewLocalHTTPHandler(s, &localTestBackend{role: role})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "https://relay.example.com/manage/", nil))
		if w.Code != 200 {
			t.Fatal(role, w.Code)
		}
		cookie, csrf := loginTest(t, h, "admin")
		if w := accountRequest(h, cookie, csrf, "GET", "/api/v1/local/status", nil); w.Code != 200 {
			t.Fatal(role, w.Code)
		}
		if w := accountRequest(h, cookie, csrf, "GET", "/api/v1/tailnets", nil); (role == "controller" && w.Code != 200) || (role != "controller" && w.Code != 404) {
			t.Fatal(role, w.Code)
		}
		ordinary, token := loginTest(t, h, "alice")
		if w := accountRequest(h, ordinary, token, "POST", "/api/v1/local/apply", map[string]bool{}); w.Code != 403 {
			t.Fatal(role, w.Code)
		}
	}
}

func TestManagementFontAsset(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/manage/fonts/InterVariable.woff2", nil)
		if !serveManagement(w, r) || w.Code != http.StatusOK {
			t.Fatalf("%s font: handled status %d", method, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != "font/woff2" {
			t.Fatalf("font content type: %q", got)
		}
		if method == http.MethodGet && (w.Body.Len() < 4 || string(w.Body.Bytes()[:4]) != "wOF2") {
			t.Fatal("font response is not WOFF2")
		}
		if method == http.MethodHead && w.Body.Len() != 0 {
			t.Fatal("HEAD font response has a body")
		}
	}
}
