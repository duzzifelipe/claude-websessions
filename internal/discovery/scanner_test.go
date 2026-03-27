package discovery_test

import (
	"testing"

	"github.com/IgorDeo/claude-websessions/internal/discovery"
)

func TestParseProcessInfo(t *testing.T) {
	info, err := discovery.ParseCmdline("/usr/local/bin/claude --resume abc123 --session-id xyz")
	if err != nil {
		t.Fatal(err)
	}
	if info.Binary != "/usr/local/bin/claude" {
		t.Errorf("expected claude binary, got %s", info.Binary)
	}
	if info.Provider != "claude" {
		t.Errorf("expected provider claude, got %s", info.Provider)
	}
	if info.ExternalSessionID != "abc123" {
		t.Errorf("expected external session id abc123, got %s", info.ExternalSessionID)
	}
	if info.ClaudeID != "abc123" {
		t.Errorf("expected claude id abc123, got %s", info.ClaudeID)
	}
}

func TestParseProcessInfo_OpenCode(t *testing.T) {
	info, err := discovery.ParseCmdline("/opt/homebrew/bin/opencode --session oc-123")
	if err != nil {
		t.Fatal(err)
	}
	if info.Binary != "/opt/homebrew/bin/opencode" {
		t.Errorf("expected opencode binary, got %s", info.Binary)
	}
	if info.Provider != "opencode" {
		t.Errorf("expected provider opencode, got %s", info.Provider)
	}
	if info.ExternalSessionID != "oc-123" {
		t.Errorf("expected external session id oc-123, got %s", info.ExternalSessionID)
	}
	if info.ClaudeID != "" {
		t.Errorf("expected empty claude id for opencode, got %s", info.ClaudeID)
	}
}

func TestParseProcessInfo_OpenCodeResumeCompat(t *testing.T) {
	info, err := discovery.ParseCmdline("opencode --resume oc-456")
	if err != nil {
		t.Fatal(err)
	}
	if info.Provider != "opencode" {
		t.Errorf("expected provider opencode, got %s", info.Provider)
	}
	if info.ExternalSessionID != "oc-456" {
		t.Errorf("expected external session id oc-456, got %s", info.ExternalSessionID)
	}
}

func TestParseProcessInfo_OpenCodeEqualsFlags(t *testing.T) {
	info, err := discovery.ParseCmdline("opencode --resume=oc-789 --session=oc-999")
	if err != nil {
		t.Fatal(err)
	}
	if info.Provider != "opencode" {
		t.Errorf("expected provider opencode, got %s", info.Provider)
	}
	if info.ExternalSessionID != "oc-999" {
		t.Errorf("expected external session id oc-999, got %s", info.ExternalSessionID)
	}
}

func TestParseProcessInfo_ClaudeEqualsFlags(t *testing.T) {
	info, err := discovery.ParseCmdline("claude --resume=cl-111 --session-id=cl-222")
	if err != nil {
		t.Fatal(err)
	}
	if info.Provider != "claude" {
		t.Errorf("expected provider claude, got %s", info.Provider)
	}
	if info.ExternalSessionID != "cl-111" {
		t.Errorf("expected external session id cl-111, got %s", info.ExternalSessionID)
	}
	if info.ClaudeID != "cl-111" {
		t.Errorf("expected claude id cl-111, got %s", info.ClaudeID)
	}
}

func TestParseProcessInfo_NotClaude(t *testing.T) {
	_, err := discovery.ParseCmdline("/usr/bin/vim somefile.go")
	if err == nil {
		t.Error("expected error for unsupported process")
	}
}

func TestIsSupportedBinary(t *testing.T) {
	tests := []struct {
		path   string
		expect bool
	}{
		{"/usr/local/bin/claude", true},
		{"/home/user/.npm/bin/claude", true},
		{"/opt/homebrew/bin/opencode", true},
		{"opencode", true},
		{"/usr/bin/vim", false},
		{"claude", true},
	}
	for _, tt := range tests {
		if got := discovery.IsSupportedBinary(tt.path); got != tt.expect {
			t.Errorf("IsSupportedBinary(%q) = %v, want %v", tt.path, got, tt.expect)
		}
	}
}
