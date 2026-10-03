package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"tailscale.com/types/key"
)

const MaxPolicyBytes = 8 << 20

type Policy struct {
	ClusterID   string        `json:"cluster_id"`
	NodeID      string        `json:"node_id"`
	Revision    uint64        `json:"revision"`
	GeneratedAt time.Time     `json:"generated_at"`
	Grants      []GrantPolicy `json:"grants"`
	QoS         QoSPolicy     `json:"qos"`
}

type GrantPolicy struct {
	GrantID             string      `json:"grant_id"`
	GrantRevision       uint64      `json:"grant_revision"`
	TailnetID           string      `json:"tailnet_id"`
	LastIdentitySuccess time.Time   `json:"last_identity_success"`
	IdentityUntil       time.Time   `json:"identity_until"`
	ControlUntil        time.Time   `json:"control_until"`
	ExplicitUntil       time.Time   `json:"explicit_until"`
	Keys                []DeviceKey `json:"keys"`
}

type DeviceKey struct {
	NodePublic string    `json:"node_public"`
	NotAfter   time.Time `json:"not_after"`
}

type QoSPolicy struct {
	BudgetBPS    uint64       `json:"budget_bps"`
	OwnerWeight  uint32       `json:"owner_weight"`
	SharedWeight uint32       `json:"shared_weight"`
	SharedMaxBPS uint64       `json:"shared_max_bps"`
	Tailnets     []TailnetQoS `json:"tailnets"`
}

type TailnetQoS struct {
	TailnetID string `json:"tailnet_id"`
	Group     string `json:"group"`
	Weight    uint32 `json:"weight"`
	MaxBPS    uint64 `json:"max_bps"`
}

func EffectiveUntil(g GrantPolicy, k DeviceKey) time.Time {
	until := g.IdentityUntil
	for _, d := range []time.Time{g.ControlUntil, g.ExplicitUntil, k.NotAfter} {
		if !d.IsZero() && d.Before(until) {
			until = d
		}
	}
	return until
}

func DecodePolicy(r io.Reader, clusterID, nodeID string) (Policy, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxPolicyBytes+1))
	if err != nil || len(body) > MaxPolicyBytes {
		return Policy{}, errors.New("policy unreadable or too large")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("decode policy: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Policy{}, errors.New("trailing policy data")
	}
	if err := ValidatePolicy(p, clusterID, nodeID); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func ValidatePolicy(p Policy, clusterID, nodeID string) error {
	validID := func(id string) bool { return id != "" && len(id) <= 128 && strings.TrimSpace(id) == id }
	validWeight := func(w uint32) bool { return w > 0 && w <= 1024 }
	validCeiling := func(bps uint64) bool { return bps == 0 || (bps >= 8 && bps <= math.MaxInt64) }
	if !validID(p.ClusterID) || !validID(p.NodeID) || p.ClusterID != clusterID || p.NodeID != nodeID || p.Revision == 0 || p.GeneratedAt.IsZero() {
		return errors.New("invalid policy identity or revision")
	}
	q := p.QoS
	if q.BudgetBPS < 8 || q.BudgetBPS > math.MaxInt64 || !validCeiling(q.SharedMaxBPS) || !validWeight(q.OwnerWeight) || !validWeight(q.SharedWeight) {
		return errors.New("invalid policy budget or group weight")
	}
	if len(p.Grants) > 4096 || len(q.Tailnets) > 4096 {
		return errors.New("too many policy tailnets")
	}
	tailnets := make(map[string]bool, len(p.Grants))
	grants := make(map[string]bool, len(p.Grants))
	keys := make(map[key.NodePublic]bool)
	for _, g := range p.Grants {
		if !validID(g.GrantID) || !validID(g.TailnetID) || g.GrantRevision == 0 || tailnets[g.TailnetID] || grants[g.GrantID] {
			return errors.New("invalid or duplicate grant identity")
		}
		if g.LastIdentitySuccess.IsZero() || g.IdentityUntil.IsZero() || g.ControlUntil.IsZero() || g.IdentityUntil.Before(g.LastIdentitySuccess) {
			return errors.New("invalid grant identity deadlines")
		}
		tailnets[g.TailnetID] = true
		grants[g.GrantID] = true
		for _, k := range g.Keys {
			var public key.NodePublic
			if public.UnmarshalText([]byte(k.NodePublic)) != nil || public.IsZero() || keys[public] {
				return errors.New("invalid or duplicate device public key")
			}
			keys[public] = true
			if len(keys) > 65536 {
				return errors.New("too many policy device keys")
			}
		}
	}
	seen := make(map[string]bool, len(q.Tailnets))
	for _, t := range q.Tailnets {
		if !tailnets[t.TailnetID] || seen[t.TailnetID] || (t.Group != "owner" && t.Group != "shared") || !validWeight(t.Weight) || !validCeiling(t.MaxBPS) {
			return errors.New("invalid tailnet bandwidth rule")
		}
		seen[t.TailnetID] = true
	}
	if len(seen) != len(tailnets) {
		return errors.New("missing tailnet bandwidth rule")
	}
	return nil
}
