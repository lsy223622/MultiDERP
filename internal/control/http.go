package control

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

const sessionCookie = "__Host-uniderp_session"

type httpHandler struct {
	store        *Store
	mux          *http.ServeMux
	mu           sync.Mutex
	attempts     map[string]loginAttempt
	clusterMux   *http.ServeMux
	nodeAttempts map[string]loginAttempt
	local        LocalBackend
}
type loginAttempt struct {
	count int
	until time.Time
}

func NewHTTPHandler(s *Store) http.Handler {
	return newHTTPHandler(s)
}

func newHTTPHandler(s *Store) *httpHandler {
	h := &httpHandler{store: s, mux: http.NewServeMux(), clusterMux: http.NewServeMux(), attempts: make(map[string]loginAttempt), nodeAttempts: make(map[string]loginAttempt)}
	h.mountTailnets()
	h.mountNodes()
	h.mountNodeControl()
	h.mountGrants()
	h.mountObservability()
	h.mountResources()
	h.mux.HandleFunc("POST /api/v1/login", h.login)
	h.mux.HandleFunc("POST /api/v1/setup", h.login)
	h.mux.HandleFunc("GET /api/v1/setup", func(w http.ResponseWriter, r *http.Request) {
		exists, err := s.hasAdmin(r.Context())
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"required": !exists})
	})
	h.mux.HandleFunc("POST /api/v1/logout", func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie(sessionCookie)
		if _, err := s.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=?", tokenHash(cookie.Value)); err != nil {
			httpError(w, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("GET /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie(sessionCookie)
		actor, err := s.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, struct {
			Actor      Actor  `json:"actor"`
			CSRF       string `json:"csrf_token"`
			ServerRole string `json:"server_role"`
		}{actor, hex.EncodeToString(tokenHash("uniderp-csrf:" + cookie.Value)), h.serverRole()})
	})
	h.mux.HandleFunc("POST /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		actor := h.actor(r)
		if actor.Role != "admin" {
			httpError(w, ErrForbidden)
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		user, err := s.CreateMember(r.Context(), actor, body.Username, body.Password)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, user)
	})
	h.mux.HandleFunc("GET /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		actor := h.actor(r)
		if actor.Role != "admin" {
			httpError(w, ErrForbidden)
			return
		}
		rows, err := s.db.QueryContext(r.Context(), "SELECT id,username,role,enabled FROM users ORDER BY username")
		if err != nil {
			httpError(w, err)
			return
		}
		defer rows.Close()
		users := []Actor{}
		for rows.Next() {
			var a Actor
			if err := rows.Scan(&a.ID, &a.Username, &a.Role, &a.Enabled); err != nil {
				httpError(w, err)
				return
			}
			users = append(users, a)
		}
		if err := rows.Err(); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, users)
	})
	h.mux.HandleFunc("POST /api/v1/users/{id}/password", func(w http.ResponseWriter, r *http.Request) {
		actor := h.actor(r)
		if err := RequireOwner(actor, r.PathValue("id")); err != nil {
			httpError(w, err)
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if err := s.ChangePassword(r.Context(), actor, r.PathValue("id"), body.Password); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("POST /api/v1/users/{id}/enabled", func(w http.ResponseWriter, r *http.Request) {
		actor := h.actor(r)
		if actor.Role != "admin" {
			httpError(w, ErrForbidden)
			return
		}
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		if body.Enabled == nil {
			httpError(w, ErrInvalid)
			return
		}
		if err := s.SetUserEnabled(r.Context(), actor, r.PathValue("id"), *body.Enabled); err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	return h
}

func (h *httpHandler) actor(r *http.Request) Actor {
	cookie, _ := r.Cookie(sessionCookie)
	a, _ := h.store.Authenticate(r.Context(), cookie.Value)
	return a
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if serveManagement(w, r) {
		return
	}
	if h.serverRole() != "controller" && strings.HasPrefix(r.URL.Path, "/cluster/v1/") {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/cluster/v1/control" || r.URL.Path == "/cluster/v1/heartbeat" || r.URL.Path == "/cluster/v1/ack" || r.URL.Path == "/cluster/v1/node/leave" {
		h.clusterMux.ServeHTTP(w, r)
		return
	}
	if r.URL.Path == "/cluster/v1/enroll/challenge" || r.URL.Path == "/cluster/v1/enroll" || r.URL.Path == "/cluster/v1/session/challenge" || r.URL.Path == "/cluster/v1/session" {
		if r.Method == "POST" && !h.allowNodeAttempt(r) {
			http.Error(w, "registration rate limit", http.StatusTooManyRequests)
			return
		}
		h.clusterMux.ServeHTTP(w, r)
		return
	}
	if r.URL.Path != "/api/v1/login" && r.URL.Path != "/api/v1/setup" {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			httpError(w, ErrUnauthorized)
			return
		}
		actor, err := h.store.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			httpError(w, err)
			return
		}
		if h.serverRole() != "controller" && r.URL.Path != "/api/v1/session" && r.URL.Path != "/api/v1/logout" && !strings.HasPrefix(r.URL.Path, "/api/v1/local/") && r.URL.Path != "/api/v1/users/"+actor.ID+"/password" {
			http.NotFound(w, r)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if err := h.store.checkCSRF(r.Context(), cookie.Value, r.Header.Get("X-CSRF-Token")); err != nil {
				httpError(w, err)
				return
			}
		}
	}
	h.mux.ServeHTTP(w, r)
}

func (h *httpHandler) login(w http.ResponseWriter, r *http.Request) {
	initial := r.URL.Path == "/api/v1/setup"
	if initial {
		exists, err := h.store.hasAdmin(r.Context())
		if err != nil {
			httpError(w, err)
			return
		}
		if exists {
			httpError(w, ErrConflict)
			return
		}
		origin := r.Header.Get("Origin")
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if (origin != "https://"+r.Host && origin != "http://"+r.Host) || err != nil || mediaType != "application/json" {
			httpError(w, ErrForbidden)
			return
		}
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	now := time.Now()
	h.mu.Lock()
	for k, a := range h.attempts {
		if !now.Before(a.until) {
			delete(h.attempts, k)
		}
	}
	a := h.attempts[ip]
	if a.count >= 5 || len(h.attempts) >= 1024 {
		h.mu.Unlock()
		http.Error(w, "login rate limit", http.StatusTooManyRequests)
		return
	}
	a.count++
	a.until = now.Add(time.Minute)
	h.attempts[ip] = a
	h.mu.Unlock()
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	if initial {
		if _, err := h.store.InitializeAdmin(r.Context(), body.Username, body.Password); err != nil {
			httpError(w, err)
			return
		}
	}
	token, csrf, err := h.store.login(r.Context(), body.Username, body.Password)
	if err != nil {
		httpError(w, err)
		return
	}
	h.mu.Lock()
	delete(h.attempts, ip)
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: 43200, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, map[string]string{"csrf_token": csrf})
}

func decodeRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(dst) != nil || dec.Decode(new(any)) != io.EOF {
		httpError(w, ErrInvalid)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "request failed"
	switch {
	case errors.Is(err, ErrUnauthorized):
		status = 401
		message = ErrUnauthorized.Error()
	case errors.Is(err, ErrForbidden):
		status = 403
		message = ErrForbidden.Error()
	case errors.Is(err, ErrInvalid):
		status = 400
		message = ErrInvalid.Error()
	case errors.Is(err, ErrConflict):
		status = 409
		message = ErrConflict.Error()
	case errors.Is(err, ErrRateLimited):
		status = 429
		message = ErrRateLimited.Error()
	case errors.Is(err, ErrIdentityUnavailable):
		status = 503
		message = ErrIdentityUnavailable.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]string{"error": message}
	if errors.Is(err, cluster.ErrIdentityConflict) {
		body["code"] = "identity_conflict"
	}
	json.NewEncoder(w).Encode(body)
}
