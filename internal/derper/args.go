package derper

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"multiderp/internal/config"
)

func BuildArgs(server config.ServerConfig, admissionAddress, keyPath string) ([]string, error) {
	if strings.TrimSpace(server.Hostname) == "" {
		return nil, errors.New("server.hostname is required before starting derper")
	}
	if strings.TrimSpace(keyPath) == "" {
		return nil, errors.New("derper key path is required")
	}
	if err := ValidateAdmissionAddress(admissionAddress); err != nil {
		return nil, err
	}
	if err := configAddress(server.DERP.Listen, "server.derp.listen"); err != nil {
		return nil, err
	}
	if err := validateTLSConfig(server); err != nil {
		return nil, err
	}
	stunHost, stunPort, err := net.SplitHostPort(server.DERP.STUNListen)
	if err != nil {
		return nil, fmt.Errorf("server.derp.stun_listen: %w", err)
	}
	listenHost, _, _ := net.SplitHostPort(server.DERP.Listen)
	if stunHost != "" && listenHost != "" && stunHost != listenHost {
		return nil, errors.New("server.derp.stun_listen must use the same host as server.derp.listen when both are explicit")
	}
	if stunPortNumber, err := strconv.ParseUint(stunPort, 10, 16); err != nil || stunPortNumber == 0 {
		return nil, errors.New("server.derp.stun_listen: port must be a non-zero numeric value")
	}
	args := []string{
		"-hostname=" + server.Hostname,
		"-a=" + server.DERP.Listen,
		"-stun-port=" + stunPort,
		"-stun=true",
		"-derp=true",
		"-c=" + keyPath,
		"-http-port=" + acmeHTTPPort(server),
		"-verify-clients=false",
		"-verify-client-url=http://" + net.JoinHostPort(admissionHost(admissionAddress), admissionPort(admissionAddress)) + "/admit",
		"-verify-client-url-fail-open=false",
		"-mesh-psk-file=",
	}
	if server.DERP.TLSMode == "passthrough" {
		args = append(args,
			"-certmode="+server.DERP.CertMode,
			"-certdir="+server.DERP.CertDir,
		)
	}
	return args, nil
}

func validateTLSConfig(server config.ServerConfig) error {
	switch server.DERP.TLSMode {
	case "external":
		_, port, _ := net.SplitHostPort(server.DERP.Listen)
		portNumber, _ := strconv.ParseUint(port, 10, 16)
		if portNumber == 443 {
			return errors.New("server.derp.listen: tls_mode external must use a non-443 internal port")
		}
		if server.DERP.CertMode != "" && server.DERP.CertMode != "none" {
			if server.DERP.CertMode == "gcp" {
				return errors.New("server.derp.cert_mode: unsupported value \"gcp\" in V1; supported values are none, letsencrypt, or manual")
			}
			return fmt.Errorf("server.derp.cert_mode %q requires tls_mode: passthrough; tls_mode: external uses cert_mode: none", server.DERP.CertMode)
		}
		if server.DERP.CertDir != "" {
			return errors.New("server.derp.cert_dir is only valid with tls_mode: passthrough")
		}
	case "passthrough":
		switch server.DERP.CertMode {
		case "manual":
		case "letsencrypt":
			_, port, _ := net.SplitHostPort(server.DERP.Listen)
			if port != "443" {
				return fmt.Errorf("server.derp.listen: passthrough cert_mode %q must use port 443; use cert_mode: manual for a non-443 TLS backend", server.DERP.CertMode)
			}
		case "gcp":
			return errors.New("server.derp.cert_mode: unsupported value \"gcp\" in V1; supported values are none, letsencrypt, or manual")
		default:
			return errors.New("server.derp.cert_mode must be manual or letsencrypt with tls_mode: passthrough")
		}
		if strings.TrimSpace(server.DERP.CertDir) == "" {
			return errors.New("server.derp.cert_dir is required with tls_mode: passthrough")
		}
	default:
		return fmt.Errorf("server.derp.tls_mode: unsupported value %q", server.DERP.TLSMode)
	}
	return nil
}

func acmeHTTPPort(server config.ServerConfig) string {
	if server.DERP.TLSMode == "passthrough" && server.DERP.CertMode == "letsencrypt" {
		return "80"
	}
	return "-1"
}

func ValidateAdmissionAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("admission address: invalid listen address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("admission address: host must be a loopback IP address, got %q", host)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return fmt.Errorf("admission address: port must be a non-zero numeric value, got %q", port)
	}
	return nil
}

func admissionHost(address string) string {
	host, _, _ := net.SplitHostPort(address)
	return host
}

func admissionPort(address string) string {
	_, port, _ := net.SplitHostPort(address)
	return port
}

func configAddress(address, field string) error {
	if strings.TrimSpace(address) == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if _, port, err := net.SplitHostPort(address); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	} else if portNumber, err := strconv.ParseUint(port, 10, 16); err != nil || portNumber == 0 {
		return fmt.Errorf("%s: port must be a non-zero numeric value", field)
	}
	return nil
}
