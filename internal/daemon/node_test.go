package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/derper"
)

func TestHeartbeatBudgetReportUsesActiveLocalLimit(t *testing.T) {
	d := New(t.Context(), Options{})
	d.current = config.Default()
	d.current.Node.MaxBudgetBPS = 40000000
	d.desired = d.current.Clone()
	d.desired.Node.MaxBudgetBPS = 30000000
	path := filepath.Join(shortTempDir(t), "budget.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		var request map[string]any
		err = json.NewDecoder(conn).Decode(&request)
		if err == nil {
			err = json.NewEncoder(conn).Encode(map[string]any{"effective_budget_bps": 40000000, "revision": 7, "usable": true, "traffic_observed_at": time.Now().UTC()})
		}
		done <- err
	}()
	sample, err := d.nodeControlSample(context.Background(), derper.PolicyClient{SocketPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if sample.LocalMaxBudgetBPS == nil || *sample.LocalMaxBudgetBPS != 40000000 || sample.EffectiveBudgetBPS != 40000000 || sample.Revision != 7 {
		t.Fatal("pending limit leaked into sample", sample)
	}
}

func TestMemberNodeRegistrationBoundary(t *testing.T) {
	cfg := config.Default()
	cfg.Controller = &config.ControllerConfig{Enabled: false}
	cfg.Node.ControllerURL = "https://controller.example.com"
	cfg.Node.StateDir = t.TempDir()
	cfg.Storage.StateDir = cfg.Node.StateDir
	cfg.Server.Management.Listen = freeLoopbackAddress(t)
	d := New(t.Context(), Options{Logger: log.New(io.Discard, "", 0)})
	d.current = cfg
	if err := d.startManagement(cfg); err != nil {
		t.Fatal(err)
	}
	if err := d.prepareRole(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Shutdown() })
	if _, err := os.Stat(filepath.Join(cfg.Node.StateDir, "node.key")); err != nil {
		t.Fatal(err)
	}
	if response := d.nodeAdmin(t.Context(), admin.Request{ControllerURL: "https://other.example.com", EnrollmentCode: "secret"}); response.OK {
		t.Fatal("controller binding changed")
	}
	if response := d.controllerAdmin(t.Context(), admin.Request{Action: "controller.init"}); response.OK {
		t.Fatal("member became controller")
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	for _, path := range []string{"/cluster/v1/domain-challenge/unknown", "/api/v1/users", "/cluster/v1/enroll"} {
		r, err := client.Get("http://" + d.controllerListener.Addr().String() + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		want := 404
		if path == "/api/v1/users" {
			want = 401
		}
		if r.StatusCode != want {
			t.Fatal("member exposed controller endpoint", path, r.StatusCode)
		}
	}
}

func TestNodeReloadRetainsActiveControllerBinding(t *testing.T) {
	cfg := config.Default()
	cfg.Controller = &config.ControllerConfig{Enabled: false}
	cfg.Node.ControllerURL = "https://old.example.com"
	d := New(t.Context(), Options{Logger: log.New(io.Discard, "", 0)})
	d.current = cfg
	d.desired = cfg.Clone()
	d.started = true
	t.Cleanup(func() { d.Shutdown() })
	updated := cfg.Clone()
	updated.Node.ControllerURL = ""
	updated.Controller = config.Default().Controller
	if err := d.applyCommittedConfig(t.Context(), updated, nil); err != nil {
		t.Fatal(err)
	}
	active := d.activeConfig()
	if active.Node.ControllerURL != cfg.Node.ControllerURL || active.Controller.Enabled {
		t.Fatal("reload replaced running node/controller binding")
	}
	if !d.pendingRestart || d.desiredConfig().Node.ControllerURL != updated.Node.ControllerURL {
		t.Fatal("restart change not retained")
	}
}
