package cluster

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func testPolicy() Policy {
	t0 := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	return Policy{ClusterID: "cluster", NodeID: "node", Revision: 1, GeneratedAt: t0, Grants: []GrantPolicy{{GrantID: "grant", GrantRevision: 1, TailnetID: "tailnet", LastIdentitySuccess: t0, IdentityUntil: t0.Add(6 * time.Hour), ControlUntil: t0.Add(3 * time.Hour), Keys: []DeviceKey{{NodePublic: key.NewNode().Public().String(), NotAfter: t0.Add(10 * time.Hour)}}}}, QoS: QoSPolicy{BudgetBPS: 100000000, OwnerWeight: 8, SharedWeight: 2, Tailnets: []TailnetQoS{{TailnetID: "tailnet", Group: "owner", Weight: 1}}}}
}

func TestDecodePolicyStrictSizeAndFields(t *testing.T) {
	p := testPolicy()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePolicy(bytes.NewReader(body), "cluster", "node"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{append(append([]byte(nil), body...), []byte(` {}`)...), bytes.Replace(body, []byte(`"revision":1`), []byte(`"revision":1,"unknown":true`), 1), bytes.Repeat([]byte{' '}, MaxPolicyBytes+1)} {
		if _, err := DecodePolicy(bytes.NewReader(bad), "cluster", "node"); err == nil {
			t.Fatal("accepted invalid wire policy")
		}
	}
}

func TestValidatePolicyRejectsInvalidBoundaries(t *testing.T) {
	for name, change := range map[string]func(*Policy){
		"cluster":                    func(p *Policy) { p.ClusterID = "another" },
		"node":                       func(p *Policy) { p.NodeID = "another" },
		"revision":                   func(p *Policy) { p.Revision = 0 },
		"generated":                  func(p *Policy) { p.GeneratedAt = time.Time{} },
		"identity":                   func(p *Policy) { p.Grants[0].LastIdentitySuccess = time.Time{} },
		"identity until":             func(p *Policy) { p.Grants[0].IdentityUntil = time.Time{} },
		"control until":              func(p *Policy) { p.Grants[0].ControlUntil = time.Time{} },
		"key":                        func(p *Policy) { p.Grants[0].Keys[0].NodePublic = "invalid" },
		"duplicate key":              func(p *Policy) { p.Grants[0].Keys = append(p.Grants[0].Keys, p.Grants[0].Keys[0]) },
		"duplicate tailnet":          func(p *Policy) { p.Grants = append(p.Grants, p.Grants[0]) },
		"group":                      func(p *Policy) { p.QoS.Tailnets[0].Group = "guest" },
		"weight":                     func(p *Policy) { p.QoS.SharedWeight = 0 },
		"tailnet weight":             func(p *Policy) { p.QoS.Tailnets[0].Weight = 0 },
		"budget":                     func(p *Policy) { p.QoS.BudgetBPS = math.MaxUint64 },
		"ceiling":                    func(p *Policy) { p.QoS.SharedMaxBPS = math.MaxUint64 },
		"ceiling below byte":         func(p *Policy) { p.QoS.SharedMaxBPS = 1 },
		"tailnet ceiling below byte": func(p *Policy) { p.QoS.Tailnets[0].MaxBPS = 1 },
		"tailnet ceiling":            func(p *Policy) { p.QoS.Tailnets[0].MaxBPS = math.MaxUint64 },
	} {
		t.Run(name, func(t *testing.T) {
			p := testPolicy()
			change(&p)
			if err := ValidatePolicy(p, "cluster", "node"); err == nil {
				t.Fatal("accepted invalid policy")
			}
		})
	}
}

func TestExpiredPolicyAndEmptyPolicyCanRevoke(t *testing.T) {
	p := testPolicy()
	p.GeneratedAt = p.GeneratedAt.Add(24 * time.Hour)
	if err := ValidatePolicy(p, "cluster", "node"); err != nil {
		t.Fatal(err)
	}
	p.Grants = nil
	p.QoS.Tailnets = nil
	if err := ValidatePolicy(p, "cluster", "node"); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveUntilUsesIndependentAbsoluteDeadlines(t *testing.T) {
	p := testPolicy()
	g, k := p.Grants[0], p.Grants[0].Keys[0]
	if got := EffectiveUntil(g, k); !got.Equal(g.ControlUntil) {
		t.Fatalf("effective until %v", got)
	}
	k.NotAfter = g.LastIdentitySuccess.Add(time.Hour)
	if got := EffectiveUntil(g, k); !got.Equal(k.NotAfter) {
		t.Fatal("device expiry ignored")
	}
	k.NotAfter = time.Time{}
	g.ExplicitUntil = g.LastIdentitySuccess.Add(2 * time.Hour)
	if got := EffectiveUntil(g, k); !got.Equal(g.ExplicitUntil) {
		t.Fatal("explicit expiry ignored")
	}
}
