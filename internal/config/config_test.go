package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
			if result.Config.Version != want.Version || result.Config.Server != want.Server || result.Config.Storage != want.Storage || result.Config.Logging != want.Logging || len(result.Config.Tailnets) != 0 {
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
	_, err := Parse([]byte("version: 1\nserver:\n  derp:\n    listen: ':3377'\n    listen: ':3378'\n"))
	if err == nil || !strings.Contains(err.Error(), "duplicate YAML mapping key") {
		t.Fatalf("Parse() error = %v, want duplicate-key error", err)
	}
}

func TestParseUnknownFieldsWarnWithPaths(t *testing.T) {
	result, err := Parse([]byte(`version: 1
server:
  hostname: derp.example.com
  future_server_option: true
tailnets:
  - name: alice
    auth:
      type: web
      future_auth_option: true
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []string{
		"unknown config field ignored: server.future_server_option",
		"unknown config field ignored: tailnets[0].auth.future_auth_option",
	}
	if strings.Join(result.Warnings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("warnings = %#v, want %#v", result.Warnings, want)
	}
}

func TestParseRejectsUnsupportedFieldInsideUnknownMapping(t *testing.T) {
	_, err := Parse([]byte(`version: 1
future:
  nested:
    control_url: https://control.example.invalid
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported field") || !strings.Contains(err.Error(), "future.nested.control_url") {
		t.Fatalf("Parse() error = %v, want unsupported nested control_url error", err)
	}
}

func TestParseRejectsYAMLMergeKeys(t *testing.T) {
	_, err := Parse([]byte("version: 1\nbase: &base\n  server: {}\n<<: *base\n"))
	if err == nil || !strings.Contains(err.Error(), "YAML merge keys are not supported") {
		t.Fatalf("Parse() error = %v, want merge-key error", err)
	}
}

func TestValidateAuthenticationMatrix(t *testing.T) {
	base := Default()
	base.Server.Hostname = "derp.example.com"
	tests := map[string]configAuthTest{
		"web with secret": {
			cfg:   tailnet("web", "secret", ""),
			match: "web authentication cannot specify a secret file",
		},
		"oauth without secret": {
			cfg:   tailnet("oauth", "", ""),
			match: "client_secret_file is required",
		},
		"auth key without secret": {
			cfg:   tailnet("auth_key", "", ""),
			match: "auth_key_file is required",
		},
		"both secret files": {
			cfg: func() TailnetConfig {
				item := tailnet("oauth", "client", "")
				item.Auth.AuthKeyFile = "key"
				return item
			}(),
			match: "oauth authentication cannot specify auth_key_file",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := base.Clone()
			cfg.Tailnets = []TailnetConfig{tt.cfg}
			cfg.Normalize()
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.match) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.match)
			}
		})
	}
}

func TestValidateOAuthRequiresAdvertiseTags(t *testing.T) {
	cfg := Default()
	cfg.Server.Hostname = "derp.example.com"
	cfg.Tailnets = []TailnetConfig{tailnet("oauth", "client-secret", "")}
	cfg.Normalize()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "oauth authkeys require --advertise-tags") {
		t.Fatalf("Validate() error = %v, want OAuth advertise-tags error", err)
	}
}

func TestNormalizeAndValidateAdvertiseTags(t *testing.T) {
	cfg := Default()
	cfg.Server.Hostname = "derp.example.com"
	item := tailnet("oauth", "client-secret", "")
	item.Auth.Tags = []string{" tag:one ", "tag:two"}
	cfg.Tailnets = []TailnetConfig{item}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if want := []string{"tag:one", "tag:two"}; !reflect.DeepEqual(cfg.Tailnets[0].Auth.Tags, want) {
		t.Fatalf("normalized tags = %#v, want %#v", cfg.Tailnets[0].Auth.Tags, want)
	}
}

