package daemon

import (
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/config"
)

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
