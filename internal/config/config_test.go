package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validBase returns a config whose every required field is populated, so
// individual subtests can mutate one field without tripping unrelated rules.
func validBase() Config {
	c := Defaults()
	c.SIP.Domain = "sip.example.local"
	c.SIP.Registrar = "10.0.0.50"
	c.SIP.Extension = "1001"
	c.SIP.AuthPassword = "s3cret"
	c.RTP.ExternalIP = "10.0.0.100"
	return c
}

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.SIP.RegistrarPort != 5060 {
		t.Errorf("RegistrarPort default = %d, want 5060", d.SIP.RegistrarPort)
	}
	if d.SIP.Expiry != 300 {
		t.Errorf("Expiry default = %d, want 300", d.SIP.Expiry)
	}
	if d.RTP.PortRange != "30000-30100" {
		t.Errorf("PortRange default = %q, want 30000-30100", d.RTP.PortRange)
	}
	if d.HTTP.Port != 3000 {
		t.Errorf("HTTP.Port default = %d, want 3000", d.HTTP.Port)
	}
	if d.Logging.Level != "info" || d.Logging.Format != "text" {
		t.Errorf("Logging defaults = %+v, want info/text", d.Logging)
	}
}

func TestValidate_HappyPath(t *testing.T) {
	c := validBase()
	warns, err := c.Validate()
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
}

func TestValidate_MissingRequiredFieldsAllReported(t *testing.T) {
	c := Defaults() // all required string fields empty
	_, err := c.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	msg := err.Error()
	wantSubstrings := []string{
		"SIP.Domain", "sip.domain", "SIP_DOMAIN", "--sip.domain",
		"SIP.Registrar", "SIP_REGISTRAR",
		"SIP.Extension", "SIP_EXTENSION",
		"SIP.AuthPassword", "SIP_PASSWORD",
		"RTP.ExternalIP", "EXTERNAL_IP",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(msg, s) {
			t.Errorf("error message missing %q.\nFull message:\n%s", s, msg)
		}
	}
}

func TestValidate_RangeChecks(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"registrar_port_zero", func(c *Config) { c.SIP.RegistrarPort = 0 }, "SIP.RegistrarPort"},
		{"registrar_port_too_high", func(c *Config) { c.SIP.RegistrarPort = 70000 }, "SIP.RegistrarPort"},
		{"expiry_zero", func(c *Config) { c.SIP.Expiry = 0 }, "SIP.Expiry"},
		{"http_port_negative", func(c *Config) { c.HTTP.Port = -1 }, "HTTP.Port"},
		{"tls_port_too_high", func(c *Config) { c.HTTP.TLSPort = 70000 }, "HTTP.TLSPort"},
		{"bad_port_range", func(c *Config) { c.RTP.PortRange = "not-a-range" }, "RTP.PortRange"},
		{"bad_log_level", func(c *Config) { c.Logging.Level = "verbose" }, "Logging.Level"},
		{"bad_log_format", func(c *Config) { c.Logging.Format = "yaml" }, "Logging.Format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validBase()
			tc.mutate(&c)
			_, err := c.Validate()
			if err == nil {
				t.Fatalf("%s: expected error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: error missing %q, got: %v", tc.name, tc.want, err)
			}
		})
	}
}

func TestValidate_TLSPortRequiresCertAndKey(t *testing.T) {
	c := validBase()
	c.HTTP.TLSPort = 3443
	_, err := c.Validate()
	if err == nil {
		t.Fatal("expected error for TLSPort without cert/key")
	}
	if !strings.Contains(err.Error(), "HTTP.TLSCert") || !strings.Contains(err.Error(), "HTTP.TLSKey") {
		t.Errorf("expected both TLSCert and TLSKey complaints, got: %v", err)
	}

	c.HTTP.TLSCert = "/etc/cert.pem"
	c.HTTP.TLSKey = "/etc/key.pem"
	if _, err := c.Validate(); err != nil {
		t.Fatalf("TLS with cert+key should validate, got: %v", err)
	}
}

