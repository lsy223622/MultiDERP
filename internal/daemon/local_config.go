package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/lsy223622/UniDERP/v2/internal/cluster"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"github.com/lsy223622/UniDERP/v2/internal/control"
)

func configRole(cfg config.Config) string {
	if cfg.SetupRequired {
		return "setup"
	}
	if cfg.Controller.Enabled {
		return "controller"
	}
	return "member"
}

func (d *Daemon) Role() string { return configRole(d.activeConfig()) }

func localSettings(cfg config.Config) control.LocalSettings {
	return control.LocalSettings{Role: configRole(cfg), Hostname: cfg.Server.Hostname, DERPListen: cfg.Server.DERP.Listen, STUNListen: cfg.Server.DERP.STUNListen, TLSMode: cfg.Server.DERP.TLSMode, CertMode: cfg.Server.DERP.CertMode, DERPPort: cfg.Node.DERPPort, STUNPort: cfg.Node.STUNPort, MaxBudgetBPS: cfg.Node.MaxBudgetBPS, LoggingLevel: cfg.Logging.Level}
}

func (d *Daemon) Status(ctx context.Context) (control.LocalStatus, error) {
	d.mu.RLock()
	active, saved := d.current.Clone(), d.desired.Clone()
	client, policy := d.nodeClient, d.policyClient
	status := control.LocalStatus{Role: configRole(active), Saved: localSettings(saved), Active: localSettings(active), PendingApply: d.pendingRestart, ApplyError: d.applyError}
	childOK := d.childOK
	d.mu.RUnlock()
	status.ControllerURL = saved.Node.ControllerURL
	if active.Controller.Enabled && active.Server.Hostname != "" {
		status.ControllerURL = nodeControllerURL(active)
	}
	if client != nil {
		status.ClusterID, status.NodeID = client.PolicyBinding()
		status.Joined = status.NodeID != ""
		status.Control = client.ControlStatus()
		if status.Joined {
			if p, err := cluster.LoadCache(policy.Path, status.ClusterID, status.NodeID, time.Now()); err == nil {
				status.PolicyBudgetBPS = p.QoS.BudgetBPS
				status.QoS = &p.QoS
			}
		}
	}
	status.Control.Usable = false
	if childOK {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		applied, err := policy.Status(probeCtx)
		cancel()
		if err == nil {
			status.EffectiveBudgetBPS = applied.EffectiveBudgetBPS
			status.Control.Usable = applied.Usable
			status.Control.AppliedRevision = applied.Revision
			status.Control.Traffic = applied.Traffic
			status.Control.TrafficObservedAt = applied.TrafficObservedAt
			status.Control.ActiveConnections = applied.ActiveConnections
		}
	}
	return status, nil
}

func (d *Daemon) setApplyError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.applyError = err.Error()
	d.pendingRestart = true
}

func (d *Daemon) saveLocalConfig(cfg config.Config) error {
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := config.WriteAtomic(d.configPath, cfg); err != nil {
		return err
	}
	d.mu.Lock()
	d.desired = cfg.Clone()
	d.pendingRestart = !reflect.DeepEqual(d.current, cfg)
	d.mu.Unlock()
	return nil
}

func (d *Daemon) SaveSettings(ctx context.Context, settings control.LocalSettings) error {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, active := d.desiredConfig(), d.activeConfig()
	settings.Hostname = strings.TrimSpace(settings.Hostname)
	if config.ValidateHostname(settings.Hostname) != nil {
		return &control.InputError{Code: "invalid_hostname", Field: "hostname"}
	}
	if settings.Role != "controller" && settings.Role != "member" {
		return control.ErrInvalid
	}
	if !active.SetupRequired && settings.Role != configRole(active) {
		return control.ErrConflict
	}
	d.mu.RLock()
	client := d.nodeClient
	d.mu.RUnlock()
	if client != nil {
		_, node := client.PolicyBinding()
		if node != "" && settings.Hostname != active.Server.Hostname {
			return errors.New("已注册节点更改域名前，请先退出集群并在主控修改节点域名")
		}
	}
	cfg.SetupRequired = false
	if settings.Role == "member" {
		cfg.Controller = &config.ControllerConfig{Enabled: false}
	} else {
		if !cfg.Controller.Enabled {
			cfg.Controller = config.Default().Controller
			cfg.Controller.Database = filepath.Join(cfg.Storage.StateDir, "controller.sqlite")
			cfg.Controller.KeyFile = filepath.Join(cfg.Storage.StateDir, "controller.key")
		}
		cfg.Node.ControllerURL = ""
	}
	cfg.Server.Hostname = settings.Hostname
	cfg.Server.DERP.Listen = settings.DERPListen
	cfg.Server.DERP.STUNListen = settings.STUNListen
	cfg.Server.DERP.TLSMode = settings.TLSMode
	cfg.Server.DERP.CertMode = settings.CertMode
	if settings.TLSMode == "external" {
		cfg.Server.DERP.CertDir = ""
	} else if cfg.Server.DERP.CertDir == "" {
		cfg.Server.DERP.CertDir = filepath.Join(cfg.Storage.StateDir, "certs")
	}
	cfg.Node.DERPPort, cfg.Node.STUNPort = settings.DERPPort, settings.STUNPort
	cfg.Node.MaxBudgetBPS = settings.MaxBudgetBPS
	cfg.Logging.Level = settings.LoggingLevel
	return d.saveLocalConfig(cfg)
}

