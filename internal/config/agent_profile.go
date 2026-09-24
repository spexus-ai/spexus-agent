package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AgentProfile is the text-configured identity and execution context of one runtime.
// Provider credentials belong to Pi's auth store or environment, not this profile.
type AgentProfile struct {
	ID               string   `json:"id"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	Thinking         string   `json:"thinking,omitempty"`
	SystemPrompt     string   `json:"systemPrompt,omitempty"`
	PromptFile       string   `json:"promptFile,omitempty"`
	Workspace        string   `json:"workspace"`
	SessionDirectory string   `json:"sessionDirectory,omitempty"`
	Tools            []string `json:"tools"`
	Extensions       []string `json:"extensions,omitempty"`
}

// Resolve interprets relative paths beside config.json, not the launcher cwd.
func (p AgentProfile) Resolve(configPath string) (AgentProfile, error) {
	base := filepath.Dir(configPath)
	resolve := func(value string) string {
		if !filepath.IsAbs(value) {
			return filepath.Join(base, value)
		}
		return filepath.Clean(value)
	}
	for name, value := range map[string]string{"id": p.ID, "provider": p.Provider, "model": p.Model, "workspace": p.Workspace} {
		if strings.TrimSpace(value) == "" {
			return p, fmt.Errorf("agent.%s is required in config.json", name)
		}
	}
	if strings.ContainsAny(p.ID, "/\\\x00") || p.ID == "." || p.ID == ".." {
		return p, errors.New("agent.id must be a simple name")
	}
	if p.SystemPrompt != "" && p.PromptFile != "" {
		return p, errors.New("configure only one of agent.systemPrompt and agent.promptFile")
	}
	if p.PromptFile != "" {
		p.PromptFile = resolve(p.PromptFile)
		data, err := os.ReadFile(p.PromptFile)
		if err != nil {
			return p, fmt.Errorf("read agent prompt: %w", err)
		}
		p.SystemPrompt = string(data)
	}
	if strings.TrimSpace(p.SystemPrompt) == "" {
		return p, errors.New("agent.systemPrompt or agent.promptFile is required")
	}
	p.Workspace = resolve(p.Workspace)
	info, err := os.Stat(p.Workspace)
	if err != nil {
		return p, fmt.Errorf("agent workspace: %w", err)
	}
	if !info.IsDir() {
		return p, errors.New("agent.workspace must be a directory")
	}
	if p.Thinking == "" {
		p.Thinking = "medium"
	}
	switch p.Thinking {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return p, errors.New("invalid agent.thinking level")
	}
	if p.SessionDirectory == "" {
		p.SessionDirectory = filepath.Join(base, "pi-sessions", p.ID)
	} else {
		p.SessionDirectory = resolve(p.SessionDirectory)
	}
	for i, path := range p.Extensions {
		p.Extensions[i] = resolve(path)
	}
	return p, nil
}
