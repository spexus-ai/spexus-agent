package storage

import (
	"context"
	"database/sql"
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
		SourceType:        "mention",
		DeliveryID:        "Ev123",
		ReceivedAt:        time.Now().UTC(),
		ProcessedAt:       &processedAt,
		Status:            "processed",
		DiagnosticContext: "dedupe processed",
	}); err != nil {
		t.Fatalf("SaveEventDedupe() error = %v", err)
	}

	loadedDedupe, err := runtimeRepo.LoadEventDedupe(ctx, "mention", "Ev123")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if loadedDedupe.SourceType != "mention" || loadedDedupe.DeliveryID != "Ev123" || loadedDedupe.Status != "processed" || loadedDedupe.ProcessedAt == nil || loadedDedupe.DiagnosticContext != "dedupe processed" {
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

// Test: execution requests are persisted with queued state and later lifecycle transitions keep timestamps and diagnostic context.
// Validates: AC-2107 (REQ-1572 - accepted invocation persists an execution request with a unique execution identifier), AC-2113 (REQ-1583 - lifecycle transitions persist timestamps), AC-2114 (REQ-1584 - startup failures persist failed state with diagnostic context)
func TestRuntimeRepositoryPersistsExecutionLifecycle(t *testing.T) {
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

	if err := store.Projects().Upsert(ctx, registry.Project{
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

	createdAt := time.Now().UTC().Add(-2 * time.Minute)
	request := runtimemodel.ExecutionRequest{
		ExecutionID: "exec-1",
		SourceType:  "mention",
		DeliveryID:  "Ev123",
		ChannelID:   "C12345678",
		ProjectName: "alpha",
		SessionKey:  "slack-1713686400.000100",
		ThreadTS:    "1713686400.000100",
		CommandText: "run TASK-915",
		CreatedAt:   createdAt,
	}
	if err := runtimeRepo.CreateExecution(ctx, request); err != nil {
		t.Fatalf("CreateExecution() error = %v", err)
	}

	loaded, err := runtimeRepo.LoadExecution(ctx, request.ExecutionID)
	if err != nil {
		t.Fatalf("LoadExecution() after create error = %v", err)
	}
	if loaded.Status != runtimemodel.ExecutionStateQueued {
		t.Fatalf("LoadExecution() status after create = %q, want %q", loaded.Status, runtimemodel.ExecutionStateQueued)
	}
	if loaded.SessionKey != request.SessionKey || loaded.CommandText != request.CommandText {
		t.Fatalf("LoadExecution() after create = %#v, want session key %q and command text %q", loaded, request.SessionKey, request.CommandText)
	}
	if loaded.CreatedAt.IsZero() || loaded.UpdatedAt.IsZero() {
		t.Fatalf("LoadExecution() timestamps after create = %#v, want created/updated timestamps", loaded)
	}

	startedAt := createdAt.Add(30 * time.Second)
	if err := runtimeRepo.UpdateExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: request.ExecutionID,
		Status:      runtimemodel.ExecutionStateRunning,
		StartedAt:   &startedAt,
		UpdatedAt:   startedAt,
	}); err != nil {
		t.Fatalf("UpdateExecutionState(running) error = %v", err)
	}

	failedAt := startedAt.Add(45 * time.Second)
	if err := runtimeRepo.UpdateExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID:       request.ExecutionID,
		Status:            runtimemodel.ExecutionStateFailed,
		DiagnosticContext: "startup failed: missing session owner",
		UpdatedAt:         failedAt,
	}); err != nil {
		t.Fatalf("UpdateExecutionState(failed) error = %v", err)
	}

	loaded, err = runtimeRepo.LoadExecution(ctx, request.ExecutionID)
	if err != nil {
		t.Fatalf("LoadExecution() after failed transition error = %v", err)
	}
	if loaded.Status != runtimemodel.ExecutionStateFailed {
		t.Fatalf("LoadExecution() final status = %q, want %q", loaded.Status, runtimemodel.ExecutionStateFailed)
	}
	if loaded.StartedAt == nil || !loaded.StartedAt.Equal(startedAt) {
		t.Fatalf("LoadExecution() started_at = %v, want %v", loaded.StartedAt, startedAt)
	}
	if loaded.CompletedAt == nil || !loaded.CompletedAt.Equal(failedAt) {
		t.Fatalf("LoadExecution() completed_at = %v, want %v", loaded.CompletedAt, failedAt)
	}
	if loaded.DiagnosticContext != "startup failed: missing session owner" {
		t.Fatalf("LoadExecution() diagnostic context = %q, want startup failure details", loaded.DiagnosticContext)
	}
}

