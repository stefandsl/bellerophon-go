// Package config holds the runtime configuration for bellerophon and the
// three-layer loader (YAML file → environment variables → CLI flags) defined
// in docs/M001-CONTEXT.md §2-§3.
//
// Field names, YAML keys, env var names and CLI flag names are deliberately
// kept in lock-step with the table in M001-CONTEXT.md so validation messages
// can name all four — operators set values from any layer and need to know
// which knob to reach for.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the fully-resolved runtime configuration. It is populated by
// successively overlaying Defaults, YAML, env, and flags; see Load.
type Config struct {
	SIP     SIP     `yaml:"sip"`
	RTP     RTP     `yaml:"rtp"`
	HTTP    HTTP    `yaml:"http"`
	Logging Logging `yaml:"logging"`
}

type SIP struct {
	Domain        string `yaml:"domain"`
	Registrar     string `yaml:"registrar"`
	RegistrarPort int    `yaml:"registrar_port"`
	Extension     string `yaml:"extension"`
	AuthUsername  string `yaml:"auth_username"`
	AuthID        string `yaml:"auth_id"`
	AuthPassword  string `yaml:"auth_password"`
	Expiry        int    `yaml:"expiry"`
}

type RTP struct {
	ExternalIP string `yaml:"external_ip"`
	PortRange  string `yaml:"port_range"`
}

type HTTP struct {
	Port    int    `yaml:"port"`
	TLSPort int    `yaml:"tls_port"`
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
}

type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Defaults returns the built-in default config. Required fields are left at
// their zero value so Validate flags them when no other layer fills them in.
func Defaults() Config {
	return Config{
		SIP: SIP{
			RegistrarPort: 5060,
			Expiry:        300,
		},
		RTP: RTP{
			PortRange: "30000-30100",
		},
		HTTP: HTTP{
			Port: 3000,
		},
		Logging: Logging{
			Level:  "info",
			Format: "text",
		},
	}
}

// LoadOptions controls Load. Zero value is valid: no file, env from
// os.LookupEnv, no flag overlay.
type LoadOptions struct {
	// Path to a YAML config file. Empty string skips the file layer.
	Path string
	// Getenv overrides os.LookupEnv. Used by tests to inject a fake env.
	Getenv func(string) (string, bool)
	// FlagSet is consulted for overlay. Only flags explicitly set
	// (via flag.Visit) overlay; defaults registered with flag.Int etc.
	// do not stomp lower layers.
	FlagSet *flag.FlagSet
}

// Load applies defaults → file → env → flags in order and returns the merged
// config. It does NOT call Validate; callers decide whether to honour
// --check-config style behaviour first.
func Load(opts LoadOptions) (Config, error) {
	cfg := Defaults()

	if opts.Path != "" {
		if err := mergeYAMLFile(&cfg, opts.Path); err != nil {
			return Config{}, err
		}
	}

	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	if err := mergeEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}

	if opts.FlagSet != nil {
		if err := mergeFlags(&cfg, opts.FlagSet); err != nil {
			return Config{}, err
		}
	}

	return cfg, nil
}

func mergeYAMLFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // typo'd keys are errors, not silent drops.
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}
	return nil
}

// fieldSpec describes one mappable field. It captures the four identifiers
// (yaml key, env var, flag name, struct path) so validation errors and
// override loops share a single source of truth.
type fieldSpec struct {
	YAMLKey  string
	EnvVar   string
	FlagName string
	// apply writes the (string-typed) source value into cfg. Returns a
	// parse error if the value doesn't fit the field's type.
	apply func(cfg *Config, raw string) error
}

