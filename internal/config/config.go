package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	configexample "github.com/lsy223622/UniDERP/v2"
	"gopkg.in/yaml.v3"
)

const CurrentVersion = 2

const (
	DefaultDERPListen   = ":3377"
	DefaultSTUNListen   = ":3478"
	DefaultAdminSocket  = "/run/uniderp/admin.sock"
	DefaultHealthListen = "127.0.0.1:9090"
	DefaultStateDir     = "/data"
	DefaultLoggingLevel = "info"
	DefaultTLSMode      = "external"
	DefaultConfigPath   = "/data/config.yaml"
)

type Config struct {
	SetupRequired bool              `yaml:"setup_required,omitempty"`
	Controller    *ControllerConfig `yaml:"controller"`
	Node          NodeConfig        `yaml:"node"`
	Version       int               `yaml:"version"`
	Server        ServerConfig      `yaml:"server"`
	Storage       StorageConfig     `yaml:"storage"`
	Logging       LoggingConfig     `yaml:"logging"`
}

type ControllerConfig struct {
	Enabled          bool     `yaml:"enabled"`
	Listen           string   `yaml:"listen"`
	Database         string   `yaml:"database"`
	KeyFile          string   `yaml:"key_file"`
	AllowedNodeCIDRs []string `yaml:"allowed_node_cidrs,omitempty"`
}

type NodeConfig struct {
	DERPPort      int    `yaml:"derp_port,omitempty"`
	STUNPort      int    `yaml:"stun_port,omitempty"`
	ControllerURL string `yaml:"controller_url,omitempty"`
	StateDir      string `yaml:"state_dir"`
	MaxBudgetBPS  uint64 `yaml:"max_budget_bps,omitempty"`
}

type ServerConfig struct {
	Management ManagementConfig `yaml:"management,omitempty"`
	Hostname   string           `yaml:"hostname"`
	DERP       DERPConfig       `yaml:"derp"`
	Admin      AdminConfig      `yaml:"admin"`
	Health     HealthConfig     `yaml:"health"`
}

type ManagementConfig struct {
	Listen string `yaml:"listen,omitempty"`
}

type DERPConfig struct {
	Listen     string `yaml:"listen"`
	STUNListen string `yaml:"stun_listen"`
	TLSMode    string `yaml:"tls_mode"`
	CertMode   string `yaml:"cert_mode"`
	CertDir    string `yaml:"cert_dir"`
}

type AdminConfig struct {
	Socket string `yaml:"socket"`
}

type HealthConfig struct {
	Listen string `yaml:"listen"`
}

