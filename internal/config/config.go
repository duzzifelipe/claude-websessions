package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Sessions      SessionsConfig      `yaml:"sessions"`
	Providers     ProvidersConfig     `yaml:"providers"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Docker        DockerConfig        `yaml:"docker"`
}

type ProvidersConfig struct {
	Default  string         `yaml:"default"`
	Claude   ProviderConfig `yaml:"claude"`
	OpenCode ProviderConfig `yaml:"opencode"`
}

type ProviderConfig struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

type DockerConfig struct {
	CopyCredentials bool `yaml:"copy_credentials"`
}

type ServerConfig struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
}

type SessionsConfig struct {
	ScanInterval     time.Duration `yaml:"-"`
	ScanIntervalRaw  string        `yaml:"scan_interval"`
	OutputBufferSize int64         `yaml:"-"`
	OutputBufferRaw  string        `yaml:"output_buffer_size"`
	DefaultDir       string        `yaml:"default_dir"`
}

type NotificationsConfig struct {
	Desktop         bool     `yaml:"desktop"`
	Events          []string `yaml:"events"`
	ReminderMinutes int      `yaml:"reminder_minutes"`
	Sound           bool     `yaml:"sound"`
	AudioDevice     string   `yaml:"audio_device"`
}

func defaults() *Config {
	return &Config{
		Server: ServerConfig{Port: 8080, Host: "0.0.0.0"},
		Sessions: SessionsConfig{
			ScanInterval: 30 * time.Second, ScanIntervalRaw: "30s",
			OutputBufferSize: 10 * 1024 * 1024, OutputBufferRaw: "10MB",
			DefaultDir: "~/projects",
		},
		Providers: ProvidersConfig{
			Default:  "claude",
			Claude:   ProviderConfig{Command: "claude", Args: []string{}},
			OpenCode: ProviderConfig{Command: "opencode", Args: []string{}},
		},
		Notifications: NotificationsConfig{Desktop: true, Sound: true, Events: []string{"completed", "errored", "waiting"}, ReminderMinutes: 5},
		Docker:        DockerConfig{CopyCredentials: true},
	}
}

func Load(path string) (*Config, error) {
	cfg := defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config: %w", err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
		if err := cfg.parseRawFields(); err != nil {
			return nil, err
		}
	}
	cfg.applyEnvOverrides()
	return cfg, nil
}

func (c *Config) parseRawFields() error {
	if c.Sessions.ScanIntervalRaw != "" {
		d, err := time.ParseDuration(c.Sessions.ScanIntervalRaw)
		if err != nil {
			return fmt.Errorf("parsing scan_interval: %w", err)
		}
		c.Sessions.ScanInterval = d
	}
	if c.Sessions.OutputBufferRaw != "" {
		size, err := parseByteSize(c.Sessions.OutputBufferRaw)
		if err != nil {
			return fmt.Errorf("parsing output_buffer_size: %w", err)
		}
		c.Sessions.OutputBufferSize = size
	}
	c.applyProviderDefaults()
	return nil
}

func (c *Config) applyProviderDefaults() {
	c.Providers.Default = normalizeProvider(c.Providers.Default)

	c.Providers.Claude.Command = strings.TrimSpace(c.Providers.Claude.Command)
	if c.Providers.Claude.Command == "" {
		c.Providers.Claude.Command = "claude"
	}

	c.Providers.OpenCode.Command = strings.TrimSpace(c.Providers.OpenCode.Command)
	if c.Providers.OpenCode.Command == "" {
		c.Providers.OpenCode.Command = "opencode"
	}
}

func normalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "claude" && provider != "opencode" {
		return "claude"
	}
	return provider
}

func (c *Config) applyEnvOverrides() {
	// Reserved for future env var overrides
}

func (c *Config) Save() error {
	homeDir, _ := os.UserHomeDir()
	dir := homeDir + "/.websessions"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	path := dir + "/config.yaml"

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	return n * multiplier, nil
}
