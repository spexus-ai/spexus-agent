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

// Test: storage bootstrap migrates pre-async-runtime SQLite files that have older project, dedupe, thread, and lock schemas.
// Validates: EP-154 runtime startup can open existing user databases after async execution persistence columns are introduced.
func TestOpenMigratesLegacyRuntimeSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.sqlite3")

	db, err := sql.Open("sqlite3", storageDSN(path))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	legacyStmts := []string{
		`CREATE TABLE projects (
			name TEXT PRIMARY KEY,
			local_path TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`INSERT INTO projects (name, local_path, created_at, updated_at)
			VALUES ('alpha', '/workspace/alpha', '` + now + `', '` + now + `')`,
		`CREATE TABLE events_dedupe (
			slack_event_id TEXT PRIMARY KEY,
			received_at TEXT NOT NULL,
			processed_at TEXT,
			status TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO events_dedupe (slack_event_id, received_at, status)
			VALUES ('EvLegacy', '` + now + `', 'processed')`,
		`CREATE TABLE threads (
			thread_ts TEXT PRIMARY KEY,
			channel_id TEXT NOT NULL,
			project_name TEXT NOT NULL,
			session_name TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE thread_locks (
			thread_ts TEXT PRIMARY KEY,
			lock_owner TEXT NOT NULL,
			locked_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range legacyStmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			t.Fatalf("legacy schema exec error = %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("legacy db Close() error = %v", err)
	}

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() legacy schema error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		GitRemote:        "git@github.com:org/alpha.git",
		LocalPath:        "/workspace/alpha",
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() after migration error = %v", err)
	}

	dedupe, err := store.Runtime().LoadEventDedupe(ctx, "mention", "EvLegacy")
	if err != nil {
		t.Fatalf("LoadEventDedupe() migrated legacy row error = %v", err)
	}
	if dedupe.SourceType != "mention" || dedupe.DeliveryID != "EvLegacy" || dedupe.Status != "processed" {
		t.Fatalf("LoadEventDedupe() = %#v, want migrated mention/EvLegacy processed row", dedupe)
	}

	state := runtimemodel.ThreadState{
		ThreadTS:      "1713686400.000100",
		ChannelID:     "C12345678",
		ProjectName:   "alpha",
		SessionName:   "slack-1713686400.000100",
		LastStatus:    "processed",
		LastRequestID: "req-legacy",
	}
	if err := store.Runtime().SaveThreadState(ctx, state); err != nil {
		t.Fatalf("SaveThreadState() after migration error = %v", err)
	}
	loaded, err := store.Runtime().LoadThreadState(ctx, state.ThreadTS)
	if err != nil {
		t.Fatalf("LoadThreadState() after migration error = %v", err)
	}
	if loaded.LastStatus != state.LastStatus || loaded.LastRequestID != state.LastRequestID {
		t.Fatalf("LoadThreadState() = %#v, want migrated status/request columns", loaded)
	}

	if err := store.Runtime().SaveExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: "mention:EvMigrated",
		SourceType:  "mention",
		DeliveryID:  "EvMigrated",
		ProjectName: "alpha",
		ChannelID:   "C12345678",
		ThreadTS:    state.ThreadTS,
		SessionName: state.SessionName,
		Status:      runtimemodel.ExecutionStatusQueued,
	}); err != nil {
		t.Fatalf("SaveExecutionState() after migration error = %v", err)
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

	queuedAt := time.Now().UTC().Add(-2 * time.Minute)
	startedAt := time.Now().UTC().Add(-time.Minute)
	renderingStartedAt := time.Now().UTC().Add(-30 * time.Second)
	completedAt := time.Now().UTC()
	checkpointAt := time.Now().UTC()
	executionState := runtimemodel.ExecutionState{
		ExecutionID:                "mention:Ev123",
		SourceType:                 "mention",
		DeliveryID:                 "Ev123",
		ProjectName:                "alpha",
		ChannelID:                  "C12345678",
		ThreadTS:                   threadState.ThreadTS,
		SessionName:                threadState.SessionName,
		Status:                     runtimemodel.ExecutionStatusProcessed,
		QueuedAt:                   queuedAt,
		StartedAt:                  &startedAt,
		RenderingStartedAt:         &renderingStartedAt,
		CompletedAt:                &completedAt,
		UpdatedAt:                  completedAt,
		PublisherCheckpointKind:    "assistant_message_final",
		PublisherCheckpointSummary: "status summary rendered",
		PublisherCheckpointAt:      &checkpointAt,
	}
	if err := runtimeRepo.SaveExecutionState(ctx, executionState); err != nil {
		t.Fatalf("SaveExecutionState() error = %v", err)
	}

	loadedExecutionState, err := runtimeRepo.LoadExecutionState(ctx, executionState.ExecutionID)
	if err != nil {
		t.Fatalf("LoadExecutionState() error = %v", err)
	}
	if loadedExecutionState.Status != runtimemodel.ExecutionStatusProcessed || loadedExecutionState.SessionName != executionState.SessionName {
		t.Fatalf("LoadExecutionState() = %#v, want processed state %#v", loadedExecutionState, executionState)
	}
	if loadedExecutionState.PublisherCheckpointKind != executionState.PublisherCheckpointKind || loadedExecutionState.PublisherCheckpointSummary != executionState.PublisherCheckpointSummary {
		t.Fatalf("LoadExecutionState() checkpoint = %#v, want %#v", loadedExecutionState, executionState)
	}

	loadedByDelivery, err := runtimeRepo.LoadExecutionStateByDelivery(ctx, "mention", "Ev123")
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery() error = %v", err)
	}
	if loadedByDelivery.ExecutionID != executionState.ExecutionID {
		t.Fatalf("LoadExecutionStateByDelivery() = %#v, want execution id %q", loadedByDelivery, executionState.ExecutionID)
	}

	loadedLatestByThread, err := runtimeRepo.LoadLatestExecutionByThread(ctx, threadState.ThreadTS)
	if err != nil {
		t.Fatalf("LoadLatestExecutionByThread() error = %v", err)
	}
	if loadedLatestByThread.ExecutionID != executionState.ExecutionID {
		t.Fatalf("LoadLatestExecutionByThread() = %#v, want execution id %q", loadedLatestByThread, executionState.ExecutionID)
	}

	if err := runtimeRepo.SaveExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: "mention:Ev124",
		SourceType:  "mention",
		DeliveryID:  "Ev124",
		ProjectName: "alpha",
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000101",
		SessionName: "slack-1713686400.000101",
		Status:      runtimemodel.ExecutionStatusRunning,
		QueuedAt:    queuedAt.Add(time.Second),
		StartedAt:   &startedAt,
		UpdatedAt:   startedAt,
	}); err != nil {
		t.Fatalf("SaveExecutionState(running) error = %v", err)
	}

	nonTerminal, err := runtimeRepo.ListExecutionStatesByStatus(ctx, runtimemodel.ExecutionStatusQueued, runtimemodel.ExecutionStatusRunning)
	if err != nil {
		t.Fatalf("ListExecutionStatesByStatus() error = %v", err)
	}
	if got, want := len(nonTerminal), 1; got != want {
		t.Fatalf("ListExecutionStatesByStatus() count = %d, want %d", got, want)
	}
	if nonTerminal[0].ExecutionID != "mention:Ev124" || nonTerminal[0].Status != runtimemodel.ExecutionStatusRunning {
		t.Fatalf("ListExecutionStatesByStatus() = %#v, want running Ev124", nonTerminal)
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

// Test: the accepted-delivery claim persists dedupe and queued execution state together so restart recovery can observe the delivery before enqueue processing continues.
// Validates: AC-1981 (REQ-1429 - accepted deliveries persist an initial queued execution state), AC-1982 (REQ-1431 - startup recovery can discover accepted deliveries from persisted execution state)
func TestRuntimeRepositorySaveClaimedExecutionPersistsDedupeAndQueuedState(t *testing.T) {
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

	claimedAt := time.Date(2026, time.April, 26, 19, 30, 0, 0, time.UTC)
	dedupe := runtimemodel.EventDedupe{
		SourceType: "mention",
		DeliveryID: "Ev-claim",
		ReceivedAt: claimedAt,
		Status:     "acked",
	}
	state := runtimemodel.ExecutionState{
		ExecutionID: "mention:Ev-claim",
		SourceType:  "mention",
		DeliveryID:  "Ev-claim",
		ProjectName: "alpha",
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
		Status:      runtimemodel.ExecutionStatusQueued,
		QueuedAt:    claimedAt,
		UpdatedAt:   claimedAt,
	}

	if err := store.Runtime().SaveClaimedExecution(ctx, dedupe, state); err != nil {
		t.Fatalf("SaveClaimedExecution() error = %v", err)
	}

	loadedDedupe, err := store.Runtime().LoadEventDedupe(ctx, "mention", "Ev-claim")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if loadedDedupe.Status != "acked" || !loadedDedupe.ReceivedAt.Equal(claimedAt) {
		t.Fatalf("LoadEventDedupe() = %#v, want acked claim at %s", loadedDedupe, claimedAt)
	}

	loadedState, err := store.Runtime().LoadExecutionStateByDelivery(ctx, "mention", "Ev-claim")
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery() error = %v", err)
	}
	if loadedState.Status != runtimemodel.ExecutionStatusQueued || !loadedState.QueuedAt.Equal(claimedAt) {
		t.Fatalf("LoadExecutionStateByDelivery() = %#v, want queued state at %s", loadedState, claimedAt)
	}
	if loadedState.ThreadTS != state.ThreadTS || loadedState.SessionName != state.SessionName {
		t.Fatalf("LoadExecutionStateByDelivery() thread/session = (%q, %q), want (%q, %q)", loadedState.ThreadTS, loadedState.SessionName, state.ThreadTS, state.SessionName)
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
