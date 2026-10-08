package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/control"
)

func (d *Daemon) startManagement(cfg config.Config) error {
	database, listen := filepath.Join(cfg.Storage.StateDir, "controller.sqlite"), "127.0.0.1:3341"
	if cfg.Controller.Enabled {
		database, listen = cfg.Controller.Database, cfg.Controller.Listen
	}
	store, err := control.OpenStore(database)
	if err != nil {
		return fmt.Errorf("open controller database: %w", err)
	}
	d.controllerStore = store
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen controller: %w", err)
	}
	d.controllerListener = listener
	controller := control.NewLocalHTTPHandler(store, d)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/cluster/v1/domain-challenge/") {
			d.mu.RLock()
			c := d.nodeClient
			d.mu.RUnlock()
			if c != nil {
				c.DomainHandler().ServeHTTP(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		controller.ServeHTTP(w, r)
	})
	d.controllerServer = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
	go func() {
		if err := d.controllerServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.reportFatal(fmt.Errorf("controller server: %w", err))
		}
	}()
	if cfg.Server.Management.Listen != "" {
		l, err := net.Listen("tcp", cfg.Server.Management.Listen)
		if err != nil {
			return fmt.Errorf("listen independent management: %w", err)
		}
		d.managementListener = l
		d.managementServer = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
		go func() {
			if err := d.managementServer.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				d.reportFatal(err)
			}
		}()
	}
	return nil
}

func (d *Daemon) prepareRole(ctx context.Context, cfg config.Config) error {
	if cfg.SetupRequired {
		return nil
	}
	if cfg.Controller.Enabled && d.controllerCancel == nil {
		if err := d.controllerStore.EnableIdentity(cfg.Controller.KeyFile); err != nil {
			return err
		}
		if err := d.controllerStore.ConfigureNodes(cfg.Controller.AllowedNodeCIDRs); err != nil {
			return err
		}
		syncCtx, cancel := context.WithCancel(ctx)
		d.controllerCancel = cancel
		done := make(chan struct{})
		d.controllerDone = done
		go func() {
			defer close(done)
			if err := d.controllerStore.RunIdentitySync(syncCtx); err != nil {
				d.reportFatal(err)
			}
		}()
	}
	origin := cfg.Node.ControllerURL
	if cfg.Controller.Enabled && cfg.Server.Hostname != "" {
		origin = nodeControllerURL(cfg)
	}
	d.mu.RLock()
	client := d.nodeClient
	d.mu.RUnlock()
	if client == nil {
		var err error
		client, err = cluster.NewEnrollmentClient(origin, cfg.Node.StateDir)
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.nodeClient = client
		d.mu.Unlock()
	} else if cfg.Server.Hostname != d.activeConfig().Server.Hostname && cfg.Controller.Enabled {
		if err := client.SetControllerURL(origin); err != nil {
			return err
		}
	}
	return d.prepareNodePolicy(cfg.Node)
}

func (d *Daemon) controllerAdmin(ctx context.Context, request admin.Request) admin.Response {
	if d.controllerStore == nil || d.Role() != "controller" {
		return admin.Failure("controller is not enabled")
	}
	if request.Action == "controller.init" {
		actor, err := d.controllerStore.InitializeAdmin(ctx, request.Username, request.Password)
		if err != nil {
			return admin.Failure("administrator initialization failed")
		}
		return admin.Success("administrator initialized", actor)
	}
	if err := d.controllerStore.RecoverAdmin(ctx, request.UserID, request.Password); err != nil {
		return admin.Failure("administrator recovery failed")
	}
	return admin.Success("administrator recovered", nil)
}
