package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test config init creates the config file.
// Validates: AC-1760 (REQ-1121 - global configuration is persisted in config.json)
func TestFileStoreInitCreatesConfigFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewFileStore(filepath.Join(dir, "config.json"))

	cfg, created, err := store.Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if !created {
		t.Fatalf("Init() created = false, want true")
	}
	if cfg.Runtime.ReloadMode != defaultRuntimeReloadMode {
		t.Fatalf("Init() runtime reload mode = %q, want %q", cfg.Runtime.ReloadMode, defaultRuntimeReloadMode)
	}

	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.IsDir() {
		t.Fatalf("config.json is a directory")
	}
}

// Test set-base-workspace normalizes and persists the configured workspace path.
// Validates: AC-1761 (REQ-1122 - base workspace path is updated, shown, and validated)
func TestFileStoreSetBaseWorkspacePathPersistsNormalizedAbsolutePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	store := NewFileStore(filepath.Join(t.TempDir(), "config.json"))

	cfg, err := store.SetBaseWorkspacePath(context.Background(), "~/projects")
	if err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	want := filepath.Join(home, "projects")
	if cfg.BaseWorkspacePath != want {
		t.Fatalf("SetBaseWorkspacePath() base path = %q, want %q", cfg.BaseWorkspacePath, want)
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.BaseWorkspacePath != want {
		t.Fatalf("Load() base path = %q, want %q", loaded.BaseWorkspacePath, want)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

// Test config show redacts Slack secrets from the rendered configuration.
// Validates: AC-1763 (REQ-1125 - secrets are not printed in plaintext)
func TestGlobalConfigRedactedMasksSlackSecrets(t *testing.T) {
	t.Parallel()

	cfg := GlobalConfig{
		BaseWorkspacePath: "/workspace",
		Slack: SlackAuth{
			BotToken:    "xoxb-secret",
			AppToken:    "xapp-secret",
			WorkspaceID: "T123",
		},
	}

	redacted := cfg.Redacted()
	if redacted.Slack.BotToken != redactedValue {
		t.Fatalf("Redacted() bot token = %q, want %q", redacted.Slack.BotToken, redactedValue)
	}
	if redacted.Slack.AppToken != redactedValue {
		t.Fatalf("Redacted() app token = %q, want %q", redacted.Slack.AppToken, redactedValue)
	}
	if redacted.Slack.WorkspaceID != cfg.Slack.WorkspaceID {
		t.Fatalf("Redacted() workspace ID = %q, want %q", redacted.Slack.WorkspaceID, cfg.Slack.WorkspaceID)
	}

	rendered, err := encodeConfig(redacted)
	if err != nil {
		t.Fatalf("encodeConfig() error = %v", err)
	}
	if strings.Contains(string(rendered), "xoxb-secret") || strings.Contains(string(rendered), "xapp-secret") {
		t.Fatalf("rendered config leaked secrets: %s", rendered)
	}
}

// Test config validation rejects missing workspace paths and malformed structure.
// Validates: AC-1765 (REQ-1126 - validation removes bad persisted auth/config state from active use)
func TestFileStoreValidateRejectsInvalidStructureAndBasePath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewFileStore(filepath.Join(dir, "config.json"))

	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"baseWorkspacePath":"/workspace","unexpected":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := store.Load(context.Background()); err == nil {
		t.Fatalf("Load() error = nil, want structure validation failure")
	}

	if err := store.Save(context.Background(), DefaultGlobalConfig()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if _, err := store.Validate(context.Background()); err == nil {
		t.Fatalf("Validate() error = nil, want workspace validation failure")
	}
}

// Test config parsing rejects unknown fields and trailing content before invalid state can be used.
// Validates: AC-1765 (REQ-1126 - validation removes bad persisted auth/config state from active use)
func TestFileStoreLoadRejectsUnknownFieldsAndTrailingContent(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{
			name: "unknown-field",
			data: `{"baseWorkspacePath":"/workspace","unexpected":true}`,
		},
		{
			name: "trailing-content",
			data: `{"baseWorkspacePath":"/workspace"} garbage`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			store := NewFileStore(filepath.Join(dir, "config.json"))
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.data), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			if _, err := store.Load(context.Background()); err == nil {
				t.Fatalf("Load() error = nil, want parse failure for %s", tc.name)
			}
		})
	}
}