func TestValidate_LevelCaseInsensitive(t *testing.T) {
	c := validBase()
	c.Logging.Level = "DEBUG"
	c.Logging.Format = "JSON"
	if _, err := c.Validate(); err != nil {
		t.Errorf("DEBUG/JSON should validate case-insensitively, got: %v", err)
	}
}

func TestValidate_WarningOnExamplePassword(t *testing.T) {
	c := validBase()
	c.SIP.AuthPassword = "change-me"
	warns, err := c.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], "example value") {
		t.Errorf("expected warning about example password, got: %v", warns)
	}
}

func TestParsePortRange(t *testing.T) {
	cases := []struct {
		in       string
		min, max int
		wantErr  string
	}{
		{"30000-30100", 30000, 30100, ""},
		{" 100 - 200 ", 100, 200, ""},
		{"30000", 0, 0, "expected"},
		{"abc-100", 0, 0, "min port"},
		{"100-abc", 0, 0, "max port"},
		{"0-100", 0, 0, "1-65535"},
		{"100-70000", 0, 0, "1-65535"},
		{"500-500", 0, 0, "must be <"},
		{"500-400", 0, 0, "must be <"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			mn, mx, err := ParsePortRange(c.in)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				if mn != c.min || mx != c.max {
					t.Errorf("got %d-%d, want %d-%d", mn, mx, c.min, c.max)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q missing substring %q", err.Error(), c.wantErr)
			}
		})
	}
}

func TestAuthUsernameOrExtension(t *testing.T) {
	s := SIP{Extension: "1001"}
	if got := s.AuthUsernameOrExtension(); got != "1001" {
		t.Errorf("got %q, want 1001", got)
	}
	s.AuthUsername = "alice"
	if got := s.AuthUsernameOrExtension(); got != "alice" {
		t.Errorf("got %q, want alice", got)
	}
}

