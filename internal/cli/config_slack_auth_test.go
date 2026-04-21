package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/config"
)

// Test Slack auth login prompts for secrets, persists them, and status redacts them from output.
// Validates: AC-1762 (REQ-1123 - interactive Slack auth collection) and AC-1763 (REQ-1125 - status/show do not print secrets)
func TestConfigSlackAuthLoginStatusAndLogout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := config.NewFileStore(filepath.Join(dir, "config.json"))

	var out bytes.Buffer
	handler := &configCommandHandler{
		store: store,
		in:    strings.NewReader("xoxb-secret\nxapp-secret\nT123\n"),
		out:   &out,
	}

	if err := handler.Login(context.Background(), nil); err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	wantAuth := config.SlackAuth{
		BotToken:    "xoxb-secret",
		AppToken:    "xapp-secret",
		WorkspaceID: "T123",
	}
	if loaded.Slack != wantAuth {
		t.Fatalf("Load() slack auth = %#v, want %#v", loaded.Slack, wantAuth)
	}

	out.Reset()
	if err := handler.Status(context.Background(), nil); err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	statusOutput := out.String()
	if strings.Contains(statusOutput, "xoxb-secret") || strings.Contains(statusOutput, "xapp-secret") {
		t.Fatalf("Status() leaked secrets: %s", statusOutput)
	}
	if !strings.Contains(statusOutput, "\"configured\": true") {
		t.Fatalf("Status() output missing configured state: %s", statusOutput)
	}
	if !strings.Contains(statusOutput, "\"workspaceId\": \"T123\"") {
		t.Fatalf("Status() output missing workspace ID: %s", statusOutput)
	}

	out.Reset()
	if err := handler.Logout(context.Background(), nil); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}

	loaded, err = store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Slack != (config.SlackAuth{}) {
		t.Fatalf("Logout() slack auth = %#v, want zero value", loaded.Slack)
	}
}