func specs() []fieldSpec {
	return []fieldSpec{
		{"sip.domain", "SIP_DOMAIN", "sip.domain",
			func(c *Config, v string) error { c.SIP.Domain = v; return nil }},
		{"sip.registrar", "SIP_REGISTRAR", "sip.registrar",
			func(c *Config, v string) error { c.SIP.Registrar = v; return nil }},
		{"sip.registrar_port", "SIP_REGISTRAR_PORT", "sip.registrar-port",
			intSetter(func(c *Config, n int) { c.SIP.RegistrarPort = n })},
		{"sip.extension", "SIP_EXTENSION", "sip.extension",
			func(c *Config, v string) error { c.SIP.Extension = v; return nil }},
		{"sip.auth_username", "SIP_AUTH_USERNAME", "sip.auth-username",
			func(c *Config, v string) error { c.SIP.AuthUsername = v; return nil }},
		{"sip.auth_id", "SIP_AUTH_ID", "sip.auth-id",
			func(c *Config, v string) error { c.SIP.AuthID = v; return nil }},
		{"sip.auth_password", "SIP_PASSWORD", "sip.auth-password",
			func(c *Config, v string) error { c.SIP.AuthPassword = v; return nil }},
		{"sip.expiry", "SIP_EXPIRY", "sip.expiry",
			intSetter(func(c *Config, n int) { c.SIP.Expiry = n })},
		{"rtp.external_ip", "EXTERNAL_IP", "rtp.external-ip",
			func(c *Config, v string) error { c.RTP.ExternalIP = v; return nil }},
		{"rtp.port_range", "RTP_PORT_RANGE", "rtp.port-range",
			func(c *Config, v string) error { c.RTP.PortRange = v; return nil }},
		{"http.port", "HTTP_PORT", "http.port",
			intSetter(func(c *Config, n int) { c.HTTP.Port = n })},
		{"http.tls_port", "TLS_PORT", "http.tls-port",
			intSetter(func(c *Config, n int) { c.HTTP.TLSPort = n })},
		{"http.tls_cert", "TLS_CERT_FILE", "http.tls-cert",
			func(c *Config, v string) error { c.HTTP.TLSCert = v; return nil }},
		{"http.tls_key", "TLS_KEY_FILE", "http.tls-key",
			func(c *Config, v string) error { c.HTTP.TLSKey = v; return nil }},
		{"logging.level", "LOG_LEVEL", "logging.level",
			func(c *Config, v string) error { c.Logging.Level = v; return nil }},
		{"logging.format", "LOG_FORMAT", "logging.format",
			func(c *Config, v string) error { c.Logging.Format = v; return nil }},
	}
}

func intSetter(assign func(*Config, int)) func(*Config, string) error {
	return func(c *Config, raw string) error {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("expected integer, got %q", raw)
		}
		assign(c, n)
		return nil
	}
}

func mergeEnv(cfg *Config, getenv func(string) (string, bool)) error {
	for _, s := range specs() {
		raw, ok := getenv(s.EnvVar)
		if !ok || raw == "" {
			continue
		}
		if err := s.apply(cfg, raw); err != nil {
			return fmt.Errorf("env %s: %w", s.EnvVar, err)
		}
	}
	return nil
}

// RegisterFlags declares every overridable flag on fs with empty defaults.
// Empty default + Visit-only overlay means callers can tell "user set the
// flag" apart from "flag exists but wasn't passed" — necessary so flags
// don't stomp env values just by being registered.
func RegisterFlags(fs *flag.FlagSet) {
	for _, s := range specs() {
		fs.String(s.FlagName, "", "override "+s.YAMLKey+" (env "+s.EnvVar+")")
	}
}

func mergeFlags(cfg *Config, fs *flag.FlagSet) error {
	byName := map[string]fieldSpec{}
	for _, s := range specs() {
		byName[s.FlagName] = s
	}
	var visitErr error
	fs.Visit(func(f *flag.Flag) {
		if visitErr != nil {
			return
		}
		s, ok := byName[f.Name]
		if !ok {
			return // not one of ours; --config and friends land here.
		}
		raw := f.Value.String()
		if raw == "" {
			return
		}
		if err := s.apply(cfg, raw); err != nil {
			visitErr = fmt.Errorf("flag --%s: %w", f.Name, err)
		}
	})
	return visitErr
}

