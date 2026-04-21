package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/registry"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/storage"
)

type fakeProjectHandler struct {
	importRemoteArgs []string
	listArgs         []string
}

func (f *fakeProjectHandler) ImportRemote(_ context.Context, args []string) error {
	f.importRemoteArgs = append([]string(nil), args...)
	return nil
}

func (f *fakeProjectHandler) ImportLocal(context.Context, []string) error { return nil }
func (f *fakeProjectHandler) List(_ context.Context, args []string) error {
	f.listArgs = append([]string(nil), args...)
	return nil
}
func (f *fakeProjectHandler) Show(context.Context, []string) error   { return nil }
func (f *fakeProjectHandler) Delete(context.Context, []string) error { return nil }

type projectChannelProvisionerFunc func(context.Context, string) (slack.Channel, error)

func (f projectChannelProvisionerFunc) ProvisionProjectChannel(ctx context.Context, projectName string) (slack.Channel, error) {
	return f(ctx, projectName)
}

// Test: project import-remote is routed through the CLI dispatcher with the remote argument intact.
// Validates: AC-1766 (REQ-1127 - derive project name from repository name), AC-1767 (REQ-1128 - derive local path from base workspace path)
func TestAppRunProjectImportRemoteRoutesHandler(t *testing.T) {
	handler := &fakeProjectHandler{}
	app := &App{
		Project: handler,
		Out:     &bytes.Buffer{},
		Err:     &bytes.Buffer{},
	}

	if code := app.Run([]string{"project", "import-remote", "git@github.com:wms-ai/backend-notification-app.git"}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0", code)
	}

	if got, want := strings.Join(handler.importRemoteArgs, ","), "git@github.com:wms-ai/backend-notification-app.git"; got != want {
		t.Fatalf("handler args = %q, want %q", got, want)
	}
}

// Test: project list is routed through the CLI dispatcher without extra arguments.
// Validates: AC-1769 (REQ-1130 - project CLI exposes a readable project registry surface)
func TestAppRunProjectListRoutesHandler(t *testing.T) {
	handler := &fakeProjectHandler{}
	app := &App{
		Project: handler,
		Out:     &bytes.Buffer{},
		Err:     &bytes.Buffer{},
	}

	if code := app.Run([]string{"project", "list"}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0", code)
	}

	if got, want := strings.Join(handler.listArgs, ","), ""; got != want {
		t.Fatalf("handler args = %q, want %q", got, want)
	}
}

// Test: project name resolution extracts repository names from common remote URL formats.
// Validates: AC-1766 (REQ-1127 - derive project name from repository name)
func TestProjectNameFromGitRemoteParsesCommonRemoteFormats(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"git@github.com:wms-ai/backend-notification-app.git": "backend-notification-app",
		"https://github.com/wms-ai/frontend-ui.git":          "frontend-ui",
		"ssh://git@github.com/wms-ai/platform":               "platform",
	}

	for remote, want := range cases {
		got, err := projectNameFromGitRemote(remote)
		if err != nil {
			t.Fatalf("projectNameFromGitRemote(%q) error = %v", remote, err)
		}
		if got != want {
			t.Fatalf("projectNameFromGitRemote(%q) = %q, want %q", remote, got, want)
		}
	}
}

// Test: project import-remote derives the project name and local path, then persists the registry entry.
// Validates: AC-1766 (REQ-1127 - derive project name from repository name), AC-1767 (REQ-1128 - local path uses BASE_WORKSPACE_PATH), AC-1769 (REQ-1130 - persist project registry entry in SQLite)
func TestProjectImportRemotePersistsRegistryEntry(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	var out bytes.Buffer
	handler := &projectCommandHandler{
		configStore: cfgStore,
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{ID: "C12345678", Name: "spexus-backend-notification-app"}, nil
		}),
		out: &out,
	}

	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	remote := "git@github.com:wms-ai/backend-notification-app.git"
	if code := app.Run([]string{"project", "import-remote", remote}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0; output=%s", code, out.String())
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project, err := store.Projects().Get(ctx, "backend-notification-app")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if project.Name != "backend-notification-app" {
		t.Fatalf("project name = %q, want %q", project.Name, "backend-notification-app")
	}
	if project.GitRemote != remote {
		t.Fatalf("project git remote = %q, want %q", project.GitRemote, remote)
	}
	if project.LocalPath != filepath.Join(workspacePath, "backend-notification-app") {
		t.Fatalf("project local path = %q, want %q", project.LocalPath, filepath.Join(workspacePath, "backend-notification-app"))
	}
	if project.SlackChannelName != "spexus-backend-notification-app" {
		t.Fatalf("project slack channel name = %q, want %q", project.SlackChannelName, "spexus-backend-notification-app")
	}
	if project.SlackChannelID != "C12345678" {
		t.Fatalf("project slack channel id = %q, want %q", project.SlackChannelID, "C12345678")
	}
}

