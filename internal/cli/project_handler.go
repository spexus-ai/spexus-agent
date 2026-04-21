package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/registry"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/storage"
)

type projectChannelProvisioner interface {
	ProvisionProjectChannel(context.Context, string) (slack.Channel, error)
}

type configBackedProjectChannelProvisioner struct {
	store *config.FileStore
}

func (p configBackedProjectChannelProvisioner) ProvisionProjectChannel(ctx context.Context, projectName string) (slack.Channel, error) {
	if p.store == nil {
		return slack.Channel{}, errors.New("config store unavailable")
	}

	cfg, err := p.store.LoadOrDefault(ctx)
	if err != nil {
		return slack.Channel{}, err
	}
	if strings.TrimSpace(cfg.Slack.BotToken) == "" {
		return slack.Channel{}, errors.New("slack bot token is required for project channel provisioning")
	}

	client := slack.NewHTTPClient(cfg.Slack.BotToken)
	defer func() {
		_ = client.Close()
	}()

	channelName := slack.ProjectChannelName(projectName)
	if channelName == "" {
		return slack.Channel{}, errors.New("project name is required")
	}

	channel, err := client.CreateChannel(ctx, slack.CreateChannelRequest{Name: channelName})
	if err != nil {
		return slack.Channel{}, err
	}
	if channel.Name == "" {
		channel.Name = channelName
	}
	if channel.Name != channelName {
		return slack.Channel{}, fmt.Errorf("slack channel name mismatch")
	}
	if channel.ID == "" {
		return slack.Channel{}, errors.New("slack channel id is required")
	}
	return channel, nil
}

type projectCommandHandler struct {
	configStore        *config.FileStore
	channelProvisioner projectChannelProvisioner
	out                io.Writer
}

func newProjectCommandHandler(out io.Writer) ProjectHandler {
	store := config.NewFileStore("")
	return &projectCommandHandler{
		configStore:        store,
		channelProvisioner: configBackedProjectChannelProvisioner{store: store},
		out:                out,
	}
}

func (h *projectCommandHandler) ImportRemote(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("project import-remote requires exactly one git remote argument")
	}

	configStore := h.configStore
	if configStore == nil {
		configStore = config.NewFileStore("")
	}

	gitRemote := strings.TrimSpace(args[0])
	projectName, err := projectNameFromGitRemote(gitRemote)
	if err != nil {
		return err
	}

	cfg, err := configStore.LoadOrDefault(ctx)
	if err != nil {
		return err
	}

	baseWorkspacePath, err := config.NormalizeBaseWorkspacePath(cfg.BaseWorkspacePath)
	if err != nil {
		return err
	}

	project := registry.Project{
		Name:      projectName,
		GitRemote: gitRemote,
		LocalPath: filepath.Join(baseWorkspacePath, projectName),
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = store.Close()
	}()

	repo := store.Projects()
	if repo == nil {
		return errors.New("project repository unavailable")
	}

	project, err = h.provisionProjectChannelWithRegistry(ctx, repo, project)
	if err != nil {
		return err
	}

	if err := repo.Upsert(ctx, project); err != nil {
		return err
	}

	out := h.out
	if out == nil {
		out = io.Discard
	}

	_, err = fmt.Fprintf(out, "imported project %s at %s\n", project.Name, project.LocalPath)
	return err
}

func (h *projectCommandHandler) List(ctx context.Context, args []string) error {
	return h.list(ctx, args)
}

func (h *projectCommandHandler) Show(context.Context, []string) error {
	return scaffolded(h.projectOut(), "project show")
}

func (h *projectCommandHandler) Delete(context.Context, []string) error {
	return scaffolded(h.projectOut(), "project delete")
}

func (h *projectCommandHandler) list(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("project list does not accept arguments")
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = store.Close()
	}()

	repo := store.Projects()
	if repo == nil {
		return errors.New("project repository unavailable")
	}

	projects, err := repo.List(ctx)
	if err != nil {
		return err
	}

	return writeJSON(h.projectOut(), projects)
}

func projectNameFromGitRemote(gitRemote string) (string, error) {
	trimmed := strings.TrimSpace(gitRemote)
	if trimmed == "" {
		return "", errors.New("git remote is required")
	}

	repoPath, err := gitRemoteRepositoryPath(trimmed)
	if err != nil {
		return "", err
	}

	repoPath = strings.TrimSpace(strings.TrimSuffix(repoPath, "/"))
	if repoPath == "" {
		return "", fmt.Errorf("invalid git remote %q: repository path is required", gitRemote)
	}

	name := strings.TrimSpace(path.Base(repoPath))
	name = strings.TrimSuffix(name, ".git")
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid git remote %q: repository name is required", gitRemote)
	}
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("invalid git remote %q: repository name %q is not valid", gitRemote, name)
	}

	return name, nil
}

func gitRemoteRepositoryPath(gitRemote string) (string, error) {
	if strings.Contains(gitRemote, "://") {
		parsed, err := url.Parse(gitRemote)
		if err != nil {
			return "", fmt.Errorf("invalid git remote %q: %w", gitRemote, err)
		}
		if strings.TrimSpace(parsed.Path) == "" {
			return "", fmt.Errorf("invalid git remote %q: repository path is required", gitRemote)
		}
		return parsed.Path, nil
	}

	if colon := strings.LastIndex(gitRemote, ":"); colon > 0 && strings.Contains(gitRemote[:colon], "@") {
		return gitRemote[colon+1:], nil
	}

	if strings.Contains(gitRemote, "/") || strings.HasPrefix(gitRemote, ".") || strings.HasPrefix(gitRemote, "~") {
		return gitRemote, nil
	}

	return "", fmt.Errorf("invalid git remote %q: repository path is required", gitRemote)
}

