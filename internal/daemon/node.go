package daemon

import (
	"context"
	"errors"
	"fmt"

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
	policy := derper.PolicyClient{Path: filepath.Join(cfg.StateDir, "policy.json"), SocketPath: filepath.Join(cfg.StateDir, "policy.sock"), MaxBudgetBPS: cfg.MaxBudgetBPS}
	clusterID, nodeID := d.nodeClient.PolicyBinding()
	if _, err := cluster.LoadCache(policy.Path, clusterID, nodeID, time.Now()); err != nil {
		if err := os.Remove(policy.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("discard invalid node policy: %w", err)
		}
	}
	d.mu.Lock()
	d.policyClient = policy
	d.mu.Unlock()
	if process, ok := d.derper.(*derper.Process); ok {
		process.Policy = policy
	}
	return nil
}

func (d *Daemon) startNodeControl(ctx context.Context) {
	ctx, d.nodeCancel = context.WithCancel(ctx)
	done := make(chan struct{})
	d.nodeDone = done
	client, policy := d.nodeClient, d.policyClient
	go func() {
		defer close(done)
		err := client.RunControl(ctx, policy.Path, policy.ApplyPolicy, func(ctx context.Context) (cluster.PolicyApplication, error) { return d.nodeControlSample(ctx, policy) })
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
				d.setApplyError(err)
			}
			d.logf("ERROR node identity conflict; relay stopped")
		} else if err != nil && ctx.Err() == nil {
			d.setApplyError(fmt.Errorf("node control: %w", err))
		}
	}()
}

func (d *Daemon) nodeControlSample(ctx context.Context, policy derper.PolicyClient) (cluster.PolicyApplication, error) {
	sample, err := policy.Status(ctx)
	if err == nil {
		limit := d.activeConfig().Node.MaxBudgetBPS
		sample.LocalMaxBudgetBPS = &limit
	}
	return sample, err
}

func (d *Daemon) nodeAdmin(ctx context.Context, r admin.Request) admin.Response {
	d.opMu.Lock()
	defer d.opMu.Unlock()
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
