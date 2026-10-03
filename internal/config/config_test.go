package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEmptyDocumentsUseDefaults(t *testing.T) {
	inputs := []string{"", "# intentionally empty\n", "null\n", "{}\n"}
	for _, input := range inputs {
		t.Run(strings.ReplaceAll(input, "\n", "\\n"), func(t *testing.T) {
			result, err := Parse([]byte(input))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			want := Default()
			if result.Config.Version != want.Version || result.Config.Server != want.Server || result.Config.Storage != want.Storage || result.Config.Logging != want.Logging {
				t.Fatalf("Parse() config = %#v, want defaults %#v", result.Config, want)
			}
		})
	}
}

func TestParseRequiresExplicitVersion(t *testing.T) {
	for name, input := range map[string]string{
		"missing": "server: {}\n",
		"null":    "version: null\n",
		"zero":    "version: 0\n",
		"string":  "version: one\n",
		"quoted":  "version: \"1\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("Parse() succeeded for invalid version")
			}
		})
	}
}

func TestParseRejectsDuplicateKeys(t *testing.T) {
	_, err := Parse([]byte("version: 2\nserver:\n  derp:\n    listen: ':3377'\n    listen: ':3378'\n"))
	if err == nil || !strings.Contains(err.Error(), "duplicate YAML mapping key") {
		t.Fatalf("Parse() error = %v, want duplicate-key error", err)
	}
}

func TestParseUnknownFieldsWarnWithPaths(t *testing.T) {
	result, err := Parse([]byte(`version: 2
server:
  hostname: derp.example.com
  future_server_option: true
node:
  future_node_option: true
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"unknown config field ignored: node.future_node_option", "unknown config field ignored: server.future_server_option"}
	if strings.Join(result.Warnings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("warnings = %#v, want %#v", result.Warnings, want)
	}
}

func TestParseRejectsUnsupportedFieldInsideUnknownMapping(t *testing.T) {
	_, err := Parse([]byte(`version: 2
future:
  nested:
    control_url: https://control.example.invalid
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported field") || !strings.Contains(err.Error(), "future.nested.control_url") {
		t.Fatalf("Parse() error = %v, want unsupported nested control_url error", err)
	}
}

func TestParseRejectsYAMLMergeKeys(t *testing.T) {
	_, err := Parse([]byte("version: 2\nbase: &base\n  server: {}\n<<: *base\n"))
	if err == nil || !strings.Contains(err.Error(), "YAML merge keys are not supported") {
		t.Fatalf("Parse() error = %v, want merge-key error", err)
	}
}

func TestValidateRejectsInvalidDERPListenerCombinations(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"external on 443": func(cfg *Config) {
			cfg.Server.DERP.Listen = ":443"
		},
		"passthrough letsencrypt on non-443": func(cfg *Config) {
			cfg.Server.DERP.Listen = ":3377"
			cfg.Server.DERP.TLSMode = "passthrough"
			cfg.Server.DERP.CertMode = "letsencrypt"
			cfg.Server.DERP.CertDir = "/certs"
		},
		"passthrough gcp on non-443": func(cfg *Config) {
			cfg.Server.DERP.Listen = ":3377"
			cfg.Server.DERP.TLSMode = "passthrough"
			cfg.Server.DERP.CertMode = "gcp"
			cfg.Server.DERP.CertDir = "/certs"
		},
		"different explicit hosts": func(cfg *Config) {
			cfg.Server.DERP.Listen = "127.0.0.1:3377"
			cfg.Server.DERP.STUNListen = "[::1]:3478"
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() accepted invalid DERP listener combination")
			}
		})
	}
}

func TestValidateTLSCertificateModes(t *testing.T) {
	external := Default()
	if external.Server.DERP.CertMode != "none" {
		t.Fatalf("Default() cert mode = %q, want none", external.Server.DERP.CertMode)
	}
	if err := external.Validate(); err != nil {
		t.Fatalf("external none config is invalid: %v", err)
	}

	manual := Default()
	manual.Server.DERP.Listen = ":8443"
	manual.Server.DERP.TLSMode = "passthrough"
	manual.Server.DERP.CertMode = "manual"
	manual.Server.DERP.CertDir = "/certs"
	if err := manual.Validate(); err != nil {
		t.Fatalf("manual TLS on non-443 config is invalid: %v", err)
	}

	letsencrypt := manual.Clone()
	letsencrypt.Server.DERP.Listen = ":443"
	letsencrypt.Server.DERP.CertMode = "letsencrypt"
	if err := letsencrypt.Validate(); err != nil {
		t.Fatalf("letsencrypt TLS on 443 config is invalid: %v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"gcp external": func(cfg *Config) {
			cfg.Server.DERP.Listen = ":443"
			cfg.Server.DERP.CertMode = "gcp"
		},
		"gcp passthrough": func(cfg *Config) {
			cfg.Server.DERP.TLSMode = "passthrough"
			cfg.Server.DERP.CertMode = "gcp"
			cfg.Server.DERP.CertDir = "/certs"
		},
		"none passthrough": func(cfg *Config) {
			cfg.Server.DERP.TLSMode = "passthrough"
			cfg.Server.DERP.CertMode = "none"
			cfg.Server.DERP.CertDir = "/certs"
		},
		"manual external": func(cfg *Config) {
			cfg.Server.DERP.CertMode = "manual"
			cfg.Server.DERP.CertDir = "/certs"
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() accepted unsupported TLS certificate configuration")
			}
			if strings.Contains(name, "gcp") && (!strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "gcp")) {
				t.Fatalf("Validate() error = %v, want clear gcp unsupported error", err)
			}
		})
	}
}

func TestWriteAtomicNormalizesAndCanBeReloaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := WriteAtomic(path, Config{Version: CurrentVersion}); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	result, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if result.Config.Server.DERP.Listen != DefaultDERPListen || result.Config.Server.DERP.CertMode != "none" || result.Config.Storage.StateDir != DefaultStateDir {
		t.Fatalf("reloaded config did not receive defaults: %#v", result.Config)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat config: %v", err)
	} else if info.IsDir() {
		t.Fatal("config path is a directory")
	}
}

func TestCreateFileIfMissingUsesExampleAndPreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	example := ExampleYAML()
	created, err := CreateFileIfMissing(path, example)
	if err != nil {
		t.Fatalf("CreateFileIfMissing() error = %v", err)
	}
	if !created {
		t.Fatal("CreateFileIfMissing() reported that the new file was not created")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read created config: %v", err)
	}
	if !bytes.Equal(data, example) {
		t.Fatalf("created config differs from example:\n%s", data)
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() created config error = %v", err)
	}
	if parsed.Config.Server.DERP.Listen != DefaultDERPListen || parsed.Config.Storage.StateDir != DefaultStateDir || parsed.Config.Logging.Level != DefaultLoggingLevel {
		t.Fatalf("created config did not contain expected defaults: %#v", parsed.Config)
	}

	replacement := []byte("version: 2\n")
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatalf("write existing config: %v", err)
	}
	created, err = CreateFileIfMissing(path, []byte("must not replace"))
	if err != nil {
		t.Fatalf("CreateFileIfMissing() existing-file error = %v", err)
	}
	if created {
		t.Fatal("CreateFileIfMissing() reported creation for an existing file")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read existing config: %v", err)
	}
	if !bytes.Equal(data, replacement) {
		t.Fatalf("existing config was replaced: %q", data)
	}
}

func TestLoadFileMissingIsExplicitError(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadFile() error = %v, want wrapped not-exist error", err)
	}
}