// Test: project list returns the persisted registry snapshot as JSON.
// Validates: AC-1769 (REQ-1130 - persisted project registrations are readable through the project CLI)
func TestProjectListReturnsJSONRegistrySnapshot(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	for _, project := range []registry.Project{
		{
			Name:             "alpha",
			LocalPath:        filepath.Join(home, "workspace", "alpha"),
			SlackChannelName: "spexus-alpha",
			SlackChannelID:   "C111",
		},
		{
			Name:             "beta",
			LocalPath:        filepath.Join(home, "workspace", "beta"),
			SlackChannelName: "spexus-beta",
			SlackChannelID:   "C222",
		},
	} {
		if err := store.Projects().Upsert(ctx, project); err != nil {
			t.Fatalf("Upsert(%q) error = %v", project.Name, err)
		}
	}

	var out bytes.Buffer
	handler := &projectCommandHandler{out: &out}
	if err := handler.List(ctx, nil); err != nil {
		t.Fatalf("List() error = %v", err)
	}

	var projects []registry.Project
	if err := json.Unmarshal(out.Bytes(), &projects); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}

	if got, want := len(projects), 2; got != want {
		t.Fatalf("project count = %d, want %d", got, want)
	}
	if projects[0].Name != "alpha" || projects[1].Name != "beta" {
		t.Fatalf("projects = %#v, want alphabetically ordered registry snapshot", projects)
	}
}

// Test: project list rejects unexpected arguments.
// Validates: AC-1769 (REQ-1130 - project list has a stable bounded CLI contract)
func TestProjectListRejectsArguments(t *testing.T) {
	t.Parallel()

	handler := &projectCommandHandler{out: &bytes.Buffer{}}
	err := handler.List(context.Background(), []string{"unexpected"})
	if err == nil || !strings.Contains(err.Error(), "project list does not accept arguments") {
		t.Fatalf("List() error = %v, want argument rejection", err)
	}
}

// Test: project import-remote rejects invalid remote input before any registry entry is written.
// Validates: AC-1766 (REQ-1127 - fail-fast validation of the remote input), AC-1770 (REQ-1131 - no partial project registration on failure)
func TestProjectImportRemoteRejectsInvalidRemoteBeforePersistence(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, filepath.Join(home, "workspace")); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	var out bytes.Buffer
	handler := &projectCommandHandler{
		configStore: cfgStore,
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{ID: "C12345678", Name: "spexus-local-repo"}, nil
		}),
		out: &out,
	}

	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	if code := app.Run([]string{"project", "import-remote", "origin"}); code == 0 {
		t.Fatalf("Run() exit code = %d, want non-zero; output=%s", code, out.String())
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if _, err := store.Projects().Get(ctx, "origin"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Get() error = %v, want registry.ErrNotFound", err)
	}
}

// Test: project import-remote does not persist a registry entry if the Slack-binding boundary fails.
// Validates: AC-1770 (REQ-1131 - no partial project registration when the import flow fails)
func TestProjectImportRemoteDoesNotPersistOnBinderFailure(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, filepath.Join(home, "workspace")); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	var out bytes.Buffer
	handler := &projectCommandHandler{
		configStore: cfgStore,
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{}, errors.New("slack provisioning failed")
		}),
		out: &out,
	}

	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	remote := "git@github.com:wms-ai/backend-notification-app.git"
	if code := app.Run([]string{"project", "import-remote", remote}); code == 0 {
		t.Fatalf("Run() exit code = %d, want non-zero; output=%s", code, out.String())
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if _, err := store.Projects().Get(ctx, "backend-notification-app"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Get() error = %v, want registry.ErrNotFound", err)
	}
}

