package daemon

import (
	"context"
	"errors"
	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"github.com/lsy223622/UniDERP/v2/internal/control"
	"time"
)

func (d *Daemon) Join(ctx context.Context, origin, code string) error {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.Role() != "member" || d.nodeClient == nil {
		return control.ErrConflict
	}
	_, id := d.nodeClient.PolicyBinding()
	if id != "" {
		return control.ErrConflict
	}
	d.stopNodeControl()
	cfg := d.desiredConfig()
	if cfg.Node.ControllerURL != origin {
		check := cfg.Clone()
		check.Node.ControllerURL = origin
		if err := check.Validate(); err != nil {
			return err
		}
		if _, err := d.nodeClient.ClearRegistration(); err != nil {
			return err
		}
		if err := d.nodeClient.SetControllerURL(origin); err != nil {
			return err
		}
		cfg.Node.ControllerURL = origin
		if err := d.saveLocalConfig(cfg); err != nil {
			return err
		}
	}
	if _, err := d.nodeClient.Enroll(ctx, code); err != nil {
		return err
	}
	if err := d.applyLocalConfig(ctx, cfg); err != nil {
		d.setApplyError(err)
		return err
	}
	return nil
}

func (d *Daemon) Leave(ctx context.Context) (bool, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.nodeClient == nil {
		return false, control.ErrConflict
	}
	d.stopNodeControl()
	if err := d.stopRelay(ctx); err != nil {
		return false, err
	}
	previous, err := d.nodeClient.ClearRegistration()
	if err != nil {
		d.setApplyError(err)
		return false, err
	}
	if err := cluster.RemoveCache(d.policyClient.Path); err != nil {
		d.setApplyError(err)
		return false, err
	}
	released := false
	if previous.Token != "" {
		releaseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		released = d.nodeClient.Release(releaseCtx, previous) == nil
		cancel()
	}
	cfg := d.desiredConfig()
	if !cfg.Controller.Enabled {
		cfg.Node.ControllerURL = ""
	}
	if err := d.saveLocalConfig(cfg); err != nil {
		d.setApplyError(err)
		return released, err
	}
	if err := d.applyLocalConfig(ctx, cfg); err != nil {
		d.setApplyError(err)
		return released, err
	}
	return released, nil
}

func (d *Daemon) RegisterLocal(ctx context.Context, actor control.Actor, name string) (control.Node, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if actor.Role != "admin" {
		return control.Node{}, control.ErrForbidden
	}
	if d.Role() != "controller" || d.nodeClient == nil {
		return control.Node{}, control.ErrConflict
	}
	_, id := d.nodeClient.PolicyBinding()
	if id != "" {
		return d.controllerStore.Node(ctx, actor, id)
	}
	cfg := d.activeConfig()
	if cfg.Server.Hostname == "" {
		return control.Node{}, control.ErrInvalid
	}
	if code := d.nodeClient.PendingEnrollmentCode(); code != "" {
		if session, err := d.nodeClient.Enroll(ctx, code); err == nil {
			if err := d.applyLocalConfig(ctx, cfg); err != nil {
				d.setApplyError(err)
				return control.Node{}, err
			}
			return d.controllerStore.Node(ctx, actor, session.NodeID)
		} else if errors.Is(err, cluster.ErrEnrollmentCodeRejected) {
			if _, err := d.nodeClient.ClearRegistration(); err != nil {
				return control.Node{}, err
			}
		} else {
			return control.Node{}, err
		}
	}
	nodes, err := d.controllerStore.ListNodes(ctx, actor)
	if err != nil {
		return control.Node{}, err
	}
	var node control.Node
	var enrollment control.Enrollment
	for _, n := range nodes {
		if n.Domain == cfg.Server.Hostname {
			node = n
			break
		}
	}
	if node.ID == "" {
		node, enrollment, err = d.controllerStore.CreateNode(ctx, actor, name, cfg.Server.Hostname, cfg.Node.DERPPort, cfg.Node.STUNPort)
	} else {
		if node.OwnerID != actor.ID || node.State != "pending" {
			return control.Node{}, control.ErrConflict
		}
		enrollment, err = d.controllerStore.IssueEnrollment(ctx, actor, node.ID)
	}
	if err != nil {
		return control.Node{}, err
	}
	if _, err := d.nodeClient.Enroll(ctx, enrollment.Code); err != nil {
		return control.Node{}, err
	}
	d.stopNodeControl()
	if err := d.prepareNodePolicy(cfg.Node); err != nil {
		return control.Node{}, err
	}
	if !d.derper.Running() {
		if err := d.syncDerper(ctx); err != nil {
			return control.Node{}, err
		}
	}
	if d.runCtx == nil {
		return control.Node{}, errors.New("daemon is not running")
	}
	d.startNodeControl(d.runCtx)
	return d.controllerStore.Node(ctx, actor, node.ID)
}
