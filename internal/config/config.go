package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type GlobalConfig struct {
	BaseWorkspacePath string         `json:"baseWorkspacePath,omitempty"`
	Slack             SlackAuth      `json:"slack,omitempty"`
	Runtime           RuntimeOptions `json:"runtime,omitempty"`
}

type SlackAuth struct {
	BotToken    string `json:"botToken,omitempty"`
	AppToken    string `json:"appToken,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

type SlackAuthStatus struct {
	Configured  bool   `json:"configured"`
	BotTokenSet bool   `json:"botTokenSet"`
	AppTokenSet bool   `json:"appTokenSet"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

type RuntimeOptions struct {
	ReloadMode string `json:"reloadMode,omitempty"`
	LogLevel   string `json:"logLevel,omitempty"`
}

const (
	defaultRuntimeReloadMode = "auto"
	defaultRuntimeLogLevel   = "info"
	redactedValue            = "<redacted>"
	configDirName            = ".config"
	configAppDirName         = "spexus-agent"
	configFileName           = "config.json"
)

var ErrConfigNotFound = errors.New("config file not found")

type Store interface {
	Load(context.Context) (GlobalConfig, error)
	Save(context.Context, GlobalConfig) error
}

type FileStore struct {
	Path string
}

func NewFileStore(path string) *FileStore {
	return &FileStore{Path: path}
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, configDirName, configAppDirName, configFileName), nil
}

func DefaultGlobalConfig() GlobalConfig {
	return GlobalConfig{
		Runtime: RuntimeOptions{
			ReloadMode: defaultRuntimeReloadMode,
			LogLevel:   defaultRuntimeLogLevel,
		},
	}
}

func (c GlobalConfig) Redacted() GlobalConfig {
	redacted := c
	if redacted.Slack.BotToken != "" {
		redacted.Slack.BotToken = redactedValue
	}
	if redacted.Slack.AppToken != "" {
		redacted.Slack.AppToken = redactedValue
	}
	return redacted
}

func (a SlackAuth) Status() SlackAuthStatus {
	return SlackAuthStatus{
		Configured:  a.BotToken != "" && a.AppToken != "" && a.WorkspaceID != "",
		BotTokenSet: a.BotToken != "",
		AppTokenSet: a.AppToken != "",
		WorkspaceID: a.WorkspaceID,
	}
}

func (c GlobalConfig) Validate() error {
	_, err := NormalizeBaseWorkspacePath(c.BaseWorkspacePath)
	return err
}

func NormalizeBaseWorkspacePath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", errors.New("base_workspace_path is required")
	}

	expanded, err := expandHomePrefix(trimmed)
	if err != nil {
		return "", err
	}

	if !filepath.IsAbs(expanded) {
		expanded, err = filepath.Abs(expanded)
		if err != nil {
			return "", fmt.Errorf("resolve base_workspace_path: %w", err)
		}
	}

	return filepath.Clean(expanded), nil
}

func (s *FileStore) Load(ctx context.Context) (GlobalConfig, error) {
	if err := ctx.Err(); err != nil {
		return GlobalConfig{}, err
	}

	path, err := s.resolvePath()
	if err != nil {
		return GlobalConfig{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GlobalConfig{}, ErrConfigNotFound
		}
		return GlobalConfig{}, fmt.Errorf("read config.json: %w", err)
	}

	return decodeConfig(data)
}

func (s *FileStore) Save(ctx context.Context, cfg GlobalConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	path, err := s.resolvePath()
	if err != nil {
		return err
	}

	if err := ensureConfigDir(filepath.Dir(path)); err != nil {
		return err
	}

	data, err := encodeConfig(cfg)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create config temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set config temp file permissions: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config.json: %w", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set config permissions: %w", err)
	}

	return nil
}

func (s *FileStore) Init(ctx context.Context) (GlobalConfig, bool, error) {
	path, err := s.resolvePath()
	if err != nil {
		return GlobalConfig{}, false, err
	}

	if _, err := os.Stat(path); err == nil {
		cfg, loadErr := s.Load(ctx)
		return cfg, false, loadErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return GlobalConfig{}, false, fmt.Errorf("stat config.json: %w", err)
	}

	cfg := DefaultGlobalConfig()
	if err := s.Save(ctx, cfg); err != nil {
		return GlobalConfig{}, false, err
	}

	return cfg, true, nil
}

func (s *FileStore) SetBaseWorkspacePath(ctx context.Context, workspacePath string) (GlobalConfig, error) {
	cfg, err := s.LoadOrDefault(ctx)
	if err != nil {
		return GlobalConfig{}, err
	}

	normalized, err := NormalizeBaseWorkspacePath(workspacePath)
	if err != nil {
		return GlobalConfig{}, err
	}

	cfg.BaseWorkspacePath = normalized
	if err := s.Save(ctx, cfg); err != nil {
		return GlobalConfig{}, err
	}

	return cfg, nil
}

func (s *FileStore) SetSlackAuth(ctx context.Context, auth SlackAuth) (GlobalConfig, error) {
	cfg, err := s.LoadOrDefault(ctx)
	if err != nil {
		return GlobalConfig{}, err
	}

	cfg.Slack = auth
	if err := s.Save(ctx, cfg); err != nil {
		return GlobalConfig{}, err
	}

	return cfg, nil
}

func (s *FileStore) ClearSlackAuth(ctx context.Context) (GlobalConfig, error) {
	cfg, err := s.LoadOrDefault(ctx)
	if err != nil {
		return GlobalConfig{}, err
	}

	cfg.Slack = SlackAuth{}
	if err := s.Save(ctx, cfg); err != nil {
		return GlobalConfig{}, err
	}

	return cfg, nil
}

func (s *FileStore) Validate(ctx context.Context) (GlobalConfig, error) {
	cfg, err := s.Load(ctx)
	if err != nil {
		return GlobalConfig{}, err
	}

	if err := cfg.Validate(); err != nil {
		return GlobalConfig{}, err
	}

	return cfg, nil
}

func (s *FileStore) LoadOrDefault(ctx context.Context) (GlobalConfig, error) {
	cfg, err := s.Load(ctx)
	if err == nil {
		return cfg, nil
	}

	if errors.Is(err, ErrConfigNotFound) {
		return DefaultGlobalConfig(), nil
	}

	return GlobalConfig{}, err
}

func (s *FileStore) resolvePath() (string, error) {
	if s != nil && strings.TrimSpace(s.Path) != "" {
		return s.Path, nil
	}

	return DefaultPath()
}

func decodeConfig(data []byte) (GlobalConfig, error) {
	var cfg GlobalConfig

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&cfg); err != nil {
		return GlobalConfig{}, fmt.Errorf("parse config.json: %w", err)
	}

	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return GlobalConfig{}, errors.New("parse config.json: unexpected trailing content")
		}
		return GlobalConfig{}, fmt.Errorf("parse config.json: %w", err)
	}

	return cfg, nil
}

func encodeConfig(cfg GlobalConfig) ([]byte, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode config.json: %w", err)
	}

	return append(data, '\n'), nil
}

func ensureConfigDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set config directory permissions: %w", err)
	}

	return nil
}

func expandHomePrefix(path string) (string, error) {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return home, nil
	}

	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, path[2:]), nil
	}

	return path, nil
}
