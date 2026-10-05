package cluster

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var errNodeSessionRejected = errors.New("node session rejected")

type ControlStatus struct {
	Connected         bool             `json:"connected"`
	DesiredRevision   uint64           `json:"desired_revision"`
	ReceivedRevision  uint64           `json:"received_revision"`
	AppliedRevision   uint64           `json:"applied_revision"`
	Usable            bool             `json:"usable"`
	LastContact       time.Time        `json:"last_contact"`
	Error             string           `json:"error"`
	Traffic           []TailnetTraffic `json:"traffic,omitempty"`
	TrafficObservedAt time.Time        `json:"traffic_observed_at"`
	ActiveConnections uint64           `json:"active_connections"`
}

func (c *EnrollmentClient) ControlStatus() ControlStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.controlStatus
}

func waitControl(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(max(d, 0))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *EnrollmentClient) RunControl(ctx context.Context, path string, apply func(context.Context, Policy) (PolicyApplication, error), status func(context.Context) (PolicyApplication, error)) error {
	if path == "" || apply == nil || status == nil {
		return errors.New("policy cache, application and status callbacks are required")
	}
	defer func() { c.mu.Lock(); c.controlStatus.Connected = false; c.mu.Unlock() }()
	loaded := false
	backoff := time.Second
	renewRequired := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		session := c.state.Session
		c.mu.Unlock()
		if session.NodeID == "" {
			if err := waitControl(ctx, time.Second); err != nil {
				return err
			}
			continue
		}
		if !loaded {
			loaded = true
			if p, err := LoadCache(path, session.ClusterID, session.NodeID, time.Now()); err == nil {
				result, err := apply(ctx, p)
				if err == nil && result.Revision == p.Revision {
					c.mu.Lock()
					c.controlStatus.ReceivedRevision = p.Revision
					c.controlStatus.AppliedRevision = result.Revision
					c.controlStatus.Usable = result.Usable
					c.controlStatus.Traffic = result.Traffic
					c.controlStatus.TrafficObservedAt = result.TrafficObservedAt
					c.mu.Unlock()
				}
			}
		}
		if renewRequired || session.InstanceID != c.instanceID || !time.Now().Add(10*time.Minute).Before(session.ExpiresAt) {
			if session.InstanceID != c.instanceID {
				if err := waitControl(ctx, time.Until(session.LeaseUntil)); err != nil {
					return err
				}
			}
			var err error
			session, err = c.RenewSession(ctx)
			if err != nil {
				if errors.Is(err, ErrIdentityConflict) {
					c.mu.Lock()
					c.controlStatus.Error = "identity_conflict"
					c.controlStatus.Usable = false
					c.mu.Unlock()
					os.Remove(path)
					return ErrIdentityConflict
				}
				c.mu.Lock()
				c.controlStatus.Connected = false
				c.controlStatus.Error = "controller_unavailable"
				c.mu.Unlock()
				if err := waitControl(ctx, backoff); err != nil {
					return err
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			renewRequired = false
		}
		err := c.controlConnection(ctx, path, session, apply, status)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errNodeSessionRejected) {
			renewRequired = true
		}
		if errors.Is(err, ErrIdentityConflict) {
			c.mu.Lock()
			c.controlStatus.Error = "identity_conflict"
			c.controlStatus.Usable = false
			c.mu.Unlock()
			os.Remove(path)
			return ErrIdentityConflict
		}
		c.mu.Lock()
		c.controlStatus.Connected = false
		c.controlStatus.Error = "controller_unavailable"
		last := c.controlStatus.LastContact
		c.mu.Unlock()
		if !last.IsZero() && time.Since(last) < time.Minute {
			backoff = time.Second
		}
		if err := waitControl(ctx, backoff); err != nil {
			return err
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *EnrollmentClient) controlConnection(ctx context.Context, path string, session NodeSession, apply func(context.Context, Policy) (PolicyApplication, error), status func(context.Context) (PolicyApplication, error)) error {
	applicationContext := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	origin := c.state.ControllerURL
	applied := c.controlStatus.AppliedRevision
	c.mu.Unlock()
	r, err := http.NewRequestWithContext(ctx, "GET", origin+"/cluster/v1/control?applied_revision="+strconv.FormatUint(applied, 10), nil)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+session.Token)
	client := &http.Client{Transport: c.httpClient.Transport, CheckRedirect: c.httpClient.CheckRedirect}
	response, err := client.Do(r)
	if err != nil {
		return errors.New("controller stream unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized {
			return errNodeSessionRejected
		}
		return errors.New("controller stream rejected")
	}
	c.mu.Lock()
	c.controlStatus.Connected = true
	c.mu.Unlock()
	heartbeatErrors := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		if err := c.controlHeartbeats(ctx, session, status); err != nil {
			heartbeatErrors <- err
			cancel()
		}
	})
	defer func() { cancel(); workers.Wait() }()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), MaxControlMessageBytes)
	for scanner.Scan() {
		var message ControlMessage
		if decodeNodeJSON(scanner.Bytes(), &message) != nil {
			return errors.New("invalid control message")
		}
		if message.Type == "ping" && message.Policy == nil {
			c.mu.Lock()
			c.controlStatus.LastContact = time.Now().UTC()
			c.mu.Unlock()
			continue
		}
		if message.Type != "policy" || message.Policy == nil {
			return errors.New("invalid control message")
		}
		p := *message.Policy
		ack := PolicyACK{Revision: p.Revision, State: "nack", Error: "invalid_policy"}
		if err := ValidatePolicy(p, session.ClusterID, session.NodeID); err != nil {
			var reply struct {
				OK bool `json:"ok"`
			}
			if err := c.post(ctx, "/cluster/v1/ack", ack, &reply, session.Token); err != nil {
				return err
			}
			continue
		}
		c.mu.Lock()
		c.controlStatus.DesiredRevision = p.Revision
		c.controlStatus.LastContact = time.Now().UTC()
		c.mu.Unlock()
		if err := SaveCache(path, p); err != nil {
			ack.Error = "persist_failed"
			var reply struct {
				OK bool `json:"ok"`
			}
			if err := c.post(ctx, "/cluster/v1/ack", ack, &reply, session.Token); err != nil {
				return err
			}
			continue
		}
		var reply struct {
			OK bool `json:"ok"`
		}
		c.mu.Lock()
		c.controlStatus.ReceivedRevision = p.Revision
		c.mu.Unlock()
		receiveErr := c.post(ctx, "/cluster/v1/ack", PolicyACK{Revision: p.Revision, State: "received"}, &reply, session.Token)
		// A failed confirmation cannot prevent applying an authenticated revocation.
		result, err := apply(applicationContext, p)
		if err != nil || result.Revision != p.Revision {
			ack.Error = "apply_failed"
			c.mu.Lock()
			c.controlStatus.Error = ack.Error
			c.mu.Unlock()
		} else {
			ack = PolicyACK{Revision: result.Revision, State: "applied", Usable: result.Usable}
			c.mu.Lock()
			c.controlStatus.AppliedRevision = result.Revision
			c.controlStatus.Usable = result.Usable
			c.controlStatus.Traffic = result.Traffic
			c.controlStatus.TrafficObservedAt = result.TrafficObservedAt
			c.controlStatus.Error = ""
			c.mu.Unlock()
		}
		if receiveErr != nil {
			return receiveErr
		}
		if err := c.post(ctx, "/cluster/v1/ack", ack, &reply, session.Token); err != nil {
			return err
		}
	}
	select {
	case err := <-heartbeatErrors:
		return err
	default:
	}
	if err := scanner.Err(); err != nil {
		return errors.New("controller stream disconnected")
	}
	return errors.New("controller stream ended")
}

