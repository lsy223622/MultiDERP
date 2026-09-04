package derper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsy223622/MultiDERP/internal/config"
)

func testServer(tlsMode string) config.ServerConfig {
	certMode := "none"
	certDir := ""
	if tlsMode == "passthrough" {
		certMode = "manual"
		certDir = "/run/secrets/derp-certs"
	}
	return config.ServerConfig{
		Hostname: "derp.example.com",
		DERP: config.DERPConfig{
			Listen:     ":3377",
			STUNListen: ":3478",
			TLSMode:    tlsMode,
			CertMode:   certMode,
			CertDir:    certDir,
		},
	}
}

func TestBuildArgsPinsAdmissionAndDisablesMesh(t *testing.T) {
	args, err := BuildArgs(testServer("external"), "127.0.0.1:3340", "/data/derper/derper.key")
	if err != nil {
		t.Fatalf("BuildArgs() error = %v", err)
	}
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"-hostname=derp.example.com",
		"-a=:3377",
		"-stun-port=3478",
		"-http-port=-1",
		"-verify-clients=false",
		"-verify-client-url=http://127.0.0.1:3340/admit",
		"-verify-client-url-fail-open=false",
		"-mesh-psk-file=",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args do not contain %q: %v", want, args)
		}
	}
	for _, forbidden := range []string{"-mesh-with", "-secrets-url", "-rate-config", "-accept-connection-limit", "-accept-connection-burst"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("args contain forbidden flag %q: %v", forbidden, args)
		}
	}
	if strings.Contains(joined, "-certmode=") || strings.Contains(joined, "-certdir=") {
		t.Fatalf("external args unexpectedly contain certificate flags: %v", args)
	}
}

func TestBuildArgsPassthroughUsesConfiguredCertificateFlags(t *testing.T) {
	args, err := BuildArgs(testServer("passthrough"), "127.0.0.1:3340", "/data/derper/derper.key")
	if err != nil {
		t.Fatalf("BuildArgs() error = %v", err)
	}
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "-certmode=manual") || !strings.Contains(joined, "-certdir=/run/secrets/derp-certs") {
		t.Fatalf("passthrough args lack certificate settings: %v", args)
	}
	if !strings.Contains(joined, "-http-port=-1") {
		t.Fatalf("manual TLS unexpectedly enables ACME HTTP listener: %v", args)
	}
}

func TestBuildArgsLetsEncryptEnablesHTTPChallenge(t *testing.T) {
	server := testServer("passthrough")
	server.DERP.Listen = ":443"
	server.DERP.CertMode = "letsencrypt"
	args, err := BuildArgs(server, "127.0.0.1:3340", "/data/derper/derper.key")
	if err != nil {
		t.Fatalf("BuildArgs() error = %v", err)
	}
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "-certmode=letsencrypt") || !strings.Contains(joined, "-http-port=80") {
		t.Fatalf("letsencrypt args lack ACME HTTP challenge settings: %v", args)
	}
}

func TestBuildArgsRejectsGCPCertificateMode(t *testing.T) {
	server := testServer("passthrough")
	server.DERP.CertMode = "gcp"
	if _, err := BuildArgs(server, "127.0.0.1:3340", "/data/derper/derper.key"); err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "gcp") {
		t.Fatalf("BuildArgs() error = %v, want clear gcp unsupported error", err)
	}
}

func TestBuildArgsRejectsNonLoopbackAdmission(t *testing.T) {
	for _, address := range []string{":3340", "0.0.0.0:3340", "127.0.0.1:0", "127.0.0.1:not-a-port"} {
		_, err := BuildArgs(testServer("external"), address, "/data/derper/derper.key")
		if err == nil {
			t.Errorf("BuildArgs(%q) succeeded for invalid admission address", address)
		}
	}
}

func TestEnsureKeyIsStableAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "derper.key")
	if err := EnsureKey(path); err != nil {
		t.Fatalf("first EnsureKey() error = %v", err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated key: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("generated key is empty")
	}
	if err := EnsureKey(path); err != nil {
		t.Fatalf("second EnsureKey() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stable key: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("EnsureKey() replaced an existing key")
	}
}

func TestEnsureKeyRejectsInvalidExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "derper.key")
	if err := os.WriteFile(path, []byte(`{"PrivateKey":""}`), 0o600); err != nil {
		t.Fatalf("write invalid key: %v", err)
	}
	if err := EnsureKey(path); err == nil {
		t.Fatal("EnsureKey() accepted a zero private key")
	}
}