// Validate checks required fields and cross-field invariants. Returns a single
// error whose message lists every problem on its own line, each naming the
// struct field path, YAML key, env var and flag so the operator can fix it
// from any input source. Warnings (non-fatal) are returned separately.
func (c *Config) Validate() (warnings []string, err error) {
	var problems []string

	required := []struct {
		ok                              bool
		field, yamlKey, env, flagSwitch string
	}{
		{c.SIP.Domain != "", "SIP.Domain", "sip.domain", "SIP_DOMAIN", "--sip.domain"},
		{c.SIP.Registrar != "", "SIP.Registrar", "sip.registrar", "SIP_REGISTRAR", "--sip.registrar"},
		{c.SIP.Extension != "", "SIP.Extension", "sip.extension", "SIP_EXTENSION", "--sip.extension"},
		{c.SIP.AuthPassword != "", "SIP.AuthPassword", "sip.auth_password", "SIP_PASSWORD", "--sip.auth-password"},
		{c.RTP.ExternalIP != "", "RTP.ExternalIP", "rtp.external_ip", "EXTERNAL_IP", "--rtp.external-ip"},
	}
	for _, r := range required {
		if !r.ok {
			problems = append(problems, fmt.Sprintf(
				"%s (yaml %s, env %s, flag %s) is required",
				r.field, r.yamlKey, r.env, r.flagSwitch))
		}
	}

	if c.SIP.RegistrarPort <= 0 || c.SIP.RegistrarPort > 65535 {
		problems = append(problems, fmt.Sprintf(
			"SIP.RegistrarPort (yaml sip.registrar_port, env SIP_REGISTRAR_PORT, flag --sip.registrar-port) must be 1-65535, got %d",
			c.SIP.RegistrarPort))
	}
	if c.SIP.Expiry <= 0 {
		problems = append(problems, fmt.Sprintf(
			"SIP.Expiry (yaml sip.expiry, env SIP_EXPIRY, flag --sip.expiry) must be > 0, got %d",
			c.SIP.Expiry))
	}

	if c.RTP.PortRange != "" {
		if _, _, perr := ParsePortRange(c.RTP.PortRange); perr != nil {
			problems = append(problems, fmt.Sprintf(
				"RTP.PortRange (yaml rtp.port_range, env RTP_PORT_RANGE, flag --rtp.port-range): %v",
				perr))
		}
	}

	if c.HTTP.Port < 0 || c.HTTP.Port > 65535 {
		problems = append(problems, fmt.Sprintf(
			"HTTP.Port (yaml http.port, env HTTP_PORT, flag --http.port) must be 0-65535, got %d",
			c.HTTP.Port))
	}
	if c.HTTP.TLSPort < 0 || c.HTTP.TLSPort > 65535 {
		problems = append(problems, fmt.Sprintf(
			"HTTP.TLSPort (yaml http.tls_port, env TLS_PORT, flag --http.tls-port) must be 0-65535, got %d",
			c.HTTP.TLSPort))
	}
	if c.HTTP.TLSPort > 0 {
		if c.HTTP.TLSCert == "" {
			problems = append(problems, "HTTP.TLSCert (yaml http.tls_cert, env TLS_CERT_FILE, flag --http.tls-cert) is required when HTTP.TLSPort > 0")
		}
		if c.HTTP.TLSKey == "" {
			problems = append(problems, "HTTP.TLSKey (yaml http.tls_key, env TLS_KEY_FILE, flag --http.tls-key) is required when HTTP.TLSPort > 0")
		}
	}

	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf(
			"Logging.Level (yaml logging.level, env LOG_LEVEL, flag --logging.level) must be debug|info|warn|error, got %q",
			c.Logging.Level))
	}
	switch strings.ToLower(c.Logging.Format) {
	case "text", "json":
	default:
		problems = append(problems, fmt.Sprintf(
			"Logging.Format (yaml logging.format, env LOG_FORMAT, flag --logging.format) must be text|json, got %q",
			c.Logging.Format))
	}

	if c.SIP.AuthPassword == "change-me" {
		warnings = append(warnings, "SIP.AuthPassword still holds the example value \"change-me\"")
	}

	if len(problems) == 0 {
		return warnings, nil
	}
	return warnings, errors.New("config invalid:\n  - " + strings.Join(problems, "\n  - "))
}

// ParsePortRange splits "min-max" into the two endpoints, validating that
// both are valid ports and that min < max. Exported so RTP code in S04 can
// reuse the same parser.
func ParsePortRange(s string) (minPort, maxPort int, err error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected \"min-max\", got %q", s)
	}
	minPort, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("min port %q: %w", parts[0], err)
	}
	maxPort, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("max port %q: %w", parts[1], err)
	}
	if minPort <= 0 || minPort > 65535 || maxPort <= 0 || maxPort > 65535 {
		return 0, 0, fmt.Errorf("ports must be 1-65535, got %d-%d", minPort, maxPort)
	}
	if minPort >= maxPort {
		return 0, 0, fmt.Errorf("min port %d must be < max port %d", minPort, maxPort)
	}
	return minPort, maxPort, nil
}

// AuthUsernameOrExtension returns the AuthUsername if set, otherwise the
// Extension — the same fallback the 3CX REGISTER path uses in voice-app.
func (s SIP) AuthUsernameOrExtension() string {
	if s.AuthUsername != "" {
		return s.AuthUsername
	}
	return s.Extension
}
