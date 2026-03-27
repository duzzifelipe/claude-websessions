package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IgorDeo/claude-websessions/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Server.Port)
	}
	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("expected host 0.0.0.0, got %s", cfg.Server.Host)
	}
	if cfg.Sessions.ScanInterval != 30*time.Second {
		t.Errorf("expected scan interval 30s, got %v", cfg.Sessions.ScanInterval)
	}
	if cfg.Sessions.OutputBufferSize != 10*1024*1024 {
		t.Errorf("expected buffer 10MB, got %d", cfg.Sessions.OutputBufferSize)
	}
	if cfg.Providers.Default != "claude" {
		t.Errorf("expected default provider claude, got %s", cfg.Providers.Default)
	}
	if cfg.Providers.Claude.Command != "claude" {
		t.Errorf("expected claude command claude, got %s", cfg.Providers.Claude.Command)
	}
	if cfg.Providers.OpenCode.Command != "opencode" {
		t.Errorf("expected opencode command opencode, got %s", cfg.Providers.OpenCode.Command)
	}
	if cfg.Notifications.ReminderMinutes != 5 {
		t.Errorf("expected reminder minutes 5, got %d", cfg.Notifications.ReminderMinutes)
	}
	if !cfg.Notifications.Sound {
		t.Error("expected notifications sound enabled by default")
	}
	if cfg.Notifications.AudioDevice != "" {
		t.Errorf("expected default audio device empty, got %q", cfg.Notifications.AudioDevice)
	}
	if !cfg.Docker.CopyCredentials {
		t.Error("expected docker.copy_credentials enabled by default")
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte(`
server:
  port: 9090
  host: 127.0.0.1
sessions:
  scan_interval: 10s
  output_buffer_size: 5MB
  default_dir: /tmp/test
providers:
  default: opencode
  claude:
    command: /usr/local/bin/claude
    args: [--dangerously-skip-permissions]
  opencode:
    command: /usr/local/bin/opencode
    args: [--approval-mode, full-auto]
notifications:
  desktop: false
  events: [completed]
  reminder_minutes: 15
  sound: false
  audio_device: Built-in Output
docker:
  copy_credentials: false
auth:
  enabled: true
  token: "secret123"
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Sessions.OutputBufferSize != 5*1024*1024 {
		t.Errorf("expected buffer 5MB, got %d", cfg.Sessions.OutputBufferSize)
	}
	if cfg.Providers.Default != "opencode" {
		t.Errorf("expected default provider opencode, got %s", cfg.Providers.Default)
	}
	if cfg.Providers.Claude.Command != "/usr/local/bin/claude" {
		t.Errorf("expected custom claude command, got %s", cfg.Providers.Claude.Command)
	}
	if len(cfg.Providers.Claude.Args) != 1 || cfg.Providers.Claude.Args[0] != "--dangerously-skip-permissions" {
		t.Errorf("unexpected claude args: %#v", cfg.Providers.Claude.Args)
	}
	if cfg.Providers.OpenCode.Command != "/usr/local/bin/opencode" {
		t.Errorf("expected custom opencode command, got %s", cfg.Providers.OpenCode.Command)
	}
	if len(cfg.Providers.OpenCode.Args) != 2 || cfg.Providers.OpenCode.Args[0] != "--approval-mode" || cfg.Providers.OpenCode.Args[1] != "full-auto" {
		t.Errorf("unexpected opencode args: %#v", cfg.Providers.OpenCode.Args)
	}
	if cfg.Notifications.ReminderMinutes != 15 {
		t.Errorf("expected reminder minutes 15, got %d", cfg.Notifications.ReminderMinutes)
	}
	if cfg.Notifications.Sound {
		t.Error("expected notifications sound disabled")
	}
	if cfg.Notifications.AudioDevice != "Built-in Output" {
		t.Errorf("expected audio device Built-in Output, got %q", cfg.Notifications.AudioDevice)
	}
	if cfg.Docker.CopyCredentials {
		t.Error("expected docker.copy_credentials disabled")
	}
}

func TestLoadProvidersFallbackToClaudeWhenUnsetOrInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte(`
providers:
  default: unknown
  claude:
    command: ""
  opencode:
    command: ""
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Providers.Default != "claude" {
		t.Errorf("expected default provider to fallback to claude, got %s", cfg.Providers.Default)
	}
	if cfg.Providers.Claude.Command != "claude" {
		t.Errorf("expected claude command fallback, got %s", cfg.Providers.Claude.Command)
	}
	if cfg.Providers.OpenCode.Command != "opencode" {
		t.Errorf("expected opencode command fallback, got %s", cfg.Providers.OpenCode.Command)
	}
}