func (c *EnrollmentClient) controlHeartbeats(ctx context.Context, session NodeSession, status func(context.Context) (PolicyApplication, error)) error {
	for {
		if !time.Now().Add(5 * time.Minute).Before(session.ExpiresAt) {
			return errors.New("node session needs renewal")
		}
		request := NodeHeartbeatRequest{}
		sampleCtx, cancel := context.WithTimeout(ctx, time.Second)
		sample, sampleErr := status(sampleCtx)
		cancel()
		if sampleErr == nil && !sample.TrafficObservedAt.IsZero() {
			connections := sample.ActiveConnections
			request.Report = &NodeReport{Revision: sample.Revision, Usable: sample.Usable, ObservedAt: sample.TrafficObservedAt, Traffic: sample.Traffic, ActiveConnections: &connections}
			c.mu.Lock()
			c.controlStatus.Usable = sample.Usable
			c.controlStatus.Traffic = sample.Traffic
			c.controlStatus.TrafficObservedAt = sample.TrafficObservedAt
			c.controlStatus.ActiveConnections = sample.ActiveConnections
			c.mu.Unlock()
		}
		var heartbeat NodeHeartbeat
		if err := c.post(ctx, "/cluster/v1/heartbeat", request, &heartbeat, session.Token); err != nil {
			return err
		}
		if heartbeat.NodeID != session.NodeID || heartbeat.LeaseUntil.IsZero() || heartbeat.HeartbeatIntervalMillis < 250 || heartbeat.HeartbeatIntervalMillis > 30000 {
			return errors.New("invalid node heartbeat")
		}
		c.mu.Lock()
		previous := c.state.Session.LeaseUntil
		c.state.Session.LeaseUntil = heartbeat.LeaseUntil
		err := c.saveRegistration()
		if err != nil {
			c.state.Session.LeaseUntil = previous
		}
		c.controlStatus.LastContact = time.Now().UTC()
		c.mu.Unlock()
		if err != nil {
			return errors.New("node lease persistence failed")
		}
		if err := waitControl(ctx, time.Duration(heartbeat.HeartbeatIntervalMillis)*time.Millisecond); err != nil {
			return err
		}
	}
}
