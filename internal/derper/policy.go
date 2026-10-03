package derper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

type PolicyClient struct {
	Path         string
	SocketPath   string
	MaxBudgetBPS uint64
}

type policyNotice struct {
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

type policyReply struct {
	Revision          uint64                   `json:"revision"`
	Usable            bool                     `json:"usable"`
	Error             string                   `json:"error"`
	Traffic           []cluster.TailnetTraffic `json:"traffic,omitempty"`
	TrafficObservedAt time.Time                `json:"traffic_observed_at"`
}

func (c PolicyClient) exchange(ctx context.Context, request policyNotice) (cluster.PolicyApplication, error) {
	if err := ctx.Err(); err != nil {
		return cluster.PolicyApplication{}, err
	}
	if c.SocketPath == "" {
		return cluster.PolicyApplication{}, errors.New("derper policy socket is required")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return cluster.PolicyApplication{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return cluster.PolicyApplication{}, err
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return cluster.PolicyApplication{}, err
	}
	body, err := bufio.NewReader(io.LimitReader(conn, cluster.MaxPolicyBytes+1)).ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return cluster.PolicyApplication{}, ctx.Err()
		}
		return cluster.PolicyApplication{}, err
	}
	if len(body) > cluster.MaxPolicyBytes {
		return cluster.PolicyApplication{}, errors.New("invalid derper policy response")
	}
	var reply policyReply
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&reply) != nil || d.Decode(new(any)) != io.EOF {
		return cluster.PolicyApplication{}, errors.New("invalid derper policy response")
	}
	result := cluster.PolicyApplication{Revision: reply.Revision, Usable: reply.Usable, Traffic: reply.Traffic, TrafficObservedAt: reply.TrafficObservedAt}
	if reply.Error != "" {
		return result, errors.New("derper policy application failed")
	}
	if request.Revision != 0 && reply.Revision != request.Revision {
		return result, errors.New("derper applied revision differs")
	}
	return result, nil
}

func (c PolicyClient) ApplyPolicy(ctx context.Context, p cluster.Policy) (cluster.PolicyApplication, error) {
	if err := ctx.Err(); err != nil {
		return cluster.PolicyApplication{}, err
	}
	if err := cluster.ValidatePolicy(p, p.ClusterID, p.NodeID); err != nil {
		return cluster.PolicyApplication{}, err
	}
	if c.MaxBudgetBPS != 0 && p.QoS.BudgetBPS > c.MaxBudgetBPS {
		return cluster.PolicyApplication{}, cluster.ErrHostBudget
	}
	if err := cluster.SaveCache(c.Path, p); err != nil {
		return cluster.PolicyApplication{}, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return cluster.PolicyApplication{}, err
	}
	h := sha256.Sum256(body)
	return c.exchange(ctx, policyNotice{Revision: p.Revision, Digest: hex.EncodeToString(h[:])})
}

func (c PolicyClient) Status(ctx context.Context) (cluster.PolicyApplication, error) {
	return c.exchange(ctx, policyNotice{})
}