// Test: an existing events_dedupe table without diagnostic_context is migrated in place and preserves new diagnostics.
// Validates: AC-2112 (REQ-1580 - ack-window failures persist diagnostic context on the claimed delivery)
func TestOpenMigratesEventDedupeDiagnosticContextColumn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.sqlite3")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE events_dedupe (
			source_type TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			received_at TEXT NOT NULL,
			processed_at TEXT,
			status TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(source_type, delivery_id)
		)
	`); err != nil {
		t.Fatalf("create legacy events_dedupe table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() error = %v", err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	runtimeRepo := store.Runtime()
	if err := runtimeRepo.SaveEventDedupe(ctx, runtimemodel.EventDedupe{
		SourceType:        "slash",
		DeliveryID:        "Ev-migrated",
		ReceivedAt:        time.Now().UTC(),
		Status:            "failed",
		DiagnosticContext: "ack window expired",
	}); err != nil {
		t.Fatalf("SaveEventDedupe() error = %v", err)
	}

	loaded, err := runtimeRepo.LoadEventDedupe(ctx, "slash", "Ev-migrated")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if loaded.DiagnosticContext != "ack window expired" {
		t.Fatalf("LoadEventDedupe() diagnostic context = %q, want migrated value", loaded.DiagnosticContext)
	}
}

// Test: an existing executions table without session_key is migrated in place and derives a stable session key from thread state.
// Validates: AC-2107 (REQ-1573 - persisted execution requests remain readable after schema upgrades)
func TestOpenMigratesExecutionsSessionKeyColumn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.sqlite3")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		PRAGMA foreign_keys = ON;
		CREATE TABLE projects (
			name TEXT PRIMARY KEY,
			git_remote TEXT NOT NULL DEFAULT '',
			local_path TEXT NOT NULL,
			slack_channel_name TEXT NOT NULL DEFAULT '',
			slack_channel_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE executions (
			execution_id TEXT PRIMARY KEY,
			source_type TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			project_name TEXT NOT NULL,
			thread_ts TEXT NOT NULL DEFAULT '',
			command_text TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			diagnostic_context TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			started_at TEXT,
			updated_at TEXT NOT NULL,
			completed_at TEXT,
			FOREIGN KEY(project_name) REFERENCES projects(name) ON UPDATE CASCADE ON DELETE RESTRICT
		);
	`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO projects (
			name, git_remote, local_path, slack_channel_name, slack_channel_id, created_at, updated_at
		) VALUES (?, '', ?, ?, ?, ?, ?)
	`, "alpha", filepath.Join(dir, "alpha"), "alpha", "C123", createdAt, createdAt); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO executions (
			execution_id, source_type, delivery_id, channel_id, project_name,
			thread_ts, command_text, status, diagnostic_context, created_at, started_at, updated_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"exec-legacy",
		"mention",
		"Ev-legacy",
		"C123",
		"alpha",
		"1713686400.000100",
		"status",
		"running",
		"",
		createdAt,
		nil,
		createdAt,
		nil,
	); err != nil {
		t.Fatalf("insert legacy execution: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() error = %v", err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	loaded, err := store.Runtime().LoadExecution(ctx, "exec-legacy")
	if err != nil {
		t.Fatalf("LoadExecution() error = %v", err)
	}
	if loaded.SessionKey != "slack-1713686400.000100" {
		t.Fatalf("LoadExecution() session key = %q, want derived session key", loaded.SessionKey)
	}
	if loaded.Status != runtimemodel.ExecutionStateRunning {
		t.Fatalf("LoadExecution() status = %q, want %q", loaded.Status, runtimemodel.ExecutionStateRunning)
	}
}

// Test: the older executions schema with session_name and queued_at is migrated into the current execution model.
// Validates: AC-2107 (REQ-1573 - persisted execution requests remain readable after schema upgrades)
func TestOpenMigratesLegacyExecutionsSessionNameSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.sqlite3")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		PRAGMA foreign_keys = ON;
		CREATE TABLE projects (
			name TEXT PRIMARY KEY,
			git_remote TEXT NOT NULL DEFAULT '',
			local_path TEXT NOT NULL,
			slack_channel_name TEXT NOT NULL DEFAULT '',
			slack_channel_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE executions (
			execution_id TEXT PRIMARY KEY,
			source_type TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			project_name TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			thread_ts TEXT NOT NULL DEFAULT '',
			session_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			queued_at TEXT NOT NULL,
			started_at TEXT,
			rendering_started_at TEXT,
			completed_at TEXT,
			cancelled_at TEXT,
			updated_at TEXT NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			publisher_checkpoint_kind TEXT NOT NULL DEFAULT '',
			publisher_checkpoint_summary TEXT NOT NULL DEFAULT '',
			publisher_checkpoint_at TEXT,
			FOREIGN KEY(project_name) REFERENCES projects(name) ON UPDATE CASCADE ON DELETE RESTRICT
		);
	`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	queuedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO projects (
			name, git_remote, local_path, slack_channel_name, slack_channel_id, created_at, updated_at
		) VALUES (?, '', ?, ?, ?, ?, ?)
	`, "alpha", filepath.Join(dir, "alpha"), "alpha", "C123", queuedAt, queuedAt); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO executions (
			execution_id, source_type, delivery_id, project_name, channel_id, thread_ts, session_name,
			status, queued_at, started_at, rendering_started_at, completed_at, cancelled_at, updated_at,
			last_error, publisher_checkpoint_kind, publisher_checkpoint_summary, publisher_checkpoint_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		"exec-legacy-v0",
		"slash",
		"Ev-legacy-v0",
		"alpha",
		"C123",
		"1713686400.000100",
		"slack-1713686400.000100",
		"failed",
		queuedAt,
		nil,
		nil,
		nil,
		nil,
		queuedAt,
		"ack window expired",
		"",
		"",
		nil,
	); err != nil {
		t.Fatalf("insert legacy execution: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() error = %v", err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	loaded, err := store.Runtime().LoadExecution(ctx, "exec-legacy-v0")
	if err != nil {
		t.Fatalf("LoadExecution() error = %v", err)
	}
	if loaded.SessionKey != "slack-1713686400.000100" {
		t.Fatalf("LoadExecution() session key = %q, want migrated session key", loaded.SessionKey)
	}
	if loaded.CreatedAt.IsZero() {
		t.Fatalf("LoadExecution() createdAt is zero, want queued_at migrated")
	}
	if loaded.DiagnosticContext != "ack window expired" {
		t.Fatalf("LoadExecution() diagnostic context = %q, want last_error migrated", loaded.DiagnosticContext)
	}
	if loaded.Status != runtimemodel.ExecutionStateFailed {
		t.Fatalf("LoadExecution() status = %q, want %q", loaded.Status, runtimemodel.ExecutionStateFailed)
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