type StorageConfig struct {
	StateDir string `yaml:"state_dir"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}

type ParseResult struct {
	Config   Config
	Warnings []string
}

func Default() Config {
	c := Config{Version: CurrentVersion}
	c.Normalize()
	return c
}

func (c *Config) Normalize() {
	if c.Version == 0 {
		c.Version = CurrentVersion
	}
	if c.Server.DERP.Listen == "" {
		c.Server.DERP.Listen = DefaultDERPListen
	}
	if c.Server.DERP.STUNListen == "" {
		c.Server.DERP.STUNListen = DefaultSTUNListen
	}
	if c.Server.DERP.TLSMode == "" {
		c.Server.DERP.TLSMode = DefaultTLSMode
	}
	if c.Server.DERP.CertMode == "" && c.Server.DERP.TLSMode == "external" {
		c.Server.DERP.CertMode = "none"
	}
	if c.Server.Admin.Socket == "" {
		c.Server.Admin.Socket = DefaultAdminSocket
	}
	if c.Server.Health.Listen == "" {
		c.Server.Health.Listen = DefaultHealthListen
	}
	if c.Storage.StateDir == "" {
		c.Storage.StateDir = DefaultStateDir
	}
	if c.Controller == nil {
		c.Controller = &ControllerConfig{Enabled: true}
	}
	if c.Controller.Enabled {
		if c.Controller.Listen == "" {
			c.Controller.Listen = "127.0.0.1:3341"
		}
		if c.Controller.Database == "" {
			c.Controller.Database = filepath.Join(c.Storage.StateDir, "controller.sqlite")
		}
		if c.Controller.KeyFile == "" {
			c.Controller.KeyFile = filepath.Join(c.Storage.StateDir, "controller.key")
		}
	}
	if c.Node.StateDir == "" {
		c.Node.StateDir = filepath.Join(c.Storage.StateDir, "node")
	}
	if c.Node.DERPPort == 0 {
		c.Node.DERPPort = 443
	}
	if c.Node.STUNPort == 0 {
		c.Node.STUNPort = 3478
	}
	if c.Logging.Level == "" {
		c.Logging.Level = DefaultLoggingLevel
	}
}

func (c Config) Clone() Config {
	clone := c
	if c.Controller != nil {
		controller := *c.Controller
		controller.AllowedNodeCIDRs = append([]string(nil), controller.AllowedNodeCIDRs...)
		clone.Controller = &controller
	}
	return clone
}

func Parse(data []byte) (ParseResult, error) {
	var root yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return ParseResult{Config: Default()}, nil
		}
		return ParseResult{}, fmt.Errorf("parse config YAML: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ParseResult{}, errors.New("config must contain exactly one YAML document")
		}
		return ParseResult{}, fmt.Errorf("parse trailing YAML document: %w", err)
	}

	if isEmptyDocument(&root) {
		return ParseResult{Config: Default()}, nil
	}
	document := &root
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		document = root.Content[0]
	}
	if document.Kind != yaml.MappingNode {
		return ParseResult{}, errors.New("config root must be a YAML mapping")
	}
	if err := rejectDuplicateKeys(document, ""); err != nil {
		return ParseResult{}, err
	}

	hasVersion := false
	for i := 0; i+1 < len(document.Content); i += 2 {
		if document.Content[i].Value == "version" {
			hasVersion = true
			break
		}
	}
	if !hasVersion {
		return ParseResult{}, errors.New("config version is required for non-empty YAML")
	}
	versionNode := mappingValue(document, "version")
	if versionNode == nil || versionNode.Kind != yaml.ScalarNode || versionNode.Tag != "!!int" {
		return ParseResult{}, errors.New("config version must be an explicit integer")
	}
	var version int
	if err := versionNode.Decode(&version); err != nil {
		return ParseResult{}, fmt.Errorf("config version must be an integer: %w", err)
	}
	if version != CurrentVersion {
		if version == 1 {
			return ParseResult{}, errors.New("config version 1 requires migration to version 2")
		}
		return ParseResult{}, fmt.Errorf("unsupported config version %d; expected %d", version, CurrentVersion)
	}
	if mappingValue(document, "tailnets") != nil {
		return ParseResult{}, errors.New("tailnets must be managed through the controller; migrate local configuration to version 2")
	}

	warnings := make([]string, 0)
	if err := collectUnknownFields(document, "", rootSchema, &warnings); err != nil {
		return ParseResult{}, err
	}

	var cfg Config
	if err := document.Decode(&cfg); err != nil {
		return ParseResult{}, fmt.Errorf("decode config: %w", err)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return ParseResult{}, err
	}
	sort.Strings(warnings)
	return ParseResult{Config: cfg, Warnings: warnings}, nil
}

func LoadFile(path string) (ParseResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ParseResult{}, fmt.Errorf("required config file %q does not exist: %w", path, os.ErrNotExist)
		}
		return ParseResult{}, fmt.Errorf("read config file %q: %w", path, err)
	}
	result, err := Parse(data)
	if err != nil {
		return ParseResult{}, fmt.Errorf("config file %q: %w", path, err)
	}
	return result, nil
}

func ExampleYAML() []byte {
	return configexample.Content()
}

func CreateFileIfMissing(path string, data []byte) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("create config parent directory %q: %w", dir, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("create config file %q: %w", path, err)
	}
	removeOnError := true
	defer func() {
		_ = file.Close()
		if removeOnError {
			_ = os.Remove(path)
		}
	}()

	if n, err := file.Write(data); err != nil {
		return false, fmt.Errorf("write config file %q: %w", path, err)
	} else if n != len(data) {
		return false, fmt.Errorf("write config file %q: %w", path, io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync config file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("close config file %q: %w", path, err)
	}
	removeOnError = false
	return true, nil
}

func (c Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("unsupported config version %d; expected %d", c.Version, CurrentVersion)
	}
	if c.Controller == nil {
		return errors.New("controller role is required")
	}
	if c.Controller.Enabled {
		if c.Node.ControllerURL != "" {
			return errors.New("controller cannot join another controller")
		}
		if strings.TrimSpace(c.Controller.Database) == "" || strings.TrimSpace(c.Controller.KeyFile) == "" {
			return errors.New("controller database and key_file are required")
		}
		if err := validateListenAddress(c.Controller.Listen, "controller.listen"); err != nil {
			return err
		}
		host, _, _ := net.SplitHostPort(c.Controller.Listen)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return errors.New("controller.listen must be loopback")
		}
		for _, cidr := range c.Controller.AllowedNodeCIDRs {
			if _, err := netip.ParsePrefix(cidr); err != nil {
				return errors.New("controller.allowed_node_cidrs contains an invalid range")
			}
		}
	} else {
		if c.Controller.Listen != "" || c.Controller.Database != "" || c.Controller.KeyFile != "" || len(c.Controller.AllowedNodeCIDRs) != 0 {
			return errors.New("member node cannot configure controller storage or settings")
		}
		if c.Node.ControllerURL != "" {
			u, err := url.Parse(c.Node.ControllerURL)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
				return errors.New("node.controller_url must be an HTTPS origin")
			}
		}
	}
	if c.Node.DERPPort < 1 || c.Node.DERPPort > 65535 || c.Node.STUNPort < 1 || c.Node.STUNPort > 65535 {
		return errors.New("node public ports must be between 1 and 65535")
	}
	if c.Server.Management.Listen != "" {
		if err := validateListenAddress(c.Server.Management.Listen, "server.management.listen"); err != nil {
			return err
		}
	}
	if strings.TrimSpace(c.Node.StateDir) == "" {
		return errors.New("node.state_dir is required")
	}
	if c.Node.MaxBudgetBPS > math.MaxInt64 || (c.Node.MaxBudgetBPS != 0 && c.Node.MaxBudgetBPS < 8) {
		return errors.New("node.max_budget_bps overflows the supported budget")
	}
	if c.Server.Hostname != "" {
		if err := ValidateHostname(c.Server.Hostname); err != nil {
			return fmt.Errorf("server.hostname: %w", err)
		}
	}
	if err := validateListenAddress(c.Server.DERP.Listen, "server.derp.listen"); err != nil {
		return err
	}
	if err := validateListenAddress(c.Server.DERP.STUNListen, "server.derp.stun_listen"); err != nil {
		return err
	}
	derpHost, _, _ := net.SplitHostPort(c.Server.DERP.Listen)
	stunHost, _, _ := net.SplitHostPort(c.Server.DERP.STUNListen)
	if derpHost != "" && stunHost != "" && derpHost != stunHost {
		return errors.New("server.derp.stun_listen must use the same host as server.derp.listen when both are explicit")
	}
	if c.Server.DERP.TLSMode != "external" && c.Server.DERP.TLSMode != "passthrough" {
		return fmt.Errorf("server.derp.tls_mode: unsupported value %q", c.Server.DERP.TLSMode)
	}
	switch c.Server.DERP.CertMode {
	case "", "none", "manual", "letsencrypt":
	case "gcp":
		return errors.New("server.derp.cert_mode: unsupported value \"gcp\"; supported values are none, letsencrypt, or manual")
	default:
		return fmt.Errorf("server.derp.cert_mode: unsupported value %q; expected none, letsencrypt, or manual", c.Server.DERP.CertMode)
	}
	if c.Server.DERP.TLSMode == "external" {
		_, port, _ := net.SplitHostPort(c.Server.DERP.Listen)
		portNumber, _ := strconv.ParseUint(port, 10, 16)
		if portNumber == 443 {
			return errors.New("server.derp.listen: tls_mode external must use a non-443 internal port")
		}
	}
	if c.Server.DERP.TLSMode == "external" {
		if c.Server.DERP.CertMode != "" && c.Server.DERP.CertMode != "none" {
			return fmt.Errorf("server.derp.cert_mode %q requires tls_mode: passthrough; tls_mode: external uses cert_mode: none", c.Server.DERP.CertMode)
		}
		if c.Server.DERP.CertDir != "" {
			return errors.New("server.derp.cert_dir is only valid with tls_mode: passthrough")
		}
	}
	if c.Server.DERP.TLSMode == "passthrough" {
		if c.Server.DERP.CertMode != "manual" && c.Server.DERP.CertMode != "letsencrypt" {
			return errors.New("server.derp.cert_mode must be manual or letsencrypt with tls_mode: passthrough")
		}
		if c.Server.DERP.CertDir == "" {
			return errors.New("server.derp.cert_dir is required with tls_mode: passthrough")
		}
		if c.Server.DERP.CertMode != "manual" {
			_, port, _ := net.SplitHostPort(c.Server.DERP.Listen)
			portNumber, _ := strconv.ParseUint(port, 10, 16)
			if portNumber != 443 {
				return fmt.Errorf("server.derp.listen: passthrough cert_mode %q must use port 443; use cert_mode: manual for a non-443 TLS backend", c.Server.DERP.CertMode)
			}
		}
	}
	if strings.TrimSpace(c.Server.Admin.Socket) == "" {
		return errors.New("server.admin.socket must not be empty")
	}
	if err := validateListenAddress(c.Server.Health.Listen, "server.health.listen"); err != nil {
		return err
	}
	if c.Logging.Level != "info" && c.Logging.Level != "warn" && c.Logging.Level != "error" && c.Logging.Level != "debug" {
		return fmt.Errorf("logging.level: unsupported value %q", c.Logging.Level)
	}
	if strings.TrimSpace(c.Storage.StateDir) == "" {
		return errors.New("storage paths must not be empty")
	}

	return nil
}

func RestartOnlyChanged(oldConfig, newConfig Config) bool {
	oldConfig.Normalize()
	newConfig.Normalize()
	return oldConfig.Server.Hostname != newConfig.Server.Hostname ||
		oldConfig.SetupRequired != newConfig.SetupRequired || oldConfig.Server.Management != newConfig.Server.Management ||
		!reflect.DeepEqual(oldConfig.Server.DERP, newConfig.Server.DERP) ||
		oldConfig.Server.Admin.Socket != newConfig.Server.Admin.Socket ||
		oldConfig.Server.Health.Listen != newConfig.Server.Health.Listen ||
		oldConfig.Storage != newConfig.Storage ||
		!reflect.DeepEqual(oldConfig.Controller, newConfig.Controller) || oldConfig.Node != newConfig.Node
}

func WriteAtomic(path string, cfg Config) error {
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return writeAtomicBytes(path, data, ".config.yaml.*.tmp")
}

func writeAtomicBytes(path string, data []byte, pattern string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomically replace file: %w", err)
	}
	// Some platforms do not expose directory fsync. The file and rename are
	// still durable to the extent supported by the host filesystem.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

var hostnameLabel = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func ValidateHostname(hostname string) error {
	if len(hostname) == 0 || len(hostname) > 253 || strings.ContainsAny(hostname, "/\\ \t\r\n") {
		return errors.New("must be a valid DNS hostname")
	}
	if net.ParseIP(hostname) != nil {
		return nil
	}
	for _, label := range strings.Split(hostname, ".") {
		if !hostnameLabel.MatchString(label) {
			return fmt.Errorf("invalid DNS label %q", label)
		}
	}
	return nil
}

func validateListenAddress(address, field string) error {
	if strings.TrimSpace(address) == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s: invalid listen address %q: %w", field, address, err)
	}
	if port == "" {
		return fmt.Errorf("%s: port is empty", field)
	}
	if host != "" && host != "0.0.0.0" && host != "::" && net.ParseIP(host) == nil {
		return fmt.Errorf("%s: host must be an IP address or empty, got %q", field, host)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return fmt.Errorf("%s: port must be a non-zero numeric value", field)
	}
	return nil
}

func isEmptyDocument(root *yaml.Node) bool {
	if root == nil || root.Kind == 0 {
		return true
	}
	n := root
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		n = root.Content[0]
	}
	if n.Kind == 0 {
		return true
	}
	if n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || strings.TrimSpace(n.Value) == "") {
		return true
	}
	return n.Kind == yaml.MappingNode && len(n.Content) == 0
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

type schemaNode struct {
	Fields map[string]*schemaNode
}

var (
	rootSchema = &schemaNode{Fields: map[string]*schemaNode{
		"setup_required": nil,
		"version":        nil,
		"server":         serverSchema,
		"storage":        storageSchema,
		"logging":        loggingSchema,
		"controller":     {Fields: map[string]*schemaNode{"enabled": nil, "listen": nil, "database": nil, "key_file": nil, "allowed_node_cidrs": nil}},
		"node":           {Fields: map[string]*schemaNode{"controller_url": nil, "state_dir": nil, "max_budget_bps": nil, "derp_port": nil, "stun_port": nil}},
	}}
	serverSchema = &schemaNode{Fields: map[string]*schemaNode{
		"management": {Fields: map[string]*schemaNode{"listen": nil}},
		"hostname":   nil,
		"derp":       derpSchema,
		"admin":      adminSchema,
		"health":     healthSchema,
	}}
	derpSchema = &schemaNode{Fields: map[string]*schemaNode{
		"listen": nil, "stun_listen": nil, "tls_mode": nil, "cert_mode": nil, "cert_dir": nil,
	}}
	adminSchema   = &schemaNode{Fields: map[string]*schemaNode{"socket": nil}}
	healthSchema  = &schemaNode{Fields: map[string]*schemaNode{"listen": nil}}
	storageSchema = &schemaNode{Fields: map[string]*schemaNode{
		"state_dir": nil,
	}}
	loggingSchema = &schemaNode{Fields: map[string]*schemaNode{"level": nil}}
)

var unsupportedFields = map[string]string{
	"control_url":             "UniDERP identities use the official Tailscale device API",
	"controlurl":              "UniDERP identities use the official Tailscale device API",
	"derp_map":                "DERP maps belong to each Tailnet control plane, not UniDERP",
	"derpmap":                 "DERP maps belong to each Tailnet control plane, not UniDERP",
	"derp_map_file":           "DERP maps belong to each Tailnet control plane, not UniDERP",
	"derp_map_url":            "DERP maps belong to each Tailnet control plane, not UniDERP",
	"mesh_psk_file":           "DERP mesh is disabled in UniDERP",
	"mesh_with":               "DERP mesh is disabled in UniDERP",
	"secrets_url":             "DERP mesh is disabled in UniDERP",
	"verify_client_url":       "UniDERP uses local node policy",
	"verify_clients":          "UniDERP uses local node policy",
	"rate_config":             "UniDERP does not expose upstream experimental rate configuration",
	"accept_connection_limit": "UniDERP does not expose upstream connection limits",
	"accept_connection_burst": "UniDERP does not expose upstream connection limits",
}

func collectUnknownFields(node *yaml.Node, path string, schema *schemaNode, warnings *[]string) error {
	if node == nil || schema == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return collectUnknownFields(node.Content[0], path, schema, warnings)
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valueNode := node.Content[i], node.Content[i+1]
		key := keyNode.Value
		fieldPath := key
		if path != "" {
			fieldPath = path + "." + key
		}
		if key == "<<" {
			return fmt.Errorf("YAML merge keys are not supported at %s", fieldPath)
		}
		if reason, ok := unsupportedFields[strings.ToLower(strings.ReplaceAll(key, "-", "_"))]; ok {
			return fmt.Errorf("unsupported field %q at %s: %s", key, fieldPath, reason)
		}
		child, known := schema.Fields[key]
		if !known {
			if err := rejectUnsupportedFields(valueNode, fieldPath); err != nil {
				return err
			}
			*warnings = append(*warnings, fmt.Sprintf("unknown config field ignored: %s", fieldPath))
			continue
		}
		if child == nil {
			continue
		}
		if err := collectUnknownFields(valueNode, fieldPath, child, warnings); err != nil {
			return err
		}
	}
	return nil
}

func rejectUnsupportedFields(node *yaml.Node, path string) error {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) != 0 {
			return rejectUnsupportedFields(node.Content[0], path)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			fieldPath := key
			if path != "" {
				fieldPath = path + "." + key
			}
			if key == "<<" {
				return fmt.Errorf("YAML merge keys are not supported at %s", fieldPath)
			}
			if reason, ok := unsupportedFields[strings.ToLower(strings.ReplaceAll(key, "-", "_"))]; ok {
				return fmt.Errorf("unsupported field %q at %s: %s", key, fieldPath, reason)
			}
			if err := rejectUnsupportedFields(node.Content[i+1], fieldPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			if err := rejectUnsupportedFields(child, itemPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectDuplicateKeys(node *yaml.Node, path string) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		return rejectDuplicateKeys(node.Content[0], path)
	}
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if _, ok := seen[key]; ok {
				where := path
				if where == "" {
					where = "<root>"
				}
				return fmt.Errorf("duplicate YAML mapping key %q at %s", key, where)
			}
			seen[key] = struct{}{}
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if err := rejectDuplicateKeys(node.Content[i+1], childPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			childPath := fmt.Sprintf("%s[%d]", path, i)
			if err := rejectDuplicateKeys(child, childPath); err != nil {
				return err
			}
		}
	}
	return nil
}
