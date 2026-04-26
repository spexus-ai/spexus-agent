package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/registry"
	runtimemodel "github.com/spexus-ai/spexus-agent/internal/runtime"
)

// Test: SQLite storage bootstrap creates the storage file and registers the project mapping tables.
// Validates: AC-1783 (REQ-1144 - runtime initializes SQLite storage when absent), AC-1782 (REQ-1143 - runtime loads project registry from SQLite), AC-1769 (REQ-1130 - import-remote registers imported project in SQLite), AC-1778 (REQ-1139 - import-local persists project registry entry)
func TestOpenBootstrapsSchemaAndProjectRepository(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.sqlite3")

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}

	projectRepo := store.Projects()
	if projectRepo == nil {
		t.Fatalf("Projects() = nil")
	}

	wantProject := registry.Project{
		Name:             "alpha",
		GitRemote:        "git@github.com:org/alpha.git",
		LocalPath:        "/workspace/alpha",
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}
	if err := projectRepo.Upsert(ctx, wantProject); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	loadedByName, err := projectRepo.Get(ctx, wantProject.Name)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if loadedByName.Name != wantProject.Name || loadedByName.LocalPath != wantProject.LocalPath || loadedByName.SlackChannelID != wantProject.SlackChannelID {
		t.Fatalf("Get() project = %#v, want %#v", loadedByName, wantProject)
	}

	loadedByChannel, err := projectRepo.GetByChannelID(ctx, wantProject.SlackChannelID)
	if err != nil {
		t.Fatalf("GetByChannelID() error = %v", err)
	}
	if loadedByChannel.Name != wantProject.Name {
		t.Fatalf("GetByChannelID() project name = %q, want %q", loadedByChannel.Name, wantProject.Name)
	}

	projects, err := projectRepo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("List() len = %d, want 1", len(projects))
	}
}

// Test: project repository rejects writes that omit Slack channel metadata.
// Validates: AC-1779 (REQ-1140 - no partial project registration without Slack channel metadata)
func TestProjectRepositoryRejectsPartialSlackMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(ctx, filepath.Join(dir, "storage.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	err = store.Projects().Upsert(ctx, registry.Project{
		Name:      "alpha",
		LocalPath: "/workspace/alpha",
	})
	if err == nil {
		t.Fatalf("Upsert() error = nil, want non-nil")
	}
}

