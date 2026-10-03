package control

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"

	"tailscale.com/net/stun"
)

type Probe struct {
	State      string    `json:"state"`
	ObservedAt time.Time `json:"observed_at"`
}

type NodeProbes struct {
	DERP Probe `json:"derp"`
	STUN Probe `json:"stun"`
}

func (v *domainVerifier) probe(ctx context.Context, domain string) NodeProbes {
	result := NodeProbes{}
	derpCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := v.probeDERP(derpCtx, domain)
	cancel()
	result.DERP = Probe{State: "failed", ObservedAt: time.Now().UTC()}
	if err == nil {
		result.DERP.State = "ok"
	}
	stunCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = v.probeSTUN(stunCtx, domain)
	cancel()
	result.STUN = Probe{State: "failed", ObservedAt: time.Now().UTC()}
	if err == nil {
		result.STUN.State = "ok"
	}
	return result
}

func (v *domainVerifier) probeDERP(ctx context.Context, domain string) error {
	transport := &http.Transport{DialContext: v.dialDomain, TLSClientConfig: &tls.Config{RootCAs: v.tlsRoots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequestWithContext(ctx, "GET", "https://"+domain+"/derp/probe", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("DERP probe failed")
	}
	return nil
}

func (v *domainVerifier) probeSTUN(ctx context.Context, domain string) error {
	ips, err := v.resolve(ctx, domain)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		conn, err := v.dial(ctx, "udp", net.JoinHostPort(ip.String(), "3478"))
		if err != nil {
			continue
		}
		host, port, splitErr := net.SplitHostPort(conn.RemoteAddr().String())
		remote, parseErr := netip.ParseAddr(host)
		if splitErr != nil || parseErr != nil || remote.Unmap() != ip.Unmap() || port != "3478" || !v.allowed(remote) {
			conn.Close()
			return errors.New("STUN target changed")
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		deadline, _ := ctx.Deadline()
		err = conn.SetDeadline(deadline)
		id := stun.NewTxID()
		if err == nil {
			_, err = conn.Write(stun.Request(id))
		}
		if err == nil {
			buffer := make([]byte, 1500)
			n, readErr := conn.Read(buffer)
			err = readErr
			if err == nil {
				got, _, parseErr := stun.ParseResponse(buffer[:n])
				err = parseErr
				if err == nil && got != id {
					err = errors.New("STUN transaction changed")
				}
			}
		}
		stop()
		conn.Close()
		if err == nil {
			return nil
		}
	}
	return errors.New("STUN probe failed")
}

func (s *Store) ProbeNode(ctx context.Context, actor Actor, id string) (NodeProbes, error) {
	n, err := s.Node(ctx, actor, id)
	if err != nil {
		return NodeProbes{}, err
	}
	if n.State != "registered" && n.State != "ready" && n.State != "offline" {
		return NodeProbes{}, ErrConflict
	}
	select {
	case s.nodeRequests <- struct{}{}:
		defer func() { <-s.nodeRequests }()
	default:
		return NodeProbes{}, ErrRateLimited
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := s.probeDomain(ctx, n.Domain)
	if ctx.Err() != nil {
		return NodeProbes{}, ctx.Err()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeProbes{}, err
	}
	defer tx.Rollback()
	if err := activeActor(ctx, tx, actor); err != nil {
		return NodeProbes{}, err
	}
	var domain, owner, state string
	if err := tx.QueryRowContext(ctx, "SELECT domain,owner_id,state FROM nodes WHERE id=?", id).Scan(&domain, &owner, &state); err != nil {
		return NodeProbes{}, err
	}
	if err := RequireOwner(actor, owner); err != nil {
		return NodeProbes{}, err
	}
	if domain != n.Domain || (state != "registered" && state != "ready" && state != "offline") {
		return NodeProbes{}, ErrConflict
	}
	body, err := json.Marshal(result)
	if err != nil {
		return NodeProbes{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO node_observations(node_id,probes_json) VALUES(?,?) ON CONFLICT(node_id) DO UPDATE SET probes_json=excluded.probes_json", id, body); err != nil {
		return NodeProbes{}, err
	}
	if err := writeAudit(ctx, tx, actor.ID, owner, "node", id, "node.probe"); err != nil {
		return NodeProbes{}, err
	}
	return result, tx.Commit()
}
