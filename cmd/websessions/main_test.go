package main

import (
	"sync"
	"testing"
	"time"

	"github.com/IgorDeo/claude-websessions/internal/store"
)

func TestSnoozeSessions_SetDeleteAndIsSnoozed(t *testing.T) {
	tracker := newSnoozeSessions()
	now := time.Now()

	tracker.Set("session-1", now.Add(2*time.Minute))
	if !tracker.IsSnoozed("session-1", now) {
		t.Fatalf("expected session to be snoozed")
	}

	tracker.Delete("session-1")
	if tracker.IsSnoozed("session-1", now) {
		t.Fatalf("expected session snooze to be cleared")
	}
}

func TestSnoozeSessions_ConcurrentAccess(t *testing.T) {
	tracker := newSnoozeSessions()
	now := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "session-" + string(rune('a'+(i%10)))
			tracker.Set(id, now.Add(time.Duration(i+1)*time.Second))
			_ = tracker.IsSnoozed(id, now)
			tracker.Delete(id)
		}(i)
	}
	wg.Wait()
}

func TestOfflineRestoreCreateOptions(t *testing.T) {
	tests := []struct {
		name                  string
		rec                   store.SessionRecord
		wantProvider          string
		wantExternalSessionID string
	}{
		{
			name: "provider aware record",
			rec: store.SessionRecord{
				Provider:          "opencode",
				ExternalSessionID: "oc-123",
				ClaudeID:          "",
			},
			wantProvider:          "opencode",
			wantExternalSessionID: "oc-123",
		},
		{
			name: "legacy claude record",
			rec: store.SessionRecord{
				Provider:          "",
				ExternalSessionID: "",
				ClaudeID:          "cl-legacy",
			},
			wantProvider:          "claude",
			wantExternalSessionID: "cl-legacy",
		},
		{
			name: "opencode does not fallback to claude id",
			rec: store.SessionRecord{
				Provider:          "opencode",
				ExternalSessionID: "",
				ClaudeID:          "cl-legacy",
			},
			wantProvider:          "opencode",
			wantExternalSessionID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := offlineRestoreCreateOptions(tt.rec)
			if opts.Provider != tt.wantProvider {
				t.Fatalf("provider mismatch: got %q want %q", opts.Provider, tt.wantProvider)
			}
			if opts.ExternalSessionID != tt.wantExternalSessionID {
				t.Fatalf("external session id mismatch: got %q want %q", opts.ExternalSessionID, tt.wantExternalSessionID)
			}
		})
	}
}

func TestStoredClaudeIDForProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		claudeID string
		want     string
	}{
		{name: "claude provider keeps claude id", provider: "claude", claudeID: "cl-1", want: "cl-1"},
		{name: "empty provider treated as claude", provider: "", claudeID: "cl-2", want: "cl-2"},
		{name: "non claude provider clears claude id", provider: "opencode", claudeID: "stale", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := storedClaudeIDForProvider(tt.provider, tt.claudeID)
			if got != tt.want {
				t.Fatalf("storedClaudeIDForProvider(%q, %q) = %q, want %q", tt.provider, tt.claudeID, got, tt.want)
			}
		})
	}
}
