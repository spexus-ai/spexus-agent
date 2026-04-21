package config

import (
	"context"
	"path/filepath"
	"testing"
)

// Test Slack auth persistence and removal through the config store.
// Validates: AC-1764 (REQ-1124 - Slack auth is persisted in config.json) and AC-1765 (REQ-1126 - logout removes Slack auth from config.json)
func TestFileStoreSetAndClearSlackAuth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewFileStore(filepath.Join(dir, "config.json"))

	workspacePath := filepath.Join(dir, "workspace")
	if _, err := store.SetBaseWorkspacePath(context.Background(), workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	wantAuth := SlackAuth{
		BotToken:    "xoxb-secret",
		AppToken:    "xapp-secret",
		WorkspaceID: "T123",
	}
	cfg, err := store.SetSlackAuth(context.Background(), wantAuth)
	if err != nil {
		t.Fatalf("SetSlackAuth() error = %v", err)
	}
	if cfg.Slack != wantAuth {
		t.Fatalf("SetSlackAuth() slack auth = %#v, want %#v", cfg.Slack, wantAuth)
	}
	if cfg.BaseWorkspacePath != workspacePath {
		t.Fatalf("SetSlackAuth() base workspace path = %q, want %q", cfg.BaseWorkspacePath, workspacePath)
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Slack != wantAuth {
		t.Fatalf("Load() slack auth = %#v, want %#v", loaded.Slack, wantAuth)
	}

	cfg, err = store.ClearSlackAuth(context.Background())
	if err != nil {
		t.Fatalf("ClearSlackAuth() error = %v", err)
	}
	if cfg.Slack != (SlackAuth{}) {
		t.Fatalf("ClearSlackAuth() slack auth = %#v, want zero value", cfg.Slack)
	}
	if cfg.BaseWorkspacePath != workspacePath {
		t.Fatalf("ClearSlackAuth() base workspace path = %q, want %q", cfg.BaseWorkspacePath, workspacePath)
	}

	loaded, err = store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Slack != (SlackAuth{}) {
		t.Fatalf("Load() slack auth = %#v, want zero value", loaded.Slack)
	}
	if loaded.BaseWorkspacePath != workspacePath {
		t.Fatalf("Load() base workspace path = %q, want %q", loaded.BaseWorkspacePath, workspacePath)
	}
}
