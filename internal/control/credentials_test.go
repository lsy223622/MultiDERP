package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/types/key"
)

type identityAPIStub struct {
	mu        sync.Mutex
	status    int
	body      string
	scopes    []string
	clientIDs []string
}

func TestCredentialHTTPAccessAndDeletion(t *testing.T) {
	s, _, admin := credentialTestStore(t)
	h := NewHTTPHandler(s)
	cookie, csrf := loginTest(t, h, "admin")
	w := accountRequest(h, cookie, csrf, "POST", "/api/v1/tailnets", map[string]any{"display_name": "HTTP tailnet", "api_id": "Thttp123", "credential": OAuthCredential{"id", "secret"}})
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var tn Tailnet
	if err := json.Unmarshal(w.Body.Bytes(), &tn); err != nil {
		t.Fatal(err)
	}
	memberCookie, memberCSRF := loginTest(t, h, "alice")
	for _, method := range []string{"GET", "DELETE"} {
		path := "/api/v1/tailnets/" + tn.ID
		if method == "DELETE" {
			path += "/credential"
		}
		w = accountRequest(h, memberCookie, memberCSRF, method, path, nil)
		if w.Code != 403 {
			t.Fatalf("cross-owner %s: %d", method, w.Code)
		}
	}
	w = accountRequest(h, cookie, csrf, "DELETE", "/api/v1/tailnets/"+tn.ID+"/credential", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var data []byte
	s.db.QueryRow("SELECT encrypted FROM credentials WHERE tailnet_id=?", tn.ID).Scan(&data)
	if len(data) != 0 {
		t.Fatal("deleted secret retained")
	}
	if err := s.RefreshIdentity(t.Context(), tn.ID); err == nil {
		t.Fatal("deleted credential refreshed")
	}
	if _, err := s.Tailnet(t.Context(), admin, tn.ID); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityOldRequestCannotOverwriteNewerSnapshot(t *testing.T) {
	s, _, admin := credentialTestStore(t)
	tn, err := s.AddTailnet(t.Context(), admin, "Race", "Trace123", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	oldKey, newKey := key.NewNode().Public(), key.NewNode().Public()
	var mu sync.Mutex
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"token","token_type":"Bearer"}`)
			return
		}
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		k := newKey
		if n == 1 {
			close(entered)
			<-release
			k = oldKey
		}
		fmt.Fprint(w, `{"devices":[`+identityFixture(k)+`]}`)
	}))
	defer server.Close()
	s.apiBase = server.URL
	s.apiClient = server.Client()
	oldDone := make(chan error, 1)
	go func() { oldDone <- s.RefreshIdentity(t.Context(), tn.ID) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("old request did not start")
	}
	newErr := s.RefreshIdentity(t.Context(), tn.ID)
	close(release)
	if newErr != nil {
		t.Fatal(newErr)
	}
	if err := <-oldDone; err == nil {
		t.Fatal("old request accepted")
	}
	snap, err := s.IdentitySnapshot(t.Context(), tn.ID)
	if err != nil || len(snap.Keys) != 1 || snap.Keys[0].NodePublic != newKey.String() {
		t.Fatal("old response overwrote new identity")
	}
}

func TestIdentityTimeoutAndRestartKeepAbsoluteSuccess(t *testing.T) {
	s, stub, admin := credentialTestStore(t)
	stub.body = `{"devices":[` + identityFixture(key.NewNode().Public()) + `]}`
	tn, err := s.AddTailnet(t.Context(), admin, "A", "Taaa", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.IdentitySnapshot(t.Context(), tn.ID)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.RefreshIdentity(ctx, tn.ID); err == nil {
		t.Fatal("canceled refresh succeeded")
	}
	after, _ := s.IdentitySnapshot(t.Context(), tn.ID)
	if !after.LastSuccess.Equal(before.LastSuccess) {
		t.Fatal("cancellation renewed success")
	}
	var path string
	if err := s.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err = reopened.IdentitySnapshot(t.Context(), tn.ID)
	if err != nil || !after.LastSuccess.Equal(before.LastSuccess) {
		t.Fatal("restart renewed success")
	}
}

func TestCredentialKeyPersistenceAndMissingKeyFails(t *testing.T) {
	s, _, admin := credentialTestStore(t)
	keyPath := filepath.Join(t.TempDir(), "persistent.key")
	base, client := s.apiBase, s.apiClient
	if err := s.EnableIdentity(keyPath); err != nil {
		t.Fatal(err)
	}
	s.apiBase, s.apiClient = base, client
	tn, err := s.AddTailnet(t.Context(), admin, "A", "Taaa", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	s.db.QueryRow("SELECT encrypted FROM credentials WHERE tailnet_id=?", tn.ID).Scan(&encrypted)
	otherKey := filepath.Join(t.TempDir(), "missing.key")
	if err := s.EnableIdentity(otherKey); err == nil {
		t.Fatal("silently regenerated missing key")
	}
	if _, err := s.openCredential("different-tailnet", encrypted); err == nil {
		t.Fatal("ciphertext not bound to tailnet")
	}
	var dbPath string
	if err := s.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&dbPath); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.EnableIdentity(keyPath); err != nil {
		t.Fatal(err)
	}
	reopened.apiBase, reopened.apiClient, reopened.now = base, client, s.now
	if err := reopened.RefreshIdentity(t.Context(), tn.ID); err != nil {
		t.Fatal("persisted key could not decrypt credential")
	}
}

func TestIdentityHTTPTimeoutDoesNotRenewOrDisableLogin(t *testing.T) {
	s, _, admin := credentialTestStore(t)
	tn, err := s.AddTailnet(t.Context(), admin, "A", "Taaa", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.IdentitySnapshot(t.Context(), tn.ID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"token","token_type":"Bearer"}`)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	s.apiBase = server.URL
	s.apiClient = server.Client()
	s.apiClient.Timeout = 100 * time.Millisecond
	if err := s.RefreshIdentity(t.Context(), tn.ID); err == nil {
		t.Fatal("timed-out refresh succeeded")
	}
	after, _ := s.IdentitySnapshot(t.Context(), tn.ID)
	if after.Revision != before.Revision || !after.LastSuccess.Equal(before.LastSuccess) {
		t.Fatal("timeout renewed cache")
	}
	got, err := s.Tailnet(t.Context(), admin, tn.ID)
	if err != nil || got.CredentialStatus != "unavailable" {
		t.Fatal("missing failure status")
	}
	loginTest(t, NewHTTPHandler(s), "admin")
}

func TestIdentityRefreshReplacesRotatedKey(t *testing.T) {
	s, stub, admin := credentialTestStore(t)
	oldKey, newKey := key.NewNode().Public(), key.NewNode().Public()
	stub.body = `{"devices":[` + identityFixture(oldKey) + `]}`
	tn, err := s.AddTailnet(t.Context(), admin, "A", "Taaa", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.body = `{"devices":[` + identityFixture(newKey) + `]}`
	stub.mu.Unlock()
	if err := s.RefreshIdentity(t.Context(), tn.ID); err != nil {
		t.Fatal(err)
	}
	snap, err := s.IdentitySnapshot(t.Context(), tn.ID)
	if err != nil || len(snap.Keys) != 1 || snap.Keys[0].NodePublic != newKey.String() {
		t.Fatal("rotated key not replaced")
	}
}

func credentialTestStore(t *testing.T) (*Store, *identityAPIStub, Actor) {
	t.Helper()
	s, _, admin, _ := accountTestServer(t)
	if err := s.EnableIdentity(filepath.Join(t.TempDir(), "controller.key")); err != nil {
		t.Fatal(err)
	}
	stub := &identityAPIStub{status: 200, body: `{"devices":[]}`}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			r.ParseForm()
			if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") == "" || r.Form.Get("client_secret") == "bad" {
				w.WriteHeader(401)
				fmt.Fprint(w, "sensitive upstream error")
				return
			}
			stub.mu.Lock()
			stub.scopes = append(stub.scopes, r.Form.Get("scope"))
			stub.clientIDs = append(stub.clientIDs, r.Form.Get("client_id"))
			stub.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"test-access-secret","token_type":"Bearer","expires_in":3600,"scope":"devices:core:read"}`)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/tailnet/T") || !strings.HasSuffix(r.URL.Path, "/devices") || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer test-access-secret" {
			t.Error("unexpected identity request")
			w.WriteHeader(400)
			return
		}
		stub.mu.Lock()
		status, body := stub.status, stub.body
		stub.mu.Unlock()
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	s.apiBase = server.URL
	s.apiClient = server.Client()
	s.now = func() time.Time { return time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC) }
	return s, stub, admin
}

func TestCredentialSecretOnlyBindingReplacementAndRefresh(t *testing.T) {
	s, stub, _ := credentialTestStore(t)
	h := NewHTTPHandler(s)
	cookie, csrf := loginTest(t, h, "admin")
	w := accountRequest(h, cookie, csrf, "POST", "/api/v1/tailnets", map[string]any{
		"display_name": "Secret-only Tailnet", "api_id": "Tsecret123",
		"credential": map[string]string{"client_secret": "tskey-client-firstCNTRL-secret"},
	})
	if w.Code != 200 {
		t.Fatalf("bind with secret only: %d %s", w.Code, w.Body.String())
	}
	var tn Tailnet
	if err := json.Unmarshal(w.Body.Bytes(), &tn); err != nil {
		t.Fatal(err)
	}
	w = accountRequest(h, cookie, csrf, "POST", "/api/v1/tailnets/"+tn.ID+"/credential", map[string]string{
		"client_secret": "tskey-client-replacementCNTRL-newsecret",
	})
	if w.Code != 200 {
		t.Fatalf("replace with secret only: %d %s", w.Code, w.Body.String())
	}
	if err := s.RefreshIdentity(t.Context(), tn.ID); err != nil {
		t.Fatalf("refresh secret-only credential: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if strings.Join(stub.clientIDs, ",") != "firstCNTRL,replacementCNTRL,replacementCNTRL" {
		t.Fatalf("OAuth exchanges used incorrect client IDs: %v", stub.clientIDs)
	}
}

func TestCredentialSecretOnlyRejectsMalformedSecrets(t *testing.T) {
	s, stub, admin := credentialTestStore(t)
	for _, secret := range []string{"", "secret", "tskey-auth-client-secret", "tskey-client--secret", "tskey-client-client-", "tskey-client-client"} {
		if _, err := s.AddTailnet(t.Context(), admin, "Invalid", "Tinvalid", OAuthCredential{ClientSecret: secret}); err != ErrInvalid {
			t.Fatalf("malformed secret returned %v", err)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.scopes) != 0 {
		t.Fatal("malformed secret triggered an OAuth exchange")
	}
}

func TestCredentialValidatedEncryptedAndTailnetUnique(t *testing.T) {
	s, stub, admin := credentialTestStore(t)
	secret := OAuthCredential{ClientID: "test-client", ClientSecret: "test-client-secret"}
	tn, err := s.AddTailnet(t.Context(), admin, "Test tailnet", "Tabc123", secret)
	if err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	if err := s.db.QueryRow("SELECT encrypted FROM credentials WHERE tailnet_id=?", tn.ID).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), secret.ClientSecret) || strings.Contains(string(encrypted), secret.ClientID) {
		t.Fatal("plaintext credential in database")
	}
	if _, err := s.AddTailnet(t.Context(), admin, "Alias", "Tabc123", secret); err == nil {
		t.Fatal("duplicate tailnet accepted")
	}
	for _, id := range []string{"-", "example.com", "Tabc123/other", "https://api.example"} {
		if _, err := s.AddTailnet(t.Context(), admin, "Bad", id, secret); err == nil {
			t.Fatalf("bad tailnet ID %s", id)
		}
	}
	got, err := s.Tailnet(t.Context(), admin, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), secret.ClientSecret) || strings.Contains(string(body), "encrypted") || strings.Contains(string(body), "test-access-secret") {
		t.Fatal("credential exposed in DTO")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, scope := range stub.scopes {
		if scope != "devices:core:read" {
			t.Fatal("non-read-only request")
		}
	}
}

func TestIdentityFailureKeepsSuccessAndEmptyResponseRevokes(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, stub, admin := credentialTestStore(t)
			k := key.NewNode().Public()
			stub.body = `{"devices":[` + identityFixture(k) + `]}`
			tn, err := s.AddTailnet(t.Context(), admin, "Tailnet", "Tabc123", OAuthCredential{"id", "secret"})
			if err != nil {
				t.Fatal(err)
			}
			original, err := s.IdentitySnapshot(t.Context(), tn.ID)
			if err != nil {
				t.Fatal(err)
			}
			stub.mu.Lock()
			stub.status = status
			stub.body = `{"devices":[`
			stub.mu.Unlock()
			s.now = func() time.Time { return original.LastSuccess.Add(time.Hour) }
			if err := s.RefreshIdentity(t.Context(), tn.ID); err == nil {
				t.Fatal("failed response succeeded")
			}
			got, err := s.IdentitySnapshot(t.Context(), tn.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !got.LastSuccess.Equal(original.LastSuccess) || len(got.Keys) != 1 {
				t.Fatal("failure changed identity cache")
			}
			stub.mu.Lock()
			stub.status = 200
			stub.body = `{"devices":[]}`
			stub.mu.Unlock()
			if err := s.RefreshIdentity(t.Context(), tn.ID); err != nil {
				t.Fatal(err)
			}
			got, err = s.IdentitySnapshot(t.Context(), tn.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Keys) != 0 || !got.LastSuccess.After(original.LastSuccess) {
				t.Fatal("complete empty response did not revoke")
			}
		})
	}
}

func TestCredentialReplacementFailureDoesNotUseOldCache(t *testing.T) {
	s, _, admin := credentialTestStore(t)
	tn, err := s.AddTailnet(t.Context(), admin, "Tailnet", "Tabc123", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.IdentitySnapshot(t.Context(), tn.ID)
	err = s.ReplaceCredential(t.Context(), admin, tn.ID, OAuthCredential{"id", "bad"})
	if err == nil || strings.Contains(err.Error(), "sensitive upstream error") {
		t.Fatal("replacement leaked or accepted upstream failure")
	}
	after, _ := s.IdentitySnapshot(t.Context(), tn.ID)
	if after.Revision != before.Revision || !after.LastSuccess.Equal(before.LastSuccess) {
		t.Fatal("replacement renewed old cache")
	}
	if err := s.RefreshIdentity(t.Context(), tn.ID); err != nil {
		t.Fatal("failed replacement removed working credential")
	}
}

func TestIdentityCrossTailnetKeyConflictRejectsBoth(t *testing.T) {
	s, stub, admin := credentialTestStore(t)
	k := key.NewNode().Public()
	stub.body = `{"devices":[` + identityFixture(k) + `]}`
	a, err := s.AddTailnet(t.Context(), admin, "A", "Taaa", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AddTailnet(t.Context(), admin, "B", "Tbbb", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		got, err := s.IdentitySnapshot(t.Context(), id)
		if err != nil || len(got.Keys) != 0 {
			t.Fatal("conflicting key allowed")
		}
	}
	var events int
	if err := s.db.QueryRow("SELECT count(*) FROM events WHERE kind='identity_conflict' AND resolved_at=0").Scan(&events); err != nil || events != 2 {
		t.Fatalf("conflict events %d %v", events, err)
	}
}

func TestTailnetTransferIsAdminOnlyAndAudited(t *testing.T) {
	s, _, admin := credentialTestStore(t)
	member, err := s.CreateMember(t.Context(), admin, "owner", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	tn, err := s.AddTailnet(t.Context(), admin, "A", "Taaa", OAuthCredential{"id", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TransferTailnet(t.Context(), member, tn.ID, member.ID); err == nil {
		t.Fatal("member transferred resource")
	}
	if err := s.TransferTailnet(t.Context(), admin, tn.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tailnet(t.Context(), member, tn.ID); err != nil {
		t.Fatal(err)
	}
	var actor, owner string
	if err := s.db.QueryRow("SELECT actor_id,owner_id FROM audit WHERE action='tailnet.transfer'").Scan(&actor, &owner); err != nil || actor != admin.ID || owner != member.ID {
		t.Fatal("transfer audit lost actor")
	}
}
