package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/control"
)

func TestControllerLocalAdminAndLifecycle(t *testing.T) {
	dir := shortTempDir(t)
	cfg := config.Default()
	cfg.Server.Admin.Socket = filepath.Join(dir, "run", "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	cfg.Controller.Listen = freeLoopbackAddress(t)
	cfg.Controller.Database = filepath.Join(dir, "controller.sqlite")
	cfg.Controller.KeyFile = filepath.Join(dir, "controller.key")
	path := filepath.Join(dir, "config.yaml")
	if err := config.WriteAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	d := New(t.Context(), Options{ConfigPath: path, AdmissionAddress: freeLoopbackAddress(t), Logger: log.New(&logs, "", 0)})
	if err := d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Shutdown() })
	client := admin.Client{SocketPath: cfg.Server.Admin.Socket, Timeout: time.Second * 5}
	password := "local administrator password"
	response, err := client.Call(t.Context(), admin.Request{Action: "controller.init", Username: "admin", Password: password})
	if err != nil || !response.OK {
		t.Fatalf("init: %v %s", err, response.Error)
	}
	var user control.Actor
	if err := json.Unmarshal(response.Data, &user); err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second * 3}
	url := "http://" + d.controllerListener.Addr().String() + "/api/v1/login"
	login := func() *http.Cookie {
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
		r, err := httpClient.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("login status %d", r.StatusCode)
		}
		return r.Cookies()[0]
	}
	cookie := login()
	response, err = client.Call(t.Context(), admin.Request{Action: "controller.recover", UserID: user.ID, Password: password})
	if err != nil || !response.OK {
		t.Fatalf("recover: %v %s", err, response.Error)
	}
	req, _ := http.NewRequest("GET", "http://"+d.controllerListener.Addr().String()+"/api/v1/session", nil)
	req.AddCookie(cookie)
	r, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal("recovery retained session")
	}
	if strings.Contains(logs.String(), password) || strings.Contains(logs.String(), cookie.Value) {
		t.Fatal("secret logged")
	}
	address := d.controllerListener.Addr().String()
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if _, err := httpClient.Get("http://" + address + "/api/v1/session"); err == nil {
		t.Fatal("controller still serving")
	}
}
