package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/admin"
	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/derper"
)

func nodeControllerURL(cfg config.Config) string {
	if cfg.Node.ControllerURL != "" {
		return cfg.Node.ControllerURL
	}
	return "https://" + cfg.Server.Hostname
}

func (d *Daemon) prepareNodePolicy(cfg config.NodeConfig) error {
	d.policyClient = derper.PolicyClient{Path: filepath.Join(cfg.StateDir, "policy.json"), SocketPath: filepath.Join(cfg.StateDir, "policy.sock"), MaxBudgetBPS: cfg.MaxBudgetBPS}
	clusterID, nodeID := d.nodeClient.PolicyBinding()
	if _, err := cluster.LoadCache(d.policyClient.Path, clusterID, nodeID, time.Now()); err != nil {
		if err := os.Remove(d.policyClient.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("discard invalid node policy: %w", err)
		}
	}
	if process, ok := d.derper.(*derper.Process); ok {
		process.Policy = d.policyClient
	}
	return nil
}

func (d *Daemon) startNodeControl(ctx context.Context) {
	ctx, d.nodeCancel = context.WithCancel(ctx)
	d.nodeDone = make(chan struct{})
	go func() {
		defer close(d.nodeDone)
		err := d.nodeClient.RunControl(ctx, d.policyClient.Path, d.policyClient.ApplyPolicy)
		if errors.Is(err, cluster.ErrIdentityConflict) {
			d.derperMu.Lock()
			defer d.derperMu.Unlock()
			d.mu.Lock()
			d.nodeConflict = true
			d.childOK = false
			d.expectedChildStops[d.childGeneration] = struct{}{}
			d.mu.Unlock()
			stopCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := d.derper.Stop(stopCtx); err != nil {
				d.reportFatal(err)
			}
			d.logf("ERROR node identity conflict; relay stopped")
		} else if err != nil && ctx.Err() == nil {
			d.reportFatal(fmt.Errorf("node control: %w", err))
		}
	}()
}

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
		return admin.Failure("node identity is not configured")
	}
	d.mu.RLock()
	controllerURL := nodeControllerURL(d.current)
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
