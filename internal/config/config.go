package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type SIP struct {
	Server                    string `yaml:"server"`
	Username                  string `yaml:"username"`
	Password                  string `yaml:"password"`
	DisplayName               string `yaml:"display_name"`
	Transport                 string `yaml:"transport"`
	LocalBind                 string `yaml:"local_bind"`
	RegistrationExpirySeconds int    `yaml:"registration_expiry_seconds"`
}

type RTP struct {
	LocalIP          string `yaml:"local_ip"`
	PortMin          int    `yaml:"port_min"`
	PortMax          int    `yaml:"port_max"`
	PreferredCodec   string `yaml:"preferred_codec"`
	PacketDurationMS int    `yaml:"packet_duration_ms"`
}

type VAD struct {
	Enabled         bool `yaml:"enabled"`
	EnergyThreshold int  `yaml:"energy_threshold"`
	MinSpeechMS     int  `yaml:"min_speech_ms"`
	SilenceEndMS    int  `yaml:"silence_end_ms"`
	MaxUtteranceMS  int  `yaml:"max_utterance_ms"`
	PreRollMS       int  `yaml:"pre_roll_ms"`
}

type STT struct {
	Provider  string `yaml:"provider"`
	Endpoint  string `yaml:"endpoint"`
	APIKeyEnv string `yaml:"api_key_env"`
	Model     string `yaml:"model"`
}

type Claude struct {
	Endpoint       string `yaml:"endpoint"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type TTS struct {
	Provider     string `yaml:"provider"`
	APIKeyEnv    string `yaml:"api_key_env"`
	VoiceID      string `yaml:"voice_id"`
	ModelID      string `yaml:"model_id"`
	OutputFormat string `yaml:"output_format"`
}

type Debug struct {
	LogLevel string `yaml:"log_level"`
	SaveWAV  bool   `yaml:"save_wav"`
	WAVDir   string `yaml:"wav_dir"`
}

type Config struct {
	SIP    SIP    `yaml:"sip"`
	RTP    RTP    `yaml:"rtp"`
	VAD    VAD    `yaml:"vad"`
	STT    STT    `yaml:"stt"`
	Claude Claude `yaml:"claude"`
	TTS    TTS    `yaml:"tts"`
	Debug  Debug  `yaml:"debug"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return &c, nil
}

// Validate returns a list of structural problems with the config.
// Missing secrets (env-var indirected) are returned as warnings, not errors.
func (c *Config) Validate() (errs []string, warns []string) {
	if c.SIP.Server == "" {
		errs = append(errs, "sip.server is required")
	}
	if c.SIP.Username == "" {
		errs = append(errs, "sip.username is required")
	}
	if c.SIP.Password == "" || c.SIP.Password == "change-me" {
		warns = append(warns, "sip.password is unset or still the example value")
	}
	if c.SIP.Transport == "" {
		errs = append(errs, "sip.transport is required (udp|tcp|tls)")
	}
	if c.SIP.LocalBind == "" {
		errs = append(errs, "sip.local_bind is required")
	}
	if c.SIP.RegistrationExpirySeconds <= 0 {
		errs = append(errs, "sip.registration_expiry_seconds must be > 0")
	}

	if c.RTP.PortMin <= 0 || c.RTP.PortMax <= 0 || c.RTP.PortMin > c.RTP.PortMax {
		errs = append(errs, "rtp.port_min/port_max are invalid")
	}
	if c.RTP.PreferredCodec != "pcmu" && c.RTP.PreferredCodec != "pcma" {
		errs = append(errs, fmt.Sprintf("rtp.preferred_codec %q must be pcmu or pcma", c.RTP.PreferredCodec))
	}
	if c.RTP.PacketDurationMS <= 0 {
		errs = append(errs, "rtp.packet_duration_ms must be > 0")
	}

	if c.STT.Endpoint == "" {
		errs = append(errs, "stt.endpoint is required")
	}
	if c.STT.APIKeyEnv != "" && os.Getenv(c.STT.APIKeyEnv) == "" {
		warns = append(warns, fmt.Sprintf("stt: env var %s is not set", c.STT.APIKeyEnv))
	}

	if c.Claude.Endpoint == "" {
		errs = append(errs, "claude.endpoint is required")
	}
	if c.Claude.TimeoutSeconds <= 0 {
		errs = append(errs, "claude.timeout_seconds must be > 0")
	}

	if c.TTS.APIKeyEnv != "" && os.Getenv(c.TTS.APIKeyEnv) == "" {
		warns = append(warns, fmt.Sprintf("tts: env var %s is not set", c.TTS.APIKeyEnv))
	}
	if c.TTS.VoiceID == "" || c.TTS.VoiceID == "replace-me" {
		warns = append(warns, "tts.voice_id is unset or still the example value")
	}

	switch strings.ToLower(c.Debug.LogLevel) {
	case "", "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Sprintf("debug.log_level %q must be debug|info|warn|error", c.Debug.LogLevel))
	}

	return errs, warns
}