// --- Load (three-layer overlay) -------------------------------------------

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func TestLoad_DefaultsOnly(t *testing.T) {
	cfg, err := Load(LoadOptions{Getenv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SIP.RegistrarPort != 5060 || cfg.HTTP.Port != 3000 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoad_YAMLOverlay(t *testing.T) {
	yamlPath := writeYAML(t, `
sip:
  domain: example.com
  registrar: registrar.example.com
  extension: "1234"
  auth_password: pw
  registrar_port: 5061
rtp:
  external_ip: 10.0.0.1
`)
	cfg, err := Load(LoadOptions{
		Path:   yamlPath,
		Getenv: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SIP.Domain != "example.com" || cfg.SIP.RegistrarPort != 5061 {
		t.Errorf("YAML overlay failed: %+v", cfg.SIP)
	}
	// Defaults preserved where YAML is silent.
	if cfg.HTTP.Port != 3000 {
		t.Errorf("HTTP.Port should keep default 3000, got %d", cfg.HTTP.Port)
	}
}

func TestLoad_YAMLUnknownKeyIsError(t *testing.T) {
	yamlPath := writeYAML(t, "sip:\n  nosuchfield: x\n")
	_, err := Load(LoadOptions{Path: yamlPath, Getenv: func(string) (string, bool) { return "", false }})
	if err == nil {
		t.Fatal("expected KnownFields error, got nil")
	}
}

func TestLoad_FileMissing(t *testing.T) {
	_, err := Load(LoadOptions{Path: "/nope/does/not/exist.yaml"})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	yamlPath := writeYAML(t, `
sip:
  domain: from-yaml.example
  extension: "1000"
  auth_password: pw
  registrar: r
rtp:
  external_ip: 1.2.3.4
`)
	env := map[string]string{
		"SIP_DOMAIN":         "from-env.example",
		"SIP_REGISTRAR_PORT": "5070",
		"HTTP_PORT":          "0",
		"LOG_LEVEL":          "debug",
	}
	cfg, err := Load(LoadOptions{
		Path:   yamlPath,
		Getenv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SIP.Domain != "from-env.example" {
		t.Errorf("env should override YAML, got %q", cfg.SIP.Domain)
	}
	if cfg.SIP.RegistrarPort != 5070 {
		t.Errorf("env int override failed, got %d", cfg.SIP.RegistrarPort)
	}
	if cfg.HTTP.Port != 0 {
		t.Errorf("env should override default HTTP.Port, got %d", cfg.HTTP.Port)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("env should override default Logging.Level, got %q", cfg.Logging.Level)
	}
}

func TestLoad_EnvBadIntReportsField(t *testing.T) {
	env := map[string]string{"SIP_EXPIRY": "not-an-int"}
	_, err := Load(LoadOptions{Getenv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	if err == nil {
		t.Fatal("expected error for bad env int")
	}
	if !strings.Contains(err.Error(), "SIP_EXPIRY") {
		t.Errorf("error should mention SIP_EXPIRY, got: %v", err)
	}
}

func TestLoad_FlagsOverrideEnv(t *testing.T) {
	env := map[string]string{
		"SIP_DOMAIN":    "from-env.example",
		"SIP_EXTENSION": "9999",
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterFlags(fs)
	if err := fs.Parse([]string{
		"--sip.domain", "from-flag.example",
		"--sip.extension", "1234",
		"--sip.registrar-port", "5070",
	}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	cfg, err := Load(LoadOptions{
		Getenv:  func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		FlagSet: fs,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SIP.Domain != "from-flag.example" {
		t.Errorf("flag should override env, got %q", cfg.SIP.Domain)
	}
	if cfg.SIP.Extension != "1234" {
		t.Errorf("flag should override env, got %q", cfg.SIP.Extension)
	}
	if cfg.SIP.RegistrarPort != 5070 {
		t.Errorf("flag int override failed, got %d", cfg.SIP.RegistrarPort)
	}
}

func TestLoad_UnsetFlagsDoNotStompEnv(t *testing.T) {
	// Registering flags with empty defaults must not silently wipe env values
	// just because the flag exists. This is the whole point of the
	// flag.Visit-only overlay.
	env := map[string]string{"SIP_DOMAIN": "kept.example"}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterFlags(fs)
	if err := fs.Parse([]string{}); err != nil { // no flags passed
		t.Fatalf("flag parse: %v", err)
	}
	cfg, err := Load(LoadOptions{
		Getenv:  func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		FlagSet: fs,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SIP.Domain != "kept.example" {
		t.Errorf("unset flag stomped env; got %q", cfg.SIP.Domain)
	}
}

func TestLoad_BadFlagIntReportsField(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterFlags(fs)
	if err := fs.Parse([]string{"--sip.expiry", "garbage"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	_, err := Load(LoadOptions{
		Getenv:  func(string) (string, bool) { return "", false },
		FlagSet: fs,
	})
	if err == nil {
		t.Fatal("expected error for bad flag int")
	}
	if !strings.Contains(err.Error(), "sip.expiry") {
		t.Errorf("error should mention sip.expiry, got: %v", err)
	}
}

func TestLoad_NonOurFlagsIgnored(t *testing.T) {
	// --config and --version aren't config flags; mergeFlags must skip them
	// without misinterpreting their string values as overlay.
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("config", "", "")
	fs.Bool("version", false, "")
	RegisterFlags(fs)
	if err := fs.Parse([]string{"--config", "/tmp/x.yaml", "--version"}); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	cfg, err := Load(LoadOptions{
		Getenv:  func(string) (string, bool) { return "", false },
		FlagSet: fs,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Nothing from the harness flags should have leaked into the config.
	if cfg.SIP.Domain != "" || cfg.HTTP.Port != 3000 {
		t.Errorf("harness flags leaked: %+v", cfg)
	}
}

func TestLoad_EverySpecCovered(t *testing.T) {
	// Exercise every fieldSpec.apply closure so the spec table can't grow a
	// dead entry unnoticed and so coverage reflects the real mapping.
	env := map[string]string{
		"SIP_DOMAIN":         "d",
		"SIP_REGISTRAR":      "r",
		"SIP_REGISTRAR_PORT": "5070",
		"SIP_EXTENSION":      "e",
		"SIP_AUTH_USERNAME":  "u",
		"SIP_AUTH_ID":        "a",
		"SIP_PASSWORD":       "p",
		"SIP_EXPIRY":         "60",
		"EXTERNAL_IP":        "1.1.1.1",
		"RTP_PORT_RANGE":     "40000-40100",
		"HTTP_PORT":          "8080",
		"TLS_PORT":           "8443",
		"TLS_CERT_FILE":      "/c.pem",
		"TLS_KEY_FILE":       "/k.pem",
		"LOG_LEVEL":          "warn",
		"LOG_FORMAT":         "json",
	}
	cfg, err := Load(LoadOptions{Getenv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	checks := map[string]any{
		"SIP.Domain":         cfg.SIP.Domain,
		"SIP.Registrar":      cfg.SIP.Registrar,
		"SIP.RegistrarPort":  cfg.SIP.RegistrarPort,
		"SIP.Extension":      cfg.SIP.Extension,
		"SIP.AuthUsername":   cfg.SIP.AuthUsername,
		"SIP.AuthID":         cfg.SIP.AuthID,
		"SIP.AuthPassword":   cfg.SIP.AuthPassword,
		"SIP.Expiry":         cfg.SIP.Expiry,
		"RTP.ExternalIP":     cfg.RTP.ExternalIP,
		"RTP.PortRange":      cfg.RTP.PortRange,
		"HTTP.Port":          cfg.HTTP.Port,
		"HTTP.TLSPort":       cfg.HTTP.TLSPort,
		"HTTP.TLSCert":       cfg.HTTP.TLSCert,
		"HTTP.TLSKey":        cfg.HTTP.TLSKey,
		"Logging.Level":      cfg.Logging.Level,
		"Logging.Format":     cfg.Logging.Format,
	}
	wantInt := map[string]int{"SIP.RegistrarPort": 5070, "SIP.Expiry": 60, "HTTP.Port": 8080, "HTTP.TLSPort": 8443}
	for field, got := range checks {
		switch g := got.(type) {
		case string:
			if g == "" {
				t.Errorf("%s not populated", field)
			}
		case int:
			if g != wantInt[field] {
				t.Errorf("%s = %d, want %d", field, g, wantInt[field])
			}
		}
	}
}

func TestLoad_FullStack(t *testing.T) {
	yamlPath := writeYAML(t, `
sip:
  domain: yaml.example
  registrar: yaml-reg
  extension: "1000"
  auth_password: yamlpw
rtp:
  external_ip: 9.9.9.9
`)
	env := map[string]string{
		"SIP_DOMAIN": "env.example", // env overrides YAML
		"HTTP_PORT":  "8080",        // env-only field
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterFlags(fs)
	if err := fs.Parse([]string{"--sip.extension", "9999"}); err != nil { // flag overrides YAML
		t.Fatalf("flag parse: %v", err)
	}
	cfg, err := Load(LoadOptions{
		Path:    yamlPath,
		Getenv:  func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		FlagSet: fs,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SIP.Domain != "env.example" {
		t.Errorf("env-over-yaml failed: %q", cfg.SIP.Domain)
	}
	if cfg.SIP.Extension != "9999" {
		t.Errorf("flag-over-yaml failed: %q", cfg.SIP.Extension)
	}
	if cfg.HTTP.Port != 8080 {
		t.Errorf("env-only field failed: %d", cfg.HTTP.Port)
	}
	if cfg.RTP.ExternalIP != "9.9.9.9" {
		t.Errorf("yaml-only field lost: %q", cfg.RTP.ExternalIP)
	}
	if _, err := cfg.Validate(); err != nil {
		t.Errorf("merged config should validate: %v", err)
	}
}
