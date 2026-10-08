package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRestartDoesNotRenewCache(t *testing.T) {
	p := testPolicy()
	path := filepath.Join(t.TempDir(), "policy.json")
	t0 := p.GeneratedAt
	if err := saveCacheAt(path, p, t0); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCache(path, p.ClusterID, p.NodeID, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != p.Revision || !loaded.Grants[0].IdentityUntil.Equal(p.Grants[0].IdentityUntil) || !loaded.Grants[0].ControlUntil.Equal(p.Grants[0].ControlUntil) {
		t.Fatal("restart renewed deadline")
	}
	if err := saveCacheAt(path, p, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadCache(path, p.ClusterID, p.NodeID, t0.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if EffectiveUntil(loaded.Grants[0], loaded.Grants[0].Keys[0]).After(t0.Add(4 * time.Hour)) {
		t.Fatal("cache remained authorized after expiry")
	}
}

func TestWrongNodePolicyIsRejected(t *testing.T) {
	p := testPolicy()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := saveCacheAt(path, p, p.GeneratedAt); err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][2]string{{"other", p.NodeID}, {p.ClusterID, "other"}} {
		if _, err := LoadCache(path, ids[0], ids[1], p.GeneratedAt); err == nil {
			t.Fatal("wrong binding accepted")
		}
	}
	p.NodeID = "other"
	if err := saveCacheAt(path, p, p.GeneratedAt); err == nil {
		t.Fatal("cache switched node")
	}
}

func TestCacheRejectsRollbackAndChangedSameRevision(t *testing.T) {
	p := testPolicy()
	path := filepath.Join(t.TempDir(), "policy.json")
	t0 := p.GeneratedAt
	if err := saveCacheAt(path, p, t0); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p.Revision++
	p.Grants[0].ControlUntil = t0.Add(4 * time.Hour)
	if err := saveCacheAt(path, p, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	changed := p
	changed.QoS.BudgetBPS++
	if err := saveCacheAt(path, changed, t0.Add(time.Minute)); err == nil {
		t.Fatal("same revision changed")
	}
	p.Revision--
	if err := saveCacheAt(path, p, t0.Add(time.Minute)); err == nil {
		t.Fatal("old revision accepted")
	}
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCache(path, p.ClusterID, p.NodeID, t0.Add(time.Minute)); err == nil {
		t.Fatal("disk replay accepted")
	}
}

func TestCacheClockRollbackAndTruncationFailClosed(t *testing.T) {
	for _, damage := range []string{"clock", "policy", "watermark", "missing_watermark", "future_watermark"} {
		t.Run(damage, func(t *testing.T) {
			p := testPolicy()
			path := filepath.Join(t.TempDir(), "policy.json")
			now := p.GeneratedAt.Add(time.Hour)
			if err := saveCacheAt(path, p, p.GeneratedAt); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCache(path, p.ClusterID, p.NodeID, now); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "clock":
				now = p.GeneratedAt
			case "policy":
				os.WriteFile(path, []byte(`{"revision":`), 0600)
			case "watermark":
				os.WriteFile(path+".watermark", []byte("truncated"), 0600)
			case "missing_watermark":
				os.Remove(path + ".watermark")
			case "future_watermark":
				p.Revision++
				b, _ := json.Marshal(p)
				mark := cacheWatermark{ClusterID: p.ClusterID, NodeID: p.NodeID, Revision: p.Revision, Digest: cacheDigest(b), ObservedAt: now}
				b, _ = json.Marshal(mark)
				os.WriteFile(path+".watermark", b, 0600)
			}
			if _, err := LoadCache(path, p.ClusterID, p.NodeID, now); err == nil {
				t.Fatal("damaged cache allowed")
			}
		})
	}
}
