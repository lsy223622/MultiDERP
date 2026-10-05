package cluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func LoadNodeIdentity(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PrivateKeySize || !bytes.Equal(b, ed25519.NewKeyFromSeed(b[:ed25519.SeedSize])) {
		return nil, errors.New("invalid persistent node identity")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	return ed25519.PrivateKey(b), nil
}

type nodeRegistration struct {
	ControllerURL string             `json:"controller_url"`
	Domain        string             `json:"domain"`
	Session       NodeSession        `json:"session"`
	Pending       *EnrollmentRequest `json:"pending,omitempty"`
}

type EnrollmentClient struct {
	mu            sync.Mutex
	privateKey    ed25519.PrivateKey
	instanceID    string
	statePath     string
	state         nodeRegistration
	responder     *DomainResponder
	httpClient    *http.Client
	controlStatus ControlStatus
}

func (c *EnrollmentClient) PolicyBinding() (clusterID, nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Session.ClusterID, c.state.Session.NodeID
}

func NewEnrollmentClient(controllerURL, stateDir string) (*EnrollmentClient, error) {
	controllerURL, err := controllerOrigin(controllerURL)
	if err != nil || stateDir == "" {
		return nil, errors.New("controller must be an HTTPS origin")
	}
	keyPath := filepath.Join(stateDir, "node.key")
	if _, err := os.Stat(filepath.Join(stateDir, "registration.json")); err == nil {
		if _, err := os.Stat(keyPath); err != nil {
			return nil, errors.New("persistent node identity is missing")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := LoadNodeIdentity(keyPath)
	if err != nil {
		return nil, err
	}
	var id [32]byte
	rand.Read(id[:])
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	c := &EnrollmentClient{privateKey: key, instanceID: hex.EncodeToString(id[:]), statePath: filepath.Join(stateDir, "registration.json"), state: nodeRegistration{ControllerURL: controllerURL}, responder: NewDomainResponder(key), httpClient: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	b, err := os.ReadFile(c.statePath)
	if err == nil {
		if len(b) > 64<<10 || decodeNodeJSON(b, &c.state) != nil || (c.state.ControllerURL != controllerURL && (c.state.Session.NodeID != "" || c.state.Pending != nil)) {
			return nil, errors.New("invalid or differently bound node registration")
		}
		c.state.ControllerURL = controllerURL
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return c, nil
}

func controllerOrigin(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("controller must be an HTTPS origin")
	}
	u.Path = ""
	return u.String(), nil
}

func (c *EnrollmentClient) SetControllerURL(value string) error {
	value, err := controllerOrigin(value)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Session.NodeID != "" || c.state.Pending != nil {
		return errors.New("node is already bound to a controller")
	}
	previous := c.state
	c.state.ControllerURL = value
	if err := c.saveRegistration(); err != nil {
		c.state = previous
		return err
	}
	return nil
}

func (c *EnrollmentClient) ClearRegistration() (NodeSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.state
	c.state = nodeRegistration{ControllerURL: previous.ControllerURL}
	if err := c.saveRegistration(); err != nil {
		c.state = previous
		return NodeSession{}, err
	}
	c.controlStatus = ControlStatus{}
	return previous.Session, nil
}

func (c *EnrollmentClient) Release(ctx context.Context, previous NodeSession) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result struct {
		OK bool `json:"ok"`
	}
	return c.post(ctx, "/cluster/v1/node/leave", struct{}{}, &result, previous.Token)
}

func decodeNodeJSON(b []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid node response")
	}
	return nil
}

func (c *EnrollmentClient) saveRegistration() error {
	b, err := json.Marshal(c.state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.statePath), ".registration-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(b)
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
	return os.Rename(f.Name(), c.statePath)
}

func (c *EnrollmentClient) post(ctx context.Context, path string, body, dst any, token string) error {
	b, err := json.Marshal(body)
	if err != nil {
		return errors.New("invalid node request")
	}
	r, err := http.NewRequestWithContext(ctx, "POST", c.state.ControllerURL+path, bytes.NewReader(b))
	if err != nil {
		return errors.New("invalid controller request")
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.httpClient.Do(r)
	if err != nil {
		return errors.New("controller HTTPS request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if token != "" && response.StatusCode == http.StatusUnauthorized {
			return errNodeSessionRejected
		}
		if path == "/cluster/v1/session" && response.StatusCode == http.StatusConflict {
			b, err := io.ReadAll(io.LimitReader(response.Body, 4097))
			var failure struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err == nil && len(b) <= 4096 && decodeNodeJSON(b, &failure) == nil && failure.Code == "identity_conflict" {
				return ErrIdentityConflict
			}
		}
		return errors.New("controller rejected node request")
	}
	b, err = io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(b) > 64<<10 {
		return errors.New("invalid controller response")
	}
	return decodeNodeJSON(b, dst)
}

func validNodeToken(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(value) == 64 && len(b) == 32
}

func (c *EnrollmentClient) validChallenge(ch NodeChallenge, purpose string) bool {
	return ch.Purpose == purpose && validNodeToken(ch.ClusterID) && validNodeToken(ch.NodeID) && validNodeToken(ch.Nonce) && ch.Domain != "" && ch.InstanceID == c.instanceID && bytes.Equal(ch.PublicKey, c.privateKey.Public().(ed25519.PublicKey)) && time.Now().Before(ch.ExpiresAt) && ch.ExpiresAt.Before(time.Now().Add(3*time.Minute))
}

func (c *EnrollmentClient) acceptSession(session NodeSession, ch NodeChallenge) error {
	if session.ClusterID != ch.ClusterID || session.NodeID != ch.NodeID || session.InstanceID != ch.InstanceID || !validNodeToken(session.Token) || !time.Now().Before(session.ExpiresAt) || session.ExpiresAt.After(time.Now().Add(61*time.Minute)) || session.LeaseUntil.IsZero() || session.LeaseUntil.After(session.ExpiresAt) || session.LeaseUntil.After(time.Now().Add(2*time.Minute)) {
		return errors.New("controller returned a differently bound session")
	}
	previous := c.state
	c.state.Session = session
	c.state.Domain = ch.Domain
	c.state.Pending = nil
	if err := c.saveRegistration(); err != nil {
		c.state = previous
		return err
	}
	return nil
}

func (c *EnrollmentClient) DomainHandler() http.Handler { return c.responder }

func (c *EnrollmentClient) PendingEnrollmentCode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Pending != nil {
		return c.state.Pending.Code
	}
	return ""
}

func (c *EnrollmentClient) Enroll(ctx context.Context, code string) (NodeSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !validNodeToken(code) {
		return NodeSession{}, errors.New("invalid enrollment code")
	}
	if c.state.Session.NodeID != "" {
		return NodeSession{}, errors.New("node is already registered")
	}
	if c.state.Pending == nil || c.state.Pending.Code != code {
		var ch NodeChallenge
		if err := c.post(ctx, "/cluster/v1/enroll/challenge", struct {
			Code       string `json:"code"`
			PublicKey  []byte `json:"public_key"`
			InstanceID string `json:"instance_id"`
		}{code, c.privateKey.Public().(ed25519.PublicKey), c.instanceID}, &ch, ""); err != nil {
			return NodeSession{}, err
		}
		if !c.validChallenge(ch, "enroll") {
			return NodeSession{}, errors.New("invalid enrollment challenge")
		}
		c.state.Pending = &EnrollmentRequest{Code: code, Challenge: ch, Signature: ed25519.Sign(c.privateKey, ch.SigningBytes())}
		if err := c.saveRegistration(); err != nil {
			return NodeSession{}, err
		}
	}
	c.responder.SetChallenge(c.state.Pending.Challenge)
	var session NodeSession
	if err := c.post(ctx, "/cluster/v1/enroll", c.state.Pending, &session, ""); err != nil {
		return NodeSession{}, err
	}
	if err := c.acceptSession(session, c.state.Pending.Challenge); err != nil {
		return NodeSession{}, err
	}
	return session, nil
}

func (c *EnrollmentClient) RenewSession(ctx context.Context) (NodeSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Session.NodeID == "" {
		return NodeSession{}, errors.New("node is not registered")
	}
	var ch NodeChallenge
	if err := c.post(ctx, "/cluster/v1/session/challenge", struct {
		NodeID     string `json:"node_id"`
		InstanceID string `json:"instance_id"`
	}{c.state.Session.NodeID, c.instanceID}, &ch, ""); err != nil {
		return NodeSession{}, err
	}
	valid := (c.validChallenge(ch, "session") && ch.Domain == c.state.Domain) || c.validChallenge(ch, "domain_change")
	if !valid || ch.NodeID != c.state.Session.NodeID || ch.ClusterID != c.state.Session.ClusterID {
		return NodeSession{}, errors.New("invalid session challenge")
	}
	if ch.Purpose == "domain_change" {
		c.responder.SetChallenge(ch)
	}
	var session NodeSession
	if err := c.post(ctx, "/cluster/v1/session", struct {
		Challenge NodeChallenge `json:"challenge"`
		Signature []byte        `json:"signature"`
	}{ch, ed25519.Sign(c.privateKey, ch.SigningBytes())}, &session, ""); err != nil {
		return NodeSession{}, err
	}
	if err := c.acceptSession(session, ch); err != nil {
		return NodeSession{}, err
	}
	return session, nil
}