func (h *projectCommandHandler) projectOut() io.Writer {
	if h != nil && h.out != nil {
		return h.out
	}
	return io.Discard
}

func (h *projectCommandHandler) ImportLocal(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("project import-local requires exactly one local path argument")
	}

	localPath := strings.TrimSpace(args[0])
	if localPath == "" {
		return errors.New("local path is required")
	}

	info, err := os.Stat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("local path %q does not exist", localPath)
		}
		return fmt.Errorf("stat local path %q: %w", localPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local path %q is not a directory", localPath)
	}

	if err := ensureGitRepository(ctx, localPath); err != nil {
		return err
	}

	gitRemote, hasRemote, err := localRepositoryGitRemote(ctx, localPath)
	if err != nil {
		return err
	}

	projectName := filepath.Base(filepath.Clean(localPath))
	if hasRemote {
		projectName, err = projectNameFromGitRemote(gitRemote)
		if err != nil {
			return err
		}
	}
	if projectName == "" || projectName == "." || projectName == string(filepath.Separator) {
		return fmt.Errorf("invalid local path %q: project name is required", localPath)
	}

	project := registry.Project{
		Name:      projectName,
		GitRemote: gitRemote,
		LocalPath: localPath,
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = store.Close()
	}()

	repo := store.Projects()
	if repo == nil {
		return errors.New("project repository unavailable")
	}

	project, err = h.provisionProjectChannelWithRegistry(ctx, repo, project)
	if err != nil {
		return err
	}

	if err := repo.Upsert(ctx, project); err != nil {
		return err
	}

	out := h.out
	if out == nil {
		out = io.Discard
	}

	_, err = fmt.Fprintf(out, "imported local project %s at %s\n", project.Name, project.LocalPath)
	return err
}

func ensureGitRepository(ctx context.Context, localPath string) error {
	ok, err := gitBool(ctx, localPath, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf("validate git repository at %q: %w", localPath, err)
	}
	if !ok {
		return fmt.Errorf("local path %q is not a git repository", localPath)
	}
	return nil
}

func localRepositoryGitRemote(ctx context.Context, localPath string) (string, bool, error) {
	remotes, err := gitLines(ctx, localPath, "remote")
	if err != nil {
		return "", false, fmt.Errorf("list git remotes at %q: %w", localPath, err)
	}
	if len(remotes) == 0 {
		return "", false, nil
	}

	remoteName := remotes[0]
	for _, candidate := range remotes {
		if candidate == "origin" {
			remoteName = candidate
			break
		}
	}

	gitRemote, err := gitOutput(ctx, localPath, "remote", "get-url", remoteName)
	if err != nil {
		return "", false, fmt.Errorf("resolve git remote %q at %q: %w", remoteName, localPath, err)
	}

	return strings.TrimSpace(gitRemote), true, nil
}

func gitBool(ctx context.Context, localPath string, args ...string) (bool, error) {
	output, err := gitOutput(ctx, localPath, args...)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(output), "true"), nil
}

func gitLines(ctx context.Context, localPath string, args ...string) ([]string, error) {
	output, err := gitOutput(ctx, localPath, args...)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(output, "\n")
	remotes := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			remotes = append(remotes, trimmed)
		}
	}
	return remotes, nil
}

func gitOutput(ctx context.Context, localPath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", localPath}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed != "" {
			return "", fmt.Errorf("%s: %w", trimmed, err)
		}
		return "", err
	}
	return string(output), nil
}

func (h *projectCommandHandler) provisionProjectChannel(ctx context.Context, project registry.Project) (registry.Project, error) {
	if h == nil || h.channelProvisioner == nil {
		return registry.Project{}, errors.New("slack channel provisioner unavailable")
	}

	channel, err := h.channelProvisioner.ProvisionProjectChannel(ctx, project.Name)
	if err != nil {
		return registry.Project{}, err
	}

	expectedName := slack.ProjectChannelName(project.Name)
	if channel.Name == "" {
		channel.Name = expectedName
	}
	if channel.Name != expectedName {
		return registry.Project{}, fmt.Errorf("slack channel name mismatch: got %q, want %q", channel.Name, expectedName)
	}
	if channel.ID == "" {
		return registry.Project{}, errors.New("slack channel id is required")
	}

	project.SlackChannelName = channel.Name
	project.SlackChannelID = channel.ID
	return project, nil
}

func (h *projectCommandHandler) provisionProjectChannelWithRegistry(ctx context.Context, repo projectRegistryLookup, project registry.Project) (registry.Project, error) {
	original := project
	project, err := h.provisionProjectChannel(ctx, project)
	if err == nil {
		return project, nil
	}
	if !errors.Is(err, slack.ErrChannelNameTaken) {
		return registry.Project{}, err
	}

	expectedName := slack.ProjectChannelName(original.Name)
	existing, lookupErr := repo.Get(ctx, original.Name)
	if lookupErr != nil {
		if errors.Is(lookupErr, registry.ErrNotFound) {
			return registry.Project{}, fmt.Errorf("slack channel %q already exists but project %q is not registered", expectedName, original.Name)
		}
		return registry.Project{}, lookupErr
	}
	if existing.SlackChannelName != expectedName {
		return registry.Project{}, fmt.Errorf("registered project %q has slack channel %q, want %q", original.Name, existing.SlackChannelName, expectedName)
	}
	if existing.SlackChannelID == "" {
		return registry.Project{}, fmt.Errorf("registered project %q is missing a Slack channel id", original.Name)
	}

	original.SlackChannelName = existing.SlackChannelName
	original.SlackChannelID = existing.SlackChannelID
	return original, nil
}

type projectRegistryLookup interface {
	Get(context.Context, string) (registry.Project, error)
}
