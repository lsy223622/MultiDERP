package derper

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestWaitReadyAllowsFirstACMECertificate(t *testing.T) {
	issued := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/derp/probe" {
			t.Errorf("unexpected readiness path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	certificate := server.TLS.Certificates[0]
	server.TLS.Certificates = nil
	server.TLS.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		<-issued
		return &certificate, nil
	}
	defer server.Close()
	timer := time.AfterFunc(13*time.Second, func() { close(issued) })
	defer func() {
		if timer.Stop() {
			close(issued)
		}
	}()
	cfg := testServer("passthrough")
	cfg.DERP.CertMode = "letsencrypt"
	cfg.DERP.Listen = strings.TrimPrefix(server.URL, "https://")
	process := &Process{command: exec.Command(os.Args[0]), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := process.WaitReady(ctx, cfg); err != nil {
		t.Fatalf("first ACME issuance interrupted before readiness: %v", err)
	}
}

func TestProcessHelper(t *testing.T) {
	if os.Getenv("UNIDERP_PROCESS_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestStopCommandReturnsNilAfterForcedKill(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestProcessHelper")
	command.Env = append(os.Environ(), "UNIDERP_PROCESS_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stopCommand(ctx, command, done); err != nil {
		t.Fatalf("stopCommand() error after forced kill = %v, want nil", err)
	}
}
