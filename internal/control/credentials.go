package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"modernc.org/sqlite"
)

var ErrIdentityUnavailable = errors.New("identity source unavailable")

type OAuthCredential struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

type Tailnet struct {
	ID                  string `json:"id"`
	OwnerID             string `json:"owner_id"`
	DisplayName         string `json:"display_name"`
	APIID               string `json:"api_id"`
	Enabled             bool   `json:"enabled"`
	CredentialStatus    string `json:"credential_status"`
	CredentialError     string `json:"credential_error"`
	CredentialUpdatedAt int64  `json:"credential_updated_at"`
	LastSuccess         int64  `json:"last_identity_success"`
}

func (s *Store) EnableIdentity(keyFile string) error {
	if keyFile == "" {
		return ErrInvalid
	}
	key, err := os.ReadFile(keyFile)
	if errors.Is(err, os.ErrNotExist) {
		var count int
		if err := s.db.QueryRow("SELECT (SELECT count(*) FROM credentials)+(SELECT count(*) FROM enrollments WHERE response_encrypted IS NOT NULL)").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("controller encryption key is missing")
		}
		if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err != nil {
			return err
		}
		key = make([]byte, 32)
		rand.Read(key)
		f, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else if err != nil {
		return err
	}
	if len(key) != 32 {
		return errors.New("invalid controller encryption key")
	}
	if err := os.Chmod(keyFile, 0600); err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	s.credentialAEAD, err = cipher.NewGCM(block)
	if err != nil {
		return err
	}
	s.apiBase = "https://api.tailscale.com/api/v2"
	s.apiClient = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return nil
}

func canonicalTailnetID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 3 || len(value) > 64 || value[0] != 'T' {
		return "", ErrInvalid
	}
	for _, c := range value[1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return "", ErrInvalid
		}
	}
	return value, nil
}

func (s *Store) sealCredential(id string, c OAuthCredential) ([]byte, error) {
	if s.credentialAEAD == nil {
		return nil, ErrIdentityUnavailable
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.credentialAEAD.NonceSize())
	rand.Read(nonce)
	return s.credentialAEAD.Seal(nonce, nonce, data, []byte(id)), nil
}

func (s *Store) openCredential(id string, data []byte) (OAuthCredential, error) {
	var c OAuthCredential
	if s.credentialAEAD == nil || len(data) < s.credentialAEAD.NonceSize() {
		return c, ErrIdentityUnavailable
	}
	n := s.credentialAEAD.NonceSize()
	plain, err := s.credentialAEAD.Open(nil, data[:n], data[n:], []byte(id))
	if err != nil || json.Unmarshal(plain, &c) != nil {
		return c, ErrIdentityUnavailable
	}
	return c, nil
}

func (s *Store) fetchIdentity(ctx context.Context, apiID string, c OAuthCredential) ([]identityKey, error) {
	if s.apiClient == nil || len(c.ClientID) == 0 || len(c.ClientID) > 256 || len(c.ClientSecret) == 0 || len(c.ClientSecret) > 4096 {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case s.identityRequests <- struct{}{}:
		defer func() { <-s.identityRequests }()
	case <-ctx.Done():
		return nil, ErrIdentityUnavailable
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.apiClient)
	cfg := clientcredentials.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, TokenURL: s.apiBase + "/oauth/token", Scopes: []string{"devices:core:read"}, AuthStyle: oauth2.AuthStyleInParams}
	token, err := cfg.TokenSource(ctx).Token()
	if err != nil {
		return nil, ErrIdentityUnavailable
	}
	if !strings.EqualFold(token.TokenType, "Bearer") || token.AccessToken == "" {
		return nil, ErrIdentityUnavailable
	}
	r, err := http.NewRequestWithContext(ctx, "GET", s.apiBase+"/tailnet/"+apiID+"/devices", nil)
	if err != nil {
		return nil, ErrIdentityUnavailable
	}
	token.SetAuthHeader(r)
	response, err := s.apiClient.Do(r)
	if err != nil {
		return nil, ErrIdentityUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrIdentityUnavailable
	}
	keys, err := parseIdentity(response.Body, s.now())
	if err != nil {
		return nil, ErrIdentityUnavailable
	}
	return keys, nil
}

func conflictError(err error) error {
	var e *sqlite.Error
	if errors.As(err, &e) && e.Code()&255 == 19 {
		return ErrConflict
	}
	return err
}

