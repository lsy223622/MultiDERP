package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const CurrentVersion = 1

const (
	DefaultDERPListen       = ":3377"
	DefaultSTUNListen       = ":3478"
	DefaultAdminSocket      = "/run/multiderp/admin.sock"
	DefaultHealthListen     = "127.0.0.1:9090"
	DefaultStateDir         = "/data"
	DefaultTailnetStateDir  = "/data/tailnets"
	DefaultOrphanStateDir   = "/data/orphans"
	DefaultLoggingLevel     = "info"
	DefaultTLSMode          = "external"
	DefaultConfigPath       = "/data/config.yaml"
	DefaultAdmissionAddress = "127.0.0.1:3340"
)

type Config struct {
	Version  int             `yaml:"version"`
	Server   ServerConfig    `yaml:"server"`
	Storage  StorageConfig   `yaml:"storage"`
	Logging  LoggingConfig   `yaml:"logging"`
	Tailnets []TailnetConfig `yaml:"tailnets"`
}

type ServerConfig struct {
	Hostname string       `yaml:"hostname"`
	DERP     DERPConfig   `yaml:"derp"`
	Admin    AdminConfig  `yaml:"admin"`
	Health   HealthConfig `yaml:"health"`
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
	StateDir        string `yaml:"state_dir"`
	TailnetStateDir string `yaml:"tailnet_state_dir"`
	OrphanStateDir  string `yaml:"orphan_state_dir"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}

type TailnetConfig struct {
	Name     string     `yaml:"name"`
	Disabled bool       `yaml:"disabled"`
	Required bool       `yaml:"required"`
	Hostname string     `yaml:"hostname"`
	Auth     AuthConfig `yaml:"auth"`
}

type AuthConfig struct {
	Type             string   `yaml:"type"`
	ClientSecretFile string   `yaml:"client_secret_file"`
	AuthKeyFile      string   `yaml:"auth_key_file"`
	Tags             []string `yaml:"tags"`
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
	if c.Storage.TailnetStateDir == "" {
		c.Storage.TailnetStateDir = DefaultTailnetStateDir
	}
	if c.Storage.OrphanStateDir == "" {
		c.Storage.OrphanStateDir = DefaultOrphanStateDir
	}
	if c.Logging.Level == "" {
		c.Logging.Level = DefaultLoggingLevel
	}
	for i := range c.Tailnets {
		if c.Tailnets[i].Hostname == "" && c.Tailnets[i].Name != "" {
			c.Tailnets[i].Hostname = "multiderp-" + c.Tailnets[i].Name
		}
	}
}

func (c Config) Clone() Config {
	clone := c
	clone.Tailnets = make([]TailnetConfig, len(c.Tailnets))
	copy(clone.Tailnets, c.Tailnets)
	for i := range clone.Tailnets {
		clone.Tailnets[i].Auth.Tags = append([]string(nil), c.Tailnets[i].Auth.Tags...)
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
		return ParseResult{}, fmt.Errorf("unsupported config version %d; expected %d", version, CurrentVersion)
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

func (c Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("unsupported config version %d; expected %d", c.Version, CurrentVersion)
	}
	needsDERPHostname := false
	for _, tailnet := range c.Tailnets {
		if !tailnet.Disabled {
			needsDERPHostname = true
			break
		}
	}
	if needsDERPHostname && strings.TrimSpace(c.Server.Hostname) == "" {
		return errors.New("server.hostname is required when an enabled verifier is configured")
	}
	if c.Server.Hostname != "" {
		if err := validateHostname(c.Server.Hostname); err != nil {
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
		return errors.New("server.derp.cert_mode: unsupported value \"gcp\" in V1; supported values are none, letsencrypt, or manual")
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
	if strings.TrimSpace(c.Storage.StateDir) == "" || strings.TrimSpace(c.Storage.TailnetStateDir) == "" || strings.TrimSpace(c.Storage.OrphanStateDir) == "" {
		return errors.New("storage paths must not be empty")
	}

	seen := make(map[string]string, len(c.Tailnets))
	for i, t := range c.Tailnets {
		path := fmt.Sprintf("tailnets[%d]", i)
		if err := validateName(t.Name); err != nil {
			return fmt.Errorf("%s.name: %w", path, err)
		}
		key := strings.ToLower(t.Name)
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("duplicate verifier name %q at %s and %s", t.Name, previous, path)
		}
		seen[key] = path
		if t.Hostname != "" {
			if err := validateHostname(t.Hostname); err != nil {
				return fmt.Errorf("%s.hostname: %w", path, err)
			}
		}
		switch t.Auth.Type {
		case "web":
			if t.Auth.ClientSecretFile != "" || t.Auth.AuthKeyFile != "" {
				return fmt.Errorf("%s.auth: web authentication cannot specify a secret file", path)
			}
		case "oauth":
			if strings.TrimSpace(t.Auth.ClientSecretFile) == "" {
				return fmt.Errorf("%s.auth.client_secret_file is required for oauth authentication", path)
			}
			if t.Auth.AuthKeyFile != "" {
				return fmt.Errorf("%s.auth: oauth authentication cannot specify auth_key_file", path)
			}
		case "auth_key":
			if strings.TrimSpace(t.Auth.AuthKeyFile) == "" {
				return fmt.Errorf("%s.auth.auth_key_file is required for auth_key authentication", path)
			}
			if t.Auth.ClientSecretFile != "" {
				return fmt.Errorf("%s.auth: auth_key authentication cannot specify client_secret_file", path)
			}
		default:
			return fmt.Errorf("%s.auth.type: unsupported value %q; expected web, oauth, or auth_key", path, t.Auth.Type)
		}
		for j, tag := range t.Auth.Tags {
			if strings.TrimSpace(tag) == "" {
				return fmt.Errorf("%s.auth.tags[%d] must not be empty", path, j)
			}
		}
	}
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ValidateReload(oldConfig, newConfig Config) error {
	oldConfig.Normalize()
	newConfig.Normalize()
	if err := newConfig.Validate(); err != nil {
		return err
	}
	oldByName := make(map[string]TailnetConfig, len(oldConfig.Tailnets))
	for _, t := range oldConfig.Tailnets {
		oldByName[strings.ToLower(t.Name)] = t
	}
	for _, t := range newConfig.Tailnets {
		old, ok := oldByName[strings.ToLower(t.Name)]
		if !ok {
			continue
		}
		if old.Name != t.Name || old.Auth.Type != t.Auth.Type || old.Hostname != t.Hostname ||
			old.Auth.ClientSecretFile != t.Auth.ClientSecretFile || old.Auth.AuthKeyFile != t.Auth.AuthKeyFile ||
			!sameStrings(old.Auth.Tags, t.Auth.Tags) {
			return fmt.Errorf("verifier %q identity/auth/hostname changed; use remove/reset and add instead of reusing state", t.Name)
		}
	}
	newByName := make(map[string]struct{}, len(newConfig.Tailnets))
	for _, t := range newConfig.Tailnets {
		newByName[strings.ToLower(t.Name)] = struct{}{}
	}
	for _, t := range oldConfig.Tailnets {
		if _, ok := newByName[strings.ToLower(t.Name)]; !ok {
			return fmt.Errorf("verifier %q was removed from config; use tailnet remove to preserve its state as an orphan", t.Name)
		}
	}
	return nil
}

func RestartOnlyChanged(oldConfig, newConfig Config) bool {
	oldConfig.Normalize()
	newConfig.Normalize()
	return oldConfig.Server.Hostname != newConfig.Server.Hostname ||
		!reflect.DeepEqual(oldConfig.Server.DERP, newConfig.Server.DERP) ||
		oldConfig.Server.Admin.Socket != newConfig.Server.Admin.Socket ||
		oldConfig.Server.Health.Listen != newConfig.Server.Health.Listen ||
		oldConfig.Storage != newConfig.Storage
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

func NewOrphanID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate orphan id: %w", err)
	}
	return "orphan-" + hex.EncodeToString(raw[:]), nil
}

func validateName(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("must be a non-empty local identifier")
	}
	if len(name) > 64 || strings.ContainsAny(name, `/\\`) || strings.TrimSpace(name) != name {
		return errors.New("must be a path-safe identifier of at most 64 characters")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' && r != '.' {
			return fmt.Errorf("contains unsupported character %q", r)
		}
	}
	return nil
}

var hostnameLabel = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func validateHostname(hostname string) error {
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
	Item   *schemaNode
}

var (
	rootSchema = &schemaNode{Fields: map[string]*schemaNode{
		"version":  nil,
		"server":   serverSchema,
		"storage":  storageSchema,
		"logging":  loggingSchema,
		"tailnets": {Item: tailnetSchema},
	}}
	serverSchema = &schemaNode{Fields: map[string]*schemaNode{
		"hostname": nil,
		"derp":     derpSchema,
		"admin":    adminSchema,
		"health":   healthSchema,
	}}
	derpSchema = &schemaNode{Fields: map[string]*schemaNode{
		"listen": nil, "stun_listen": nil, "tls_mode": nil, "cert_mode": nil, "cert_dir": nil,
	}}
	adminSchema   = &schemaNode{Fields: map[string]*schemaNode{"socket": nil}}
	healthSchema  = &schemaNode{Fields: map[string]*schemaNode{"listen": nil}}
	storageSchema = &schemaNode{Fields: map[string]*schemaNode{
		"state_dir": nil, "tailnet_state_dir": nil, "orphan_state_dir": nil,
	}}
	loggingSchema = &schemaNode{Fields: map[string]*schemaNode{"level": nil}}
	tailnetSchema = &schemaNode{Fields: map[string]*schemaNode{
		"name": nil, "disabled": nil, "required": nil, "hostname": nil, "auth": authSchema,
	}}
	authSchema = &schemaNode{Fields: map[string]*schemaNode{
		"type": nil, "client_secret_file": nil, "auth_key_file": nil, "tags": nil,
	}}
)

var unsupportedFields = map[string]string{
	"control_url":             "MultiDERP V1 only uses the official Tailscale control plane",
	"controlurl":              "MultiDERP V1 only uses the official Tailscale control plane",
	"derp_map":                "DERP maps belong to each Tailnet control plane, not MultiDERP",
	"derpmap":                 "DERP maps belong to each Tailnet control plane, not MultiDERP",
	"derp_map_file":           "DERP maps belong to each Tailnet control plane, not MultiDERP",
	"derp_map_url":            "DERP maps belong to each Tailnet control plane, not MultiDERP",
	"mesh_psk_file":           "DERP mesh is disabled in MultiDERP V1",
	"mesh_with":               "DERP mesh is disabled in MultiDERP V1",
	"secrets_url":             "DERP mesh is disabled in MultiDERP V1",
	"verify_client_url":       "MultiDERP owns the admission callback",
	"verify_clients":          "MultiDERP owns client admission and does not use local tailscaled verification",
	"rate_config":             "MultiDERP V1 does not expose upstream experimental rate configuration",
	"accept_connection_limit": "MultiDERP V1 does not expose upstream connection limits",
	"accept_connection_burst": "MultiDERP V1 does not expose upstream connection limits",
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
		if key == "tailnets" && valueNode.Kind == yaml.SequenceNode {
			for index, item := range valueNode.Content {
				itemPath := fmt.Sprintf("%s[%d]", fieldPath, index)
				if err := collectUnknownFields(item, itemPath, child.Item, warnings); err != nil {
					return err
				}
			}
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

func OrphanMetadataPath(dir string) string {
	return filepath.Join(dir, "metadata.yaml")
}

type OrphanMetadata struct {
	ID        string    `yaml:"id"`
	Name      string    `yaml:"name"`
	CreatedAt time.Time `yaml:"created_at"`
}

func WriteOrphanMetadata(dir string, metadata OrphanMetadata) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if filepath.Clean(dir) != "." {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	data, err := yaml.Marshal(metadata)
	if err != nil {
		return err
	}
	return writeAtomicBytes(OrphanMetadataPath(dir), data, ".metadata.yaml.*.tmp")
}

func ReadOrphanMetadata(dir string) (OrphanMetadata, error) {
	data, err := os.ReadFile(OrphanMetadataPath(dir))
	if err != nil {
		return OrphanMetadata{}, err
	}
	var metadata OrphanMetadata
	if err := yaml.Unmarshal(data, &metadata); err != nil {
		return OrphanMetadata{}, err
	}
	return metadata, nil
}

func IsWithin(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func ConfigsEqual(a, b Config) bool {
	a.Normalize()
	b.Normalize()
	return reflect.DeepEqual(a, b)
}
