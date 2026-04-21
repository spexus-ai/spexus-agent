package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spexus-ai/spexus-agent/internal/config"
)

type configCommandHandler struct {
	store  *config.FileStore
	in     io.Reader
	prompt *bufio.Reader
	out    io.Writer
}

func newConfigCommandHandler(out io.Writer, in io.Reader) ConfigHandler {
	return &configCommandHandler{
		store: config.NewFileStore(""),
		in:    in,
		out:   out,
	}
}

func (h *configCommandHandler) Init(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("config init does not accept arguments")
	}

	path, err := h.storePath()
	if err != nil {
		return err
	}

	_, created, err := h.store.Init(ctx)
	if err != nil {
		return err
	}

	if created {
		_, err = fmt.Fprintf(h.out, "created %s\n", path)
		return err
	}

	_, err = fmt.Fprintf(h.out, "config already exists at %s\n", path)
	return err
}

func (h *configCommandHandler) Show(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("config show does not accept arguments")
	}

	cfg, err := h.store.Load(ctx)
	if err != nil {
		return err
	}

	return writeJSON(h.out, cfg.Redacted())
}

func (h *configCommandHandler) Validate(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("config validate does not accept arguments")
	}

	if _, err := h.store.Validate(ctx); err != nil {
		return err
	}

	_, err := fmt.Fprintln(h.out, "config.json is valid")
	return err
}

func (h *configCommandHandler) SetBaseWorkspace(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("config set-base-workspace requires exactly one path argument")
	}

	cfg, err := h.store.SetBaseWorkspacePath(ctx, args[0])
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(h.out, "base_workspace_path updated to %s\n", cfg.BaseWorkspacePath)
	return err
}

func (h *configCommandHandler) Login(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("config slack-auth login does not accept arguments")
	}

	auth, err := h.promptSlackAuth()
	if err != nil {
		return err
	}

	cfg, err := h.store.SetSlackAuth(ctx, auth)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(h.out, "slack auth saved for workspace %s\n", cfg.Slack.WorkspaceID)
	return err
}

func (h *configCommandHandler) Status(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("config slack-auth status does not accept arguments")
	}

	cfg, err := h.store.LoadOrDefault(ctx)
	if err != nil {
		return err
	}

	return writeJSON(h.out, cfg.Slack.Status())
}

func (h *configCommandHandler) Logout(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("config slack-auth logout does not accept arguments")
	}

	if _, err := h.store.ClearSlackAuth(ctx); err != nil {
		return err
	}

	path, err := h.storePath()
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(h.out, "slack auth removed from %s\n", path)
	return err
}

func (h *configCommandHandler) storePath() (string, error) {
	if h != nil && h.store != nil && h.store.Path != "" {
		return h.store.Path, nil
	}

	return config.DefaultPath()
}

func writeJSON(out io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(out, "%s\n", data)
	return err
}

func (h *configCommandHandler) promptSlackAuth() (config.SlackAuth, error) {
	botToken, err := h.promptRequired("Slack bot token")
	if err != nil {
		return config.SlackAuth{}, err
	}

	appToken, err := h.promptRequired("Slack app token")
	if err != nil {
		return config.SlackAuth{}, err
	}

	workspaceID, err := h.promptRequired("Slack workspace ID")
	if err != nil {
		return config.SlackAuth{}, err
	}

	return config.SlackAuth{
		BotToken:    botToken,
		AppToken:    appToken,
		WorkspaceID: workspaceID,
	}, nil
}

func (h *configCommandHandler) promptRequired(label string) (string, error) {
	if h.prompt == nil {
		reader := h.in
		if reader == nil {
			reader = strings.NewReader("")
		}
		h.prompt = bufio.NewReader(reader)
	}

	if _, err := fmt.Fprintf(h.out, "%s: ", label); err != nil {
		return "", err
	}

	line, err := h.prompt.ReadString('\n')
	if err != nil && !errorsIsEOF(err) {
		return "", err
	}

	value := strings.TrimSpace(line)
	if value == "" {
		return "", fmt.Errorf("%s is required", strings.ToLower(label))
	}

	return value, nil
}

func errorsIsEOF(err error) bool {
	return err == io.EOF
}
