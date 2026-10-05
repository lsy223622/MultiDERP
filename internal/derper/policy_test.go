package derper

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
)

func applicationPolicy(t *testing.T) cluster.Policy {
	t.Helper()
	f, err := os.Open("../cluster/testdata/policy.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := cluster.DecodePolicy(f, "cluster", "node")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p.GeneratedAt = now
	p.Grants[0].LastIdentitySuccess = now
	p.Grants[0].IdentityUntil = now.Add(time.Hour)
	p.Grants[0].ControlUntil = now.Add(time.Hour)
	p.Grants[0].ExplicitUntil = time.Time{}
	for i := range p.Grants[0].Keys {
		p.Grants[0].Keys[i].NotAfter = time.Time{}
	}
	return p
}

func TestApplyPolicyRequiresActualMatchingChildACK(t *testing.T) {
	dir, err := os.MkdirTemp("", "ud-pc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	c := PolicyClient{Path: filepath.Join(dir, "policy.json"), SocketPath: filepath.Join(dir, "policy.sock")}
	ln, err := net.Listen("unix", c.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		var request map[string]any
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		if len(request) != 2 || request["digest"] == nil {
			done <- errors.New("IPC contained policy authority")
			return
		}
		done <- json.NewEncoder(conn).Encode(map[string]any{"revision": uint64(request["revision"].(float64)) + 1, "usable": true, "error": ""})
	}()
	p := applicationPolicy(t)
	if _, err := c.ApplyPolicy(t.Context(), p); err == nil {
		t.Fatal("wrong child revision reported applied")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cached, err := cluster.LoadCache(c.Path, p.ClusterID, p.NodeID, time.Now())
	if err != nil || cached.Revision != p.Revision {
		t.Fatal("received policy not durable", err)
	}
}

func TestApplyPolicyPersistsControllerBudgetAndRespectsCancellation(t *testing.T) {
	p := applicationPolicy(t)
	dir := t.TempDir()
	c := PolicyClient{Path: filepath.Join(dir, "policy.json"), SocketPath: filepath.Join(dir, "policy.sock"), MaxBudgetBPS: 80000000}
	_, _ = c.ApplyPolicy(t.Context(), p)
	if cached, err := cluster.LoadCache(c.Path, p.ClusterID, p.NodeID, time.Now()); err != nil || cached.QoS.BudgetBPS != p.QoS.BudgetBPS {
		t.Fatal("controller policy not preserved", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.MaxBudgetBPS = 0
	if _, err := c.ApplyPolicy(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled application proceeded", err)
	}
}
