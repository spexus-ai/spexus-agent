package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentProfileResolvesTextSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("You are the prototype agent."), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := (AgentProfile{ID: "prototype", Provider: "test", Model: "model", PromptFile: "prompt.md", Workspace: "workspace"}).Resolve(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Workspace != filepath.Join(dir, "workspace") || p.SystemPrompt != "You are the prototype agent." || p.SessionDirectory != filepath.Join(dir, "pi-sessions", "prototype") {
		t.Fatalf("unexpected resolved profile: %+v", p)
	}
}

func TestAgentProfileRejectsIncompleteOrAmbiguousConfiguration(t *testing.T) {
	base := AgentProfile{ID: "prototype", Provider: "test", Model: "model", SystemPrompt: "prompt", Workspace: t.TempDir()}
	for _, name := range []string{"id", "provider", "model", "workspace", "prompt", "both-prompts", "thinking", "path-id"} {
		t.Run(name, func(t *testing.T) {
			p := base
			switch name {
			case "id":
				p.ID = ""
			case "provider":
				p.Provider = ""
			case "model":
				p.Model = ""
			case "workspace":
				p.Workspace = ""
			case "prompt":
				p.SystemPrompt = ""
			case "both-prompts":
				p.PromptFile = "prompt.md"
			case "thinking":
				p.Thinking = "invalid"
			case "path-id":
				p.ID = "../escape"
			}
			if _, err := p.Resolve(filepath.Join(t.TempDir(), "config.json")); err == nil {
				t.Fatal("expected invalid profile to fail")
			}
		})
	}
}
