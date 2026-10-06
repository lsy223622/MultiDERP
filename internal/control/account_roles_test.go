package control

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestProviderAndMemberNodeAuthority(t *testing.T) {
	s, admin, _ := nodeTestStore(t)
	h := NewLocalHTTPHandler(s, &localTestBackend{role: "controller"})
	cookie, csrf := loginTest(t, h, "admin")
	for _, role := range []string{"provider", "member"} {
		w := accountRequest(h, cookie, csrf, "POST", "/api/v1/users", map[string]string{"username": role, "password": testPassword, "role": role})
		if w.Code != 200 {
			t.Fatalf("create %s: %d %s", role, w.Code, w.Body.String())
		}
		var user Actor
		if err := json.Unmarshal(w.Body.Bytes(), &user); err != nil || user.Role != role {
			t.Fatal(user, err)
		}
		userCookie, userCSRF := loginTest(t, h, role)
		w = accountRequest(h, userCookie, userCSRF, "POST", "/api/v1/nodes", map[string]any{"display_name": role, "domain": role + ".example.com"})
		want := 403
		if role == "provider" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("%s node creation: %d %s", role, w.Code, w.Body.String())
		}
		if w := accountRequest(h, userCookie, userCSRF, "GET", "/api/v1/nodes", nil); w.Code != want {
			t.Fatal("node management access", role, w.Code)
		}
		if w := accountRequest(h, userCookie, userCSRF, "GET", "/api/v1/local/status", nil); w.Code != 403 {
			t.Fatal("ordinary account read local settings", role, w.Code)
		}
		if w := accountRequest(h, userCookie, userCSRF, "GET", "/api/v1/tailnets", nil); w.Code != 200 {
			t.Fatal("tailnet access", role, w.Code)
		}
		if w := accountRequest(h, userCookie, userCSRF, "POST", "/api/v1/users/"+admin.ID+"/role", map[string]string{"role": "provider"}); w.Code != 403 {
			t.Fatal("ordinary account assigned role", role, w.Code)
		}
	}
}

func TestRoleChangeRevokesSessionsAndKeepsNodeOwners(t *testing.T) {
	s, _, _ := nodeTestStore(t)
	h := NewHTTPHandler(s)
	adminCookie, adminCSRF := loginTest(t, h, "admin")
	w := accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/users", map[string]string{"username": "provider", "password": testPassword, "role": "provider"})
	var provider Actor
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &provider) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	cookie, csrf := loginTest(t, h, "provider")
	w = accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/users/"+provider.ID+"/role", map[string]string{"role": "member"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := accountRequest(h, cookie, csrf, "GET", "/api/v1/session", nil); w.Code != 401 {
		t.Fatal("role change retained session", w.Code)
	}
	if w := accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/users/"+provider.ID+"/role", map[string]string{"role": "provider"}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	cookie, csrf = loginTest(t, h, "provider")
	if w := accountRequest(h, cookie, csrf, "POST", "/api/v1/nodes", map[string]string{"display_name": "Relay", "domain": "role.example.com"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/users/"+provider.ID+"/role", map[string]string{"role": "member"}); w.Code != 409 {
		t.Fatal("downgrade stranded node", w.Code)
	}
	if w := accountRequest(h, adminCookie, adminCSRF, "POST", "/api/v1/users/"+provider.ID+"/role", map[string]string{"role": "admin"}); w.Code != 400 {
		t.Fatal("role assignment elevated admin", w.Code)
	}
	var audit int
	if err := s.db.QueryRow("SELECT count(*) FROM audit WHERE resource_id=? AND action='user.role'", provider.ID).Scan(&audit); err != nil || audit != 2 {
		t.Fatal(audit, err)
	}
}

func TestVersionSixMigrationPreservesResourcesAndPromotesNodeOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := s.InitializeAdmin(t.Context(), "admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.CreateMember(t.Context(), admin, "owner", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.CreateMember(t.Context(), admin, "member", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.login(t.Context(), "owner", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO nodes(id,owner_id,domain,display_name,region_id) VALUES('node',?,'legacy.example.com','Legacy',900);
INSERT INTO tailnets(id,owner_id,display_name) VALUES('tailnet',?,'Tailnet');`, owner.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`PRAGMA foreign_keys=OFF;
CREATE TABLE legacy_users(id TEXT PRIMARY KEY,username TEXT NOT NULL UNIQUE COLLATE NOCASE,role TEXT NOT NULL CHECK(role IN ('admin','member')),enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),password_hash BLOB NOT NULL,session_version INTEGER NOT NULL DEFAULT 1);
INSERT INTO legacy_users SELECT * FROM users;
DROP TABLE users; ALTER TABLE legacy_users RENAME TO users;
PRAGMA user_version=6;PRAGMA foreign_keys=ON;`)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.Authenticate(t.Context(), token)
	if err != nil || a.ID != owner.ID || a.Role != "provider" {
		t.Fatal("owner session", a, err)
	}
	var role string
	if err := s.db.QueryRow("SELECT role FROM users WHERE id=?", member.ID).Scan(&role); err != nil || role != "member" {
		t.Fatal(role, err)
	}
	var ownerID string
	if err := s.db.QueryRow("SELECT owner_id FROM tailnets WHERE id='tailnet'").Scan(&ownerID); err != nil || ownerID != owner.ID {
		t.Fatal("tailnet owner", ownerID, err)
	}
	if _, _, err := s.login(t.Context(), "owner", testPassword); err != nil {
		t.Fatal("password lost", err)
	}
	if _, err := s.db.Exec("INSERT INTO tailnets(id,owner_id,display_name) VALUES('invalid','missing','Invalid')"); err == nil {
		t.Fatal("foreign keys disabled after migration")
	}
}