// Test: repeated project import-remote reuses the stored project registration when Slack returns name_taken.
// Validates: AC-1770 (REQ-1131 - repeated imports do not fail when the project channel already exists)
func TestProjectImportRemoteReusesRegisteredProjectOnNameTaken(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	existingProject := registry.Project{
		Name:             "backend-notification-app",
		GitRemote:        "git@github.com:old/backend-notification-app.git",
		LocalPath:        "/tmp/old/backend-notification-app",
		SlackChannelName: "spexus-backend-notification-app",
		SlackChannelID:   "C12345678",
	}
	if err := store.Projects().Upsert(ctx, existingProject); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var out bytes.Buffer
	handler := &projectCommandHandler{
		configStore: cfgStore,
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{}, slack.ErrChannelNameTaken
		}),
		out: &out,
	}
	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	remote := "git@github.com:wms-ai/backend-notification-app.git"
	if code := app.Run([]string{"project", "import-remote", remote}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0; output=%s", code, out.String())
	}

	store, err = storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project, err := store.Projects().Get(ctx, "backend-notification-app")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if project.GitRemote != remote {
		t.Fatalf("project git remote = %q, want %q", project.GitRemote, remote)
	}
	if project.LocalPath != filepath.Join(workspacePath, "backend-notification-app") {
		t.Fatalf("project local path = %q, want %q", project.LocalPath, filepath.Join(workspacePath, "backend-notification-app"))
	}
	if project.SlackChannelName != existingProject.SlackChannelName {
		t.Fatalf("project slack channel name = %q, want %q", project.SlackChannelName, existingProject.SlackChannelName)
	}
	if project.SlackChannelID != existingProject.SlackChannelID {
		t.Fatalf("project slack channel id = %q, want %q", project.SlackChannelID, existingProject.SlackChannelID)
	}
}

// Test: project import-local derives the project name from the Git remote and preserves the original local path.
// Validates: AC-1771 (REQ-1132 - local path exists), AC-1772 (REQ-1133 - path is a Git repository), AC-1773 (REQ-1134 - derive project name from Git remote), AC-1775 (REQ-1136 - preserve original local path), AC-1777 (REQ-1139 - create project registry entry)
func TestProjectImportLocalPersistsRegistryEntryWithRemoteName(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoDir := filepath.Join(home, "local-repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	initGitRepo(t, repoDir)
	remote := "git@github.com:wms-ai/local-repo.git"
	gitCommand(t, repoDir, "remote", "add", "origin", remote)

	var out bytes.Buffer
	handler := &projectCommandHandler{
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{ID: "C12345678", Name: "spexus-local-repo"}, nil
		}),
		out: &out,
	}
	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	if code := app.Run([]string{"project", "import-local", repoDir}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0; output=%s", code, out.String())
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project, err := store.Projects().Get(ctx, "local-repo")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if project.GitRemote != remote {
		t.Fatalf("project git remote = %q, want %q", project.GitRemote, remote)
	}
	if project.LocalPath != repoDir {
		t.Fatalf("project local path = %q, want %q", project.LocalPath, repoDir)
	}
	if project.SlackChannelName != "spexus-local-repo" {
		t.Fatalf("project slack channel name = %q, want %q", project.SlackChannelName, "spexus-local-repo")
	}
	if project.SlackChannelID != "C12345678" {
		t.Fatalf("project slack channel id = %q, want %q", project.SlackChannelID, "C12345678")
	}
}

// Test: repeated project import-local reuses the stored project registration when Slack returns name_taken.
// Validates: AC-1770 (REQ-1131 - repeated imports do not fail when the project channel already exists)
func TestProjectImportLocalReusesRegisteredProjectOnNameTaken(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoDir := filepath.Join(home, "local-repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	initGitRepo(t, repoDir)
	remote := "git@github.com:wms-ai/local-repo.git"
	gitCommand(t, repoDir, "remote", "add", "origin", remote)

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, filepath.Join(home, "workspace")); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	existingProject := registry.Project{
		Name:             "local-repo",
		GitRemote:        "git@github.com:old/local-repo.git",
		LocalPath:        "/tmp/old/local-repo",
		SlackChannelName: "spexus-local-repo",
		SlackChannelID:   "C12345678",
	}
	if err := store.Projects().Upsert(ctx, existingProject); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var out bytes.Buffer
	handler := &projectCommandHandler{
		configStore: cfgStore,
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{}, slack.ErrChannelNameTaken
		}),
		out: &out,
	}
	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	if code := app.Run([]string{"project", "import-local", repoDir}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0; output=%s", code, out.String())
	}

	store, err = storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project, err := store.Projects().Get(ctx, "local-repo")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if project.GitRemote != remote {
		t.Fatalf("project git remote = %q, want %q", project.GitRemote, remote)
	}
	if project.LocalPath != repoDir {
		t.Fatalf("project local path = %q, want %q", project.LocalPath, repoDir)
	}
	if project.SlackChannelName != existingProject.SlackChannelName {
		t.Fatalf("project slack channel name = %q, want %q", project.SlackChannelName, existingProject.SlackChannelName)
	}
	if project.SlackChannelID != existingProject.SlackChannelID {
		t.Fatalf("project slack channel id = %q, want %q", project.SlackChannelID, existingProject.SlackChannelID)
	}
}