func (d *Daemon) stopNodeControl() {
	if d.nodeCancel != nil {
		d.nodeCancel()
		<-d.nodeDone
		d.nodeCancel = nil
		d.nodeDone = nil
	}
}

func (d *Daemon) stopRelay(ctx context.Context) error {
	d.derperMu.Lock()
	defer d.derperMu.Unlock()
	d.mu.Lock()
	d.childOK = false
	if d.derper.Running() {
		d.expectedChildStops[d.childGeneration] = struct{}{}
	}
	d.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return d.derper.Stop(stopCtx)
}

func (d *Daemon) ApplySettings(ctx context.Context) error {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	err := d.applyLocalConfig(ctx, d.desiredConfig())
	if err != nil {
		d.setApplyError(err)
	}
	return err
}

func (d *Daemon) applyLocalConfig(ctx context.Context, cfg config.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	active := d.activeConfig()
	if !active.SetupRequired && configRole(active) != configRole(cfg) {
		return control.ErrConflict
	}
	if active.Storage != cfg.Storage || active.Server.Management != cfg.Server.Management || active.Server.Admin != cfg.Server.Admin || active.Server.Health != cfg.Server.Health || active.Node.StateDir != cfg.Node.StateDir {
		return errors.New("部署路径与管理监听变更需要重启服务")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	d.stopNodeControl()
	if err := d.stopRelay(ctx); err != nil {
		return err
	}
	if err := d.prepareRole(d.runCtx, cfg); err != nil {
		return err
	}
	d.mu.Lock()
	d.nodeConflict = false
	d.mu.Unlock()
	if err := d.syncDerperConfig(ctx, cfg); err != nil {
		return err
	}
	if d.nodeClient != nil && !active.SetupRequired && (active.Node.DERPPort != cfg.Node.DERPPort || active.Node.STUNPort != cfg.Node.STUNPort || active.Node.ControllerURL != cfg.Node.ControllerURL) {
		if _, id := d.nodeClient.PolicyBinding(); id != "" {
			portsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := d.nodeClient.SetPorts(portsCtx, cfg.Node.DERPPort, cfg.Node.STUNPort)
			cancel()
			if err != nil {
				return err
			}
		}
	}
	d.logFilter.SetLevel(cfg.Logging.Level)
	d.mu.Lock()
	d.current = cfg.Clone()
	d.pendingRestart = false
	d.applyError = ""
	d.mu.Unlock()
	if d.nodeClient != nil && d.derper.Running() {
		d.startNodeControl(d.runCtx)
	}
	return nil
}

func (d *Daemon) UploadCertificate(ctx context.Context, certificate, privateKey string) error {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg := d.desiredConfig()
	pair, err := tls.X509KeyPair([]byte(certificate), []byte(privateKey))
	if err != nil || len(pair.Certificate) == 0 {
		return control.ErrInvalid
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname(cfg.Server.Hostname) != nil {
		return control.ErrInvalid
	}
	root := filepath.Join(cfg.Storage.StateDir, "certs")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(root, "pair-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(dir)
		}
	}()
	for name, content := range map[string]string{cfg.Server.Hostname + ".crt": certificate, cfg.Server.Hostname + ".key": privateKey} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			return err
		}
	}
	cfg.Server.DERP.TLSMode = "passthrough"
	cfg.Server.DERP.CertMode = "manual"
	cfg.Server.DERP.CertDir = dir
	if err := d.saveLocalConfig(cfg); err != nil {
		return err
	}
	committed = true
	return nil
}
