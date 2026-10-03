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
	mu         sync.Mutex
	privateKey ed25519.PrivateKey
	instanceID string
	statePath  string
	state      nodeRegistration
	responder  *DomainResponder
	httpClient *http.Client
}

func NewEnrollmentClient(controllerURL, stateDir string) (*EnrollmentClient, error) {
	u, err := url.Parse(controllerURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || stateDir == "" {
		return nil, errors.New("controller must be an HTTPS origin")
	}
	u.Path = ""
	controllerURL = u.String()
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
	c := &EnrollmentClient{privateKey: key, instanceID: hex.EncodeToString(id[:]), statePath: filepath.Join(stateDir, "registration.json"), state: nodeRegistration{ControllerURL: controllerURL}, responder: NewDomainResponder(key), httpClient: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	b, err := os.ReadFile(c.statePath)
	if err == nil {
		if len(b) > 64<<10 || decodeNodeJSON(b, &c.state) != nil || c.state.ControllerURL != controllerURL {
			return nil, errors.New("invalid or differently bound node registration")
		}
		if c.state.Pending != nil {
			c.instanceID = c.state.Pending.Challenge.InstanceID
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return c, nil
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

func (c *EnrollmentClient) post(ctx context.Context, path string, body, dst any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return errors.New("invalid node request")
	}
	r, err := http.NewRequestWithContext(ctx, "POST", c.state.ControllerURL+path, bytes.NewReader(b))
	if err != nil {
		return errors.New("invalid controller request")
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(r)
	if err != nil {
		return errors.New("controller HTTPS request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
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
	if session.ClusterID != ch.ClusterID || session.NodeID != ch.NodeID || session.InstanceID != ch.InstanceID || !validNodeToken(session.Token) || !time.Now().Before(session.ExpiresAt) || session.ExpiresAt.After(time.Now().Add(61*time.Minute)) {
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
		}{code, c.privateKey.Public().(ed25519.PublicKey), c.instanceID}, &ch); err != nil {
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
	if err := c.post(ctx, "/cluster/v1/enroll", c.state.Pending, &session); err != nil {
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
	}{c.state.Session.NodeID, c.instanceID}, &ch); err != nil {
		return NodeSession{}, err
	}
	if !c.validChallenge(ch, "session") || ch.NodeID != c.state.Session.NodeID || ch.ClusterID != c.state.Session.ClusterID || ch.Domain != c.state.Domain {
		return NodeSession{}, errors.New("invalid session challenge")
	}
	var session NodeSession
	if err := c.post(ctx, "/cluster/v1/session", struct {
		Challenge NodeChallenge `json:"challenge"`
		Signature []byte        `json:"signature"`
	}{ch, ed25519.Sign(c.privateKey, ch.SigningBytes())}, &session); err != nil {
		return NodeSession{}, err
	}
	if err := c.acceptSession(session, ch); err != nil {
		return NodeSession{}, err
	}
	return session, nil
}