func TestParseLoadsOAuthAdvertiseTags(t *testing.T) {
	result, err := Parse([]byte(`version: 1
server:
  hostname: derp.example.com
tailnets:
  - name: company
    auth:
      type: oauth
      client_secret_file: /run/secrets/company-oauth
      tags:
        - tag:one
        - tag:two
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if want := []string{"tag:one", "tag:two"}; !reflect.DeepEqual(result.Config.Tailnets[0].Auth.Tags, want) {
		t.Fatalf("parsed OAuth tags = %#v, want %#v", result.Config.Tailnets[0].Auth.Tags, want)
	}
}

func TestValidateRejectsInvalidOrDuplicateAdvertiseTags(t *testing.T) {
	for name, tags := range map[string][]string{
		"missing prefix":    {"verifier"},
		"invalid character": {"tag:verifier.example"},
		"empty":             {"   "},
		"duplicate":         {"tag:verifier", " tag:verifier "},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Server.Hostname = "derp.example.com"
			item := tailnet("auth_key", "", "key")
			item.Auth.Tags = tags
			cfg.Tailnets = []TailnetConfig{item}
			cfg.Normalize()
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() accepted invalid advertise tags")
			}
		})
	}
}

func TestValidateDoesNotRequireTagsForWeb(t *testing.T) {
	cfg := Default()
	cfg.Server.Hostname = "derp.example.com"
	cfg.Tailnets = []TailnetConfig{tailnet("web", "", "")}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want web config without tags to be valid", err)
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

type configAuthTest struct {
	cfg   TailnetConfig
	match string
}

func tailnet(authType, clientSecret, authKey string) TailnetConfig {
	return TailnetConfig{
		Name: "alice",
		Auth: AuthConfig{
			Type:             authType,
			ClientSecretFile: clientSecret,
			AuthKeyFile:      authKey,
		},
	}
}

func TestValidateReloadAllowsRestartOnlyAndRejectsIdentityChanges(t *testing.T) {
	old := Default()
	old.Server.Hostname = "derp.example.com"
	old.Tailnets = []TailnetConfig{tailnet("web", "", "")}
	old.Normalize()
	if err := old.Validate(); err != nil {
		t.Fatalf("old config is invalid: %v", err)
	}

	restartOnly := old.Clone()
	restartOnly.Server.DERP.Listen = ":3378"
	if err := ValidateReload(old, restartOnly); err != nil {
		t.Fatalf("ValidateReload() restart-only error = %v", err)
	}
	if !RestartOnlyChanged(old, restartOnly) {
		t.Fatal("RestartOnlyChanged() = false for DERP listener change")
	}

	identityChange := old.Clone()
	identityChange.Tailnets[0].Hostname = "another.example.com"
	if err := ValidateReload(old, identityChange); err == nil || !strings.Contains(err.Error(), "identity/auth/hostname changed") {
		t.Fatalf("ValidateReload() error = %v, want identity-change error", err)
	}

	nameCaseChange := old.Clone()
	nameCaseChange.Tailnets[0].Name = "Alice"
	if err := ValidateReload(old, nameCaseChange); err == nil || !strings.Contains(err.Error(), "identity/auth/hostname changed") {
		t.Fatalf("ValidateReload() case-only name error = %v, want identity-change error", err)
	}

	hot := old.Clone()
	hot.Tailnets[0].Required = true
	hot.Tailnets = append(hot.Tailnets, tailnet("web", "", ""))
	hot.Tailnets[1].Name = "bob"
	if err := ValidateReload(old, hot); err != nil {
		t.Fatalf("ValidateReload() hot change error = %v", err)
	}

	removed := old.Clone()
	removed.Tailnets = nil
	if err := ValidateReload(old, removed); err == nil || !strings.Contains(err.Error(), "use tailnet remove") {
		t.Fatalf("ValidateReload() removal error = %v, want explicit remove guidance", err)
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

func TestLoadFileMissingIsExplicitError(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadFile() error = %v, want wrapped not-exist error", err)
	}
}

func TestIsWithin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if !IsWithin(root, filepath.Join(root, "child")) {
		t.Fatal("IsWithin() rejected child path")
	}
	if IsWithin(root, filepath.Join(root, "..", "outside")) {
		t.Fatal("IsWithin() accepted escaped path")
	}
}