// Test: project import-local falls back to the directory basename when the repository has no Git remote.
// Validates: AC-1771 (REQ-1132 - local path exists), AC-1772 (REQ-1133 - path is a Git repository), AC-1774 (REQ-1135 - fall back to directory basename), AC-1775 (REQ-1136 - preserve original local path), AC-1777 (REQ-1139 - create project registry entry)
func TestProjectImportLocalFallsBackToDirectoryBasenameWithoutRemote(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoDir := filepath.Join(home, "fallback-name")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	initGitRepo(t, repoDir)

	var out bytes.Buffer
	handler := &projectCommandHandler{
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{ID: "C12345678", Name: "spexus-fallback-name"}, nil
		}),
		out: &out,
	}
	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	if code := app.Run([]string{"project", "import-local", repoDir}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0; output=%s", code, out.String())
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project, err := store.Projects().Get(ctx, "fallback-name")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if project.GitRemote != "" {
		t.Fatalf("project git remote = %q, want empty", project.GitRemote)
	}
	if project.LocalPath != repoDir {
		t.Fatalf("project local path = %q, want %q", project.LocalPath, repoDir)
	}
	if project.SlackChannelName != "spexus-fallback-name" {
		t.Fatalf("project slack channel name = %q, want %q", project.SlackChannelName, "spexus-fallback-name")
	}
	if project.SlackChannelID != "C12345678" {
		t.Fatalf("project slack channel id = %q, want %q", project.SlackChannelID, "C12345678")
	}
}

// Test: project import-local rejects a missing local path before any registry entry is written.
// Validates: AC-1771 (REQ-1132 - local path must exist), AC-1779 (REQ-1140 - reject partial registration on failure)
func TestProjectImportLocalRejectsMissingPathBeforePersistence(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	var out bytes.Buffer
	handler := &projectCommandHandler{
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{ID: "C12345678", Name: "spexus-binder-failure"}, nil
		}),
		out: &out,
	}
	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	missingPath := filepath.Join(home, "missing-repo")
	if code := app.Run([]string{"project", "import-local", missingPath}); code == 0 {
		t.Fatalf("Run() exit code = %d, want non-zero; output=%s", code, out.String())
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	projects, err := store.Projects().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("List() len = %d, want 0", len(projects))
	}
}

// Test: project import-local rejects a non-Git directory before any registry entry is written.
// Validates: AC-1772 (REQ-1133 - path must be a Git repository), AC-1779 (REQ-1140 - reject partial registration on failure)
func TestProjectImportLocalRejectsNonGitRepositoryBeforePersistence(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoDir := filepath.Join(home, "not-a-git-repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	var out bytes.Buffer
	handler := &projectCommandHandler{
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{ID: "C12345678", Name: "spexus-not-a-git-repo"}, nil
		}),
		out: &out,
	}
	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	if code := app.Run([]string{"project", "import-local", repoDir}); code == 0 {
		t.Fatalf("Run() exit code = %d, want non-zero; output=%s", code, out.String())
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	projects, err := store.Projects().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("List() len = %d, want 0", len(projects))
	}
}

// Test: project import-local does not persist a registry entry if the Slack-binding boundary fails.
// Validates: AC-1779 (REQ-1140 - reject partial registration on failure)
func TestProjectImportLocalDoesNotPersistOnBinderFailure(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoDir := filepath.Join(home, "binder-failure")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	initGitRepo(t, repoDir)

	var out bytes.Buffer
	handler := &projectCommandHandler{
		channelProvisioner: projectChannelProvisionerFunc(func(context.Context, string) (slack.Channel, error) {
			return slack.Channel{}, errors.New("slack provisioning failed")
		}),
		out: &out,
	}
	app := &App{
		Project: handler,
		Out:     &out,
		Err:     &out,
	}

	if code := app.Run([]string{"project", "import-local", repoDir}); code == 0 {
		t.Fatalf("Run() exit code = %d, want non-zero; output=%s", code, out.String())
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	projects, err := store.Projects().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("List() len = %d, want 0", len(projects))
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	gitCommand(t, dir, "init")
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\noutput=%s", args, err, string(output))
	}

	return strings.TrimSpace(string(output))
}
