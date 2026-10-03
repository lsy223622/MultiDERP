package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/control"
)

func (d *Daemon) startController(ctx context.Context, cfg config.ControllerConfig) error {
	store, err := control.OpenStore(cfg.Database)
	if err != nil {
		return fmt.Errorf("open controller database: %w", err)
	}
	d.controllerStore = store
	if err := store.EnableIdentity(cfg.KeyFile); err != nil {
		return fmt.Errorf("load controller key: %w", err)
	}
	if err := store.ConfigureNodes(cfg.AllowedNodeCIDRs); err != nil {
		return fmt.Errorf("configure node registration: %w", err)
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen controller: %w", err)
	}
	d.controllerListener = listener
	d.controllerServer = &http.Server{Handler: control.NewHTTPHandler(store), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
	syncCtx, cancel := context.WithCancel(ctx)
	d.controllerCancel = cancel
	d.controllerDone = make(chan struct{})
	go func() {
		defer close(d.controllerDone)
		if err := store.RunIdentitySync(syncCtx); err != nil {
			d.reportFatal(fmt.Errorf("identity synchronization: %w", err))
		}
	}()
	go func() {
		if err := d.controllerServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.reportFatal(fmt.Errorf("controller server: %w", err))
		}
	}()
	return nil
}

func (d *Daemon) controllerAdmin(ctx context.Context, request admin.Request) admin.Response {
	if d.controllerStore == nil {
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
