package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"github.com/lsy223622/UniDERP/v2/internal/config"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveSettingsInvalidHostnameReturnsInputError(t *testing.T) {
	d := localTestDaemon(t, true)
	s, _ := d.Status(t.Context())
	settings := s.Saved
	settings.Role, settings.Hostname = "controller", "https://derp.example.com/"
	err := d.SaveSettings(t.Context(), settings)
	if fmt.Sprintf("%T", err) != "*control.InputError" {
		t.Fatalf("want field input error, got %T", err)
	}
}

func TestManualTLSPreflightKeepsRunningRelayOnMissingCertificate(t *testing.T) {
	d := localTestDaemon(t, true)
	s, _ := d.Status(t.Context())
	settings := s.Saved
	settings.Role, settings.Hostname = "controller", "relay.example.com"
	settings.TLSMode, settings.CertMode = "external", "none"
	d.derper = &daemonFakeProcess{}
	if err := d.SaveSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	settings.TLSMode, settings.CertMode = "passthrough", "manual"
	if err := d.SaveSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplySettings(t.Context()); err == nil {
		t.Fatal("missing certificate applied")
	}
	if !d.derper.Running() || d.activeConfig().Server.DERP.TLSMode != "external" {
		t.Fatal("certificate preflight stopped the active relay")
	}
	after, _ := d.Status(t.Context())
	if !after.PendingApply {
		t.Fatal("saved repair configuration lost")
	}
}

func localTestDaemon(t *testing.T, setup bool) *Daemon {
	t.Helper()
	dir := shortTempDir(t)
	cfg := config.Default()
	cfg.SetupRequired = setup
	cfg.Storage.StateDir = dir
	cfg.Node.StateDir = filepath.Join(dir, "node")
	cfg.Controller.Database = filepath.Join(dir, "controller.sqlite")
	cfg.Controller.KeyFile = filepath.Join(dir, "controller.key")
	cfg.Controller.Listen = freeLoopbackAddress(t)
	cfg.Server.Management.Listen = freeLoopbackAddress(t)
	cfg.Server.Admin.Socket = filepath.Join(dir, "admin.sock")
	cfg.Server.Health.Listen = freeLoopbackAddress(t)
	path := filepath.Join(dir, "config.yaml")
	if err := config.WriteAtomic(path, cfg); err != nil {
		t.Fatal(err)
	}
	d := New(context.Background(), Options{ConfigPath: path, Logger: log.New(io.Discard, "", 0)})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Shutdown() })
	return d
}

func TestManualCertificateUploadValidatesPairAndHostname(t *testing.T) {
	d := localTestDaemon(t, true)
	s, _ := d.Status(t.Context())
	local := s.Saved
	local.Role = "controller"
	local.Hostname = "relay.example.com"
	if err := d.SaveSettings(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{local.Hostname}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := x509.MarshalPKCS8PrivateKey(priv)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	if err := d.UploadCertificate(t.Context(), certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	dir := d.desiredConfig().Server.DERP.CertDir
	if dir == "" || d.activeConfig().Server.DERP.CertDir == dir {
		t.Fatal("upload applied without confirmation")
	}
	if err := d.UploadCertificate(t.Context(), certPEM, "invalid key"); err == nil {
		t.Fatal("invalid pair accepted")
	}
	if d.desiredConfig().Server.DERP.CertDir != dir {
		t.Fatal("invalid upload changed saved certificate")
	}
	local.Hostname = "other.example.com"
	if err := d.SaveSettings(t.Context(), local); err != nil {
		t.Fatal(err)
	}
	if err := d.UploadCertificate(t.Context(), certPEM, keyPEM); err == nil {
		t.Fatal("wrong hostname accepted")
	}
}

func TestLocalSettingsSaveApplyAndRolePersistence(t *testing.T) {
	d := localTestDaemon(t, true)
	if d.Role() != "setup" || d.derper.Running() {
		t.Fatal("bootstrap role")
	}
	if _, err := os.Stat(d.current.Controller.KeyFile); !os.IsNotExist(err) {
		t.Fatal("bootstrap initialized controller identity", err)
	}
	admin, err := d.controllerStore.InitializeAdmin(t.Context(), "localadmin", "long-password-123")
	if err != nil {
		t.Fatal(err)
	}
	status, err := d.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	saved := status.Saved
	saved.Role = "member"
	saved.TLSMode, saved.CertMode = "external", "none"
	saved.MaxBudgetBPS = 80000000
	if err := d.SaveSettings(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	if d.Role() != "setup" {
		t.Fatal("save changed running role")
	}
	if err := d.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := d.Status(t.Context())
	if after.Role != "member" || after.PendingApply || after.Active != after.Saved || after.Saved.MaxBudgetBPS != 80000000 {
		t.Fatal(after)
	}
	saved.Role = "controller"
	if err := d.SaveSettings(t.Context(), saved); err == nil {
		t.Fatal("configured role changed")
	}
	cfg, err := config.LoadFile(d.configPath)
	if err != nil || cfg.Config.SetupRequired || cfg.Config.Controller.Enabled {
		t.Fatal(cfg, err)
	}
	users, err := d.controllerStore.Authenticate(t.Context(), "invalid")
	_ = users
	if err == nil {
		t.Fatal("invalid session")
	}
	if admin.Role != "admin" {
		t.Fatal(admin)
	}
	r, err := http.Get("http://" + d.managementListener.Addr().String() + "/manage/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
}

func TestApplyFailureKeepsManagementAndSavedState(t *testing.T) {
	d := localTestDaemon(t, true)
	s, _ := d.Status(t.Context())
	settings := s.Saved
	settings.Role = "controller"
	settings.Hostname = "relay.example.com"
	if err := d.SaveSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	// Missing child binary must not tear down the only repair interface.
	d.derperBinary = "nonexistent-uniderp-test-child"
	if err := d.ApplySettings(t.Context()); err == nil {
		t.Fatal("failed child launch reported success")
	}
	after, _ := d.Status(t.Context())
	if !after.PendingApply || after.ApplyError == "" || d.Role() != "setup" {
		t.Fatal(after)
	}
	r, err := http.Get("http://" + d.managementListener.Addr().String() + "/api/v1/setup")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	d.derper = &daemonFakeProcess{}
	if err := d.ApplySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ = d.Status(t.Context())
	if after.PendingApply || after.Role != "controller" || after.Active != after.Saved {
		t.Fatal(after)
	}
}
