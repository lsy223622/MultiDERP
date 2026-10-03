package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"github.com/lsy223622/UniDERP/v2/internal/config"
)

func (d *Daemon) startNode(ctx context.Context, cfg config.NodeConfig, listen string) error {
	c, err := cluster.NewEnrollmentClient(cfg.ControllerURL, cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load node identity: %w", err)
	}
	d.nodeClient = c
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen node management: %w", err)
	}
	d.controllerListener = l
	d.controllerServer = &http.Server{Handler: c.DomainHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
	go func() {
		if err := d.controllerServer.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.reportFatal(fmt.Errorf("node management server: %w", err))
		}
	}()
	return nil
}

func (d *Daemon) nodeAdmin(ctx context.Context, r admin.Request) admin.Response {
	if d.nodeClient == nil {
		return admin.Failure("member node is not enabled")
	}
	d.mu.RLock()
	controllerURL := d.current.Node.ControllerURL
	d.mu.RUnlock()
	if r.ControllerURL != controllerURL {
		return admin.Failure("controller address differs from node configuration")
	}
	session, err := d.nodeClient.Enroll(ctx, r.EnrollmentCode)
	if err != nil {
		return admin.Failure("node registration failed")
	}
	return admin.Success("node registered; waiting for policy and relay readiness", map[string]string{"node_id": session.NodeID, "state": "registered"})
}