// Test: project registry CRUD persists, updates, lists, and deletes project records in SQLite.
// Validates: AC-1769 (REQ-1130 - persist project registry entry in SQLite)
func TestProjectRepositoryCRUD(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(ctx, filepath.Join(dir, "storage.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	projectRepo := store.Projects()
	if projectRepo == nil {
		t.Fatalf("Projects() = nil")
	}

	initial := registry.Project{
		Name:             "alpha",
		GitRemote:        "git@github.com:org/alpha.git",
		LocalPath:        "/workspace/alpha",
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}
	if err := projectRepo.Upsert(ctx, initial); err != nil {
		t.Fatalf("Upsert(initial) error = %v", err)
	}

	loaded, err := projectRepo.Get(ctx, initial.Name)
	if err != nil {
		t.Fatalf("Get(initial) error = %v", err)
	}
	if loaded.Name != initial.Name || loaded.LocalPath != initial.LocalPath || loaded.SlackChannelID != initial.SlackChannelID {
		t.Fatalf("Get(initial) = %#v, want %#v", loaded, initial)
	}

	updated := registry.Project{
		Name:             "alpha",
		GitRemote:        "git@github.com:org/alpha.git",
		LocalPath:        "/workspace/alpha-updated",
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C87654321",
	}
	if err := projectRepo.Upsert(ctx, updated); err != nil {
		t.Fatalf("Upsert(updated) error = %v", err)
	}

	loaded, err = projectRepo.Get(ctx, updated.Name)
	if err != nil {
		t.Fatalf("Get(updated) error = %v", err)
	}
	if loaded.LocalPath != updated.LocalPath || loaded.SlackChannelID != updated.SlackChannelID {
		t.Fatalf("Get(updated) = %#v, want %#v", loaded, updated)
	}

	byChannel, err := projectRepo.GetByChannelID(ctx, updated.SlackChannelID)
	if err != nil {
		t.Fatalf("GetByChannelID() error = %v", err)
	}
	if byChannel.Name != updated.Name || byChannel.LocalPath != updated.LocalPath {
		t.Fatalf("GetByChannelID() = %#v, want %#v", byChannel, updated)
	}

	projects, err := projectRepo.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("List() len = %d, want 1", len(projects))
	}

	if err := projectRepo.Delete(ctx, updated.Name); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := projectRepo.Get(ctx, updated.Name); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Get() error after delete = %v, want registry.ErrNotFound", err)
	}

	projects, err = projectRepo.List(ctx)
	if err != nil {
		t.Fatalf("List() error after delete = %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("List() len after delete = %d, want 0", len(projects))
	}
}

// Test: Runtime repository persists thread state, event dedupe metadata, and thread lock state in SQLite.
// Validates: AC-1791 (REQ-1152 - runtime persists operational metadata in SQLite)
func TestRuntimeRepositoryPersistsThreadStateAndDedupeMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(ctx, filepath.Join(dir, "storage.sqlite3"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	projectRepo := store.Projects()
	if err := projectRepo.Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        "/workspace/alpha",
		SlackChannelID:   "C12345678",
		SlackChannelName: "spexus-alpha",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	runtimeRepo := store.Runtime()
	if runtimeRepo == nil {
		t.Fatalf("Runtime() = nil")
	}

	threadState := runtimemodel.ThreadState{
		ThreadTS:      "1713686400.000100",
		ChannelID:     "C12345678",
		ProjectName:   "alpha",
		SessionName:   "slack-1713686400.000100",
		LastStatus:    "running",
		LastRequestID: "req-1",
	}
	if err := runtimeRepo.SaveThreadState(ctx, threadState); err != nil {
		t.Fatalf("SaveThreadState() error = %v", err)
	}

	loadedThreadState, err := runtimeRepo.LoadThreadState(ctx, threadState.ThreadTS)
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if loadedThreadState.ThreadTS != threadState.ThreadTS || loadedThreadState.SessionName != threadState.SessionName || loadedThreadState.LastRequestID != threadState.LastRequestID {
		t.Fatalf("LoadThreadState() = %#v, want %#v", loadedThreadState, threadState)
	}

	processedAt := time.Now().UTC().Add(time.Minute)
	if err := runtimeRepo.SaveEventDedupe(ctx, runtimemodel.EventDedupe{
		SourceType:  "mention",
		DeliveryID:  "Ev123",
		ReceivedAt:  time.Now().UTC(),
		ProcessedAt: &processedAt,
		Status:      "processed",
	}); err != nil {
		t.Fatalf("SaveEventDedupe() error = %v", err)
	}

	loadedDedupe, err := runtimeRepo.LoadEventDedupe(ctx, "mention", "Ev123")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if loadedDedupe.SourceType != "mention" || loadedDedupe.DeliveryID != "Ev123" || loadedDedupe.Status != "processed" || loadedDedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe() = %#v", loadedDedupe)
	}

	lockExpires := time.Now().UTC().Add(5 * time.Minute)
	if err := runtimeRepo.SaveThreadLock(ctx, ThreadLock{
		ThreadTS:       threadState.ThreadTS,
		LockOwner:      "runtime-1",
		LockedAt:       time.Now().UTC(),
		LeaseExpiresAt: &lockExpires,
	}); err != nil {
		t.Fatalf("SaveThreadLock() error = %v", err)
	}

	loadedLock, err := runtimeRepo.LoadThreadLock(ctx, threadState.ThreadTS)
	if err != nil {
		t.Fatalf("LoadThreadLock() error = %v", err)
	}
	if loadedLock.ThreadTS != threadState.ThreadTS || loadedLock.LockOwner != "runtime-1" || loadedLock.LeaseExpiresAt == nil {
		t.Fatalf("LoadThreadLock() = %#v", loadedLock)
	}

	if err := runtimeRepo.DeleteThreadLock(ctx, threadState.ThreadTS); err != nil {
		t.Fatalf("DeleteThreadLock() error = %v", err)
	}

	if _, err := runtimeRepo.LoadThreadLock(ctx, threadState.ThreadTS); err == nil {
		t.Fatalf("LoadThreadLock() error = nil, want not found")
	}
}

// Test: DefaultPath points storage bootstrap at the user config directory.
// Validates: AC-1783 (REQ-1144 - runtime initializes SQLite storage under ~/.config/spexus-agent/storage.sqlite3)
func TestDefaultPathUsesUserConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}

	want := filepath.Join(home, ".config", "spexus-agent", "storage.sqlite3")
	if path != want {
		t.Fatalf("DefaultPath() = %q, want %q", path, want)
	}
}