func (s *Store) AddTailnet(ctx context.Context, actor Actor, name, apiID string, c OAuthCredential) (Tailnet, error) {
	var tn Tailnet
	if err := RequireOwner(actor, actor.ID); err != nil {
		return tn, err
	}
	apiID, err := canonicalTailnetID(apiID)
	if err != nil {
		return tn, err
	}
	if strings.TrimSpace(name) == "" || len(name) > 160 {
		return tn, ErrInvalid
	}
	keys, err := s.fetchIdentity(ctx, apiID, c)
	if err != nil {
		return tn, err
	}
	tn = Tailnet{ID: randomToken(), OwnerID: actor.ID, DisplayName: name, APIID: apiID, Enabled: true}
	encrypted, err := s.sealCredential(tn.ID, c)
	if err != nil {
		return Tailnet{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tailnet{}, err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return Tailnet{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO tailnets(id,owner_id,display_name,api_id) VALUES(?,?,?,?)", tn.ID, tn.OwnerID, name, apiID); err != nil {
		return Tailnet{}, conflictError(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO credentials(tailnet_id,kind,encrypted,status,updated_at) VALUES(?,'oauth',?,'valid',?)", tn.ID, encrypted, s.now().Unix()); err != nil {
		return Tailnet{}, err
	}
	if err := s.publishIdentity(ctx, tx, tn.ID, keys); err != nil {
		return Tailnet{}, err
	}
	if err := writeAudit(ctx, tx, actor.ID, actor.ID, "tailnet", tn.ID, "tailnet.create"); err != nil {
		return Tailnet{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tailnet{}, err
	}
	s.notifyPolicies()
	return s.Tailnet(ctx, actor, tn.ID)
}

const tailnetColumns = `t.id,t.owner_id,t.display_name,t.api_id,t.enabled,coalesce(c.status,'missing'),coalesce(c.error,''),coalesce(c.updated_at,0),coalesce(i.last_success,0)`
const tailnetJoins = ` FROM tailnets t LEFT JOIN credentials c ON c.tailnet_id=t.id LEFT JOIN identity_snapshots i ON i.tailnet_id=t.id`

func scanTailnet(row interface{ Scan(...any) error }) (Tailnet, error) {
	var t Tailnet
	err := row.Scan(&t.ID, &t.OwnerID, &t.DisplayName, &t.APIID, &t.Enabled, &t.CredentialStatus, &t.CredentialError, &t.CredentialUpdatedAt, &t.LastSuccess)
	return t, err
}

func (s *Store) Tailnet(ctx context.Context, actor Actor, id string) (Tailnet, error) {
	t, err := scanTailnet(s.db.QueryRowContext(ctx, "SELECT "+tailnetColumns+tailnetJoins+" WHERE t.id=?", id))
	if err != nil {
		return Tailnet{}, err
	}
	if err := RequireOwner(actor, t.OwnerID); err != nil {
		return Tailnet{}, err
	}
	return t, nil
}

func (s *Store) ListTailnets(ctx context.Context, actor Actor) ([]Tailnet, error) {
	if err := RequireOwner(actor, actor.ID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+tailnetColumns+tailnetJoins+" WHERE t.owner_id=? OR ?='admin' ORDER BY t.display_name", actor.ID, actor.Role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Tailnet{}
	for rows.Next() {
		t, err := scanTailnet(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	return items, rows.Err()
}

func tailnetOwner(ctx context.Context, tx *sql.Tx, actor Actor, id string) (string, error) {
	if err := activeActor(ctx, tx, actor); err != nil {
		return "", err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT owner_id FROM tailnets WHERE id=?", id).Scan(&owner); err != nil {
		return "", err
	}
	return owner, RequireOwner(actor, owner)
}

func (s *Store) ReplaceCredential(ctx context.Context, actor Actor, id string, c OAuthCredential) error {
	tn, err := s.Tailnet(ctx, actor, id)
	if err != nil {
		return err
	}
	var revision int64
	if err := s.db.QueryRowContext(ctx, "SELECT revision FROM credentials WHERE tailnet_id=?", id).Scan(&revision); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	keys, err := s.fetchIdentity(ctx, tn.APIID, c)
	if err != nil {
		return err
	}
	encrypted, err := s.sealCredential(id, c)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	owner, err := tailnetOwner(ctx, tx, actor, id)
	if err != nil {
		return err
	}
	var current int64
	if err := tx.QueryRowContext(ctx, "SELECT revision FROM credentials WHERE tailnet_id=?", id).Scan(&current); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if current != revision {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO credentials(tailnet_id,kind,encrypted,status,updated_at,revision) VALUES(?,'oauth',?,'valid',?,?) ON CONFLICT(tailnet_id) DO UPDATE SET encrypted=excluded.encrypted,status='valid',error='',updated_at=excluded.updated_at,revision=excluded.revision,refresh_seq=0`, id, encrypted, s.now().Unix(), revision+1); err != nil {
		return err
	}
	if err := s.publishIdentity(ctx, tx, id, keys); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, owner, "tailnet", id, "credential.replace"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicies()
	return nil
}

func (s *Store) TransferTailnet(ctx context.Context, actor Actor, id, newOwner string) error {
	if actor.Role != "admin" || !actor.Enabled {
		return ErrForbidden
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tailnetOwner(ctx, tx, actor, id); err != nil {
		return err
	}
	var enabled bool
	if err := tx.QueryRowContext(ctx, "SELECT enabled FROM users WHERE id=?", newOwner).Scan(&enabled); err != nil || !enabled {
		return ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tailnets SET owner_id=? WHERE id=?", newOwner, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tailnet_rules SET group_name='shared' WHERE tailnet_id=? AND group_name='owner' AND node_id IN (SELECT id FROM nodes WHERE owner_id<>?)", id, newOwner); err != nil {
		return err
	}
	if err := s.rebuildPolicies(ctx, tx, s.now()); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, newOwner, "tailnet", id, "tailnet.transfer"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicies()
	return nil
}

func (s *Store) DeleteCredential(ctx context.Context, actor Actor, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	owner, err := tailnetOwner(ctx, tx, actor, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE credentials SET encrypted=x'',status='missing',error='',revision=revision+1,refresh_seq=0,updated_at=? WHERE tailnet_id=?", s.now().Unix(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE identity_snapshots SET keys_json='[]',revision=revision+1 WHERE tailnet_id=?", id); err != nil {
		return err
	}
	if err := s.updateIdentityConflicts(ctx, tx); err != nil {
		return err
	}
	if err := s.rebuildPolicies(ctx, tx, s.now()); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, actor.ID, owner, "tailnet", id, "credential.delete"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notifyPolicies()
	return nil
}
