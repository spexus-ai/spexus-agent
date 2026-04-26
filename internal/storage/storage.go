package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/spexus-ai/spexus-agent/internal/registry"
	runtimemodel "github.com/spexus-ai/spexus-agent/internal/runtime"
)

const (
	defaultConfigDirName = ".config"
	defaultAppDirName    = "spexus-agent"
	defaultDBName        = "storage.sqlite3"
)

var ErrNotFound = runtimemodel.ErrNotFound

type Store struct {
	db   *sql.DB
	path string
}

type ProjectRepository struct {
	db *sql.DB
}

type RuntimeRepository struct {
	db *sql.DB
}

type ThreadLock = runtimemodel.ThreadLock

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, defaultConfigDirName, defaultAppDirName, defaultDBName), nil
}

func OpenDefault(ctx context.Context) (*Store, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Open(ctx, path)
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create storage directory: %w", err)
	}

	db, err := sql.Open("sqlite3", storageDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite storage: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db, path: path}
	if err := store.bootstrap(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	if err := ensureFilePermissions(path); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Store) Projects() *ProjectRepository {
	if s == nil || s.db == nil {
		return nil
	}
	return &ProjectRepository{db: s.db}
}

func (s *Store) Runtime() *RuntimeRepository {
	if s == nil || s.db == nil {
		return nil
	}
	return &RuntimeRepository{db: s.db}
}

func (s *Store) bootstrap(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("set journal mode: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS projects (
			name TEXT PRIMARY KEY,
			git_remote TEXT NOT NULL DEFAULT '',
			local_path TEXT NOT NULL,
			slack_channel_name TEXT NOT NULL DEFAULT '',
			slack_channel_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_local_path ON projects(local_path)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_slack_channel_id ON projects(slack_channel_id) WHERE slack_channel_id <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_slack_channel_name ON projects(slack_channel_name) WHERE slack_channel_name <> ''`,
		`CREATE TABLE IF NOT EXISTS events_dedupe (
			source_type TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			received_at TEXT NOT NULL,
			processed_at TEXT,
			status TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(source_type, delivery_id)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_events_dedupe_source_delivery ON events_dedupe(source_type, delivery_id)`,
		`CREATE TABLE IF NOT EXISTS threads (
			thread_ts TEXT PRIMARY KEY,
			channel_id TEXT NOT NULL,
			project_name TEXT NOT NULL,
			session_name TEXT NOT NULL,
			last_status TEXT NOT NULL DEFAULT '',
			last_request_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY(project_name) REFERENCES projects(name) ON UPDATE CASCADE ON DELETE RESTRICT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_threads_channel_id ON threads(channel_id)`,
		`CREATE INDEX IF NOT EXISTS idx_threads_project_name ON threads(project_name)`,
		`CREATE TABLE IF NOT EXISTS thread_locks (
			thread_ts TEXT PRIMARY KEY,
			lock_owner TEXT NOT NULL,
			locked_at TEXT NOT NULL,
			lease_expires_at TEXT,
			updated_at TEXT NOT NULL,
			FOREIGN KEY(thread_ts) REFERENCES threads(thread_ts) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS executions (
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
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_executions_source_delivery ON executions(source_type, delivery_id)`,
		`CREATE INDEX IF NOT EXISTS idx_executions_thread_ts ON executions(thread_ts) WHERE thread_ts <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_executions_session_name ON executions(session_name) WHERE session_name <> ''`,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("bootstrap schema: %w", err)
		}
	}

	if err := migrateEventDedupeTable(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema bootstrap: %w", err)
	}

	return nil
}

func (r *ProjectRepository) Upsert(ctx context.Context, project registry.Project) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if project.Name == "" {
		return fmt.Errorf("project name is required")
	}
	if project.LocalPath == "" {
		return fmt.Errorf("project local path is required")
	}
	if project.SlackChannelName == "" {
		return fmt.Errorf("project slack channel name is required")
	}
	if project.SlackChannelID == "" {
		return fmt.Errorf("project slack channel id is required")
	}

	now := time.Now().UTC()
	if project.CreatedAt.IsZero() {
		project.CreatedAt = now
	}
	if project.UpdatedAt.IsZero() {
		project.UpdatedAt = now
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO projects (
			name, git_remote, local_path, slack_channel_name, slack_channel_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			git_remote = excluded.git_remote,
			local_path = excluded.local_path,
			slack_channel_name = excluded.slack_channel_name,
			slack_channel_id = excluded.slack_channel_id,
			updated_at = excluded.updated_at
	`,
		project.Name,
		project.GitRemote,
		project.LocalPath,
		project.SlackChannelName,
		project.SlackChannelID,
		project.CreatedAt.UTC().Format(time.RFC3339Nano),
		project.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("upsert project %q: %w", project.Name, err)
	}

	return nil
}

func (r *ProjectRepository) Get(ctx context.Context, name string) (registry.Project, error) {
	if err := ctx.Err(); err != nil {
		return registry.Project{}, err
	}
	if name == "" {
		return registry.Project{}, fmt.Errorf("project name is required")
	}

	row := r.db.QueryRowContext(ctx, `
		SELECT name, git_remote, local_path, slack_channel_name, slack_channel_id, created_at, updated_at
		FROM projects
		WHERE name = ?
	`, name)

	project, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return registry.Project{}, registry.ErrNotFound
		}
		return registry.Project{}, fmt.Errorf("load project %q: %w", name, err)
	}

	return project, nil
}

func (r *ProjectRepository) GetByChannelID(ctx context.Context, channelID string) (registry.Project, error) {
	if err := ctx.Err(); err != nil {
		return registry.Project{}, err
	}
	if channelID == "" {
		return registry.Project{}, fmt.Errorf("channel id is required")
	}

	row := r.db.QueryRowContext(ctx, `
		SELECT name, git_remote, local_path, slack_channel_name, slack_channel_id, created_at, updated_at
		FROM projects
		WHERE slack_channel_id = ?
	`, channelID)

	project, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return registry.Project{}, registry.ErrNotFound
		}
		return registry.Project{}, fmt.Errorf("load project by channel id %q: %w", channelID, err)
	}

	return project, nil
}

func (r *ProjectRepository) List(ctx context.Context) ([]registry.Project, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT name, git_remote, local_path, slack_channel_name, slack_channel_id, created_at, updated_at
		FROM projects
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	projects := make([]registry.Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}

	return projects, nil
}

func (r *ProjectRepository) Count(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	row := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count projects: %w", err)
	}

	return count, nil
}

func (r *ProjectRepository) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("project name is required")
	}

	if _, err := r.db.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete project %q: %w", name, err)
	}

	return nil
}

func (r *RuntimeRepository) SaveThreadState(ctx context.Context, state runtimemodel.ThreadState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.ThreadTS == "" {
		return fmt.Errorf("thread timestamp is required")
	}
	if state.ChannelID == "" {
		return fmt.Errorf("channel id is required")
	}
	if state.ProjectName == "" {
		return fmt.Errorf("project name is required")
	}
	if state.SessionName == "" {
		return fmt.Errorf("session name is required")
	}

	now := time.Now().UTC()
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = now
	}

	createdAt := state.UpdatedAt
	row := r.db.QueryRowContext(ctx, `SELECT created_at FROM threads WHERE thread_ts = ?`, state.ThreadTS)
	var createdAtText string
	switch err := row.Scan(&createdAtText); {
	case err == nil:
		parsed, parseErr := parseTime(createdAtText)
		if parseErr != nil {
			return fmt.Errorf("read existing thread state %q: %w", state.ThreadTS, parseErr)
		}
		createdAt = parsed
	case errors.Is(err, sql.ErrNoRows):
		// Keep the new timestamp.
	default:
		return fmt.Errorf("read existing thread state %q: %w", state.ThreadTS, err)
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO threads (
			thread_ts, channel_id, project_name, session_name, last_status, last_request_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(thread_ts) DO UPDATE SET
			channel_id = excluded.channel_id,
			project_name = excluded.project_name,
			session_name = excluded.session_name,
			last_status = excluded.last_status,
			last_request_id = excluded.last_request_id,
			updated_at = excluded.updated_at
	`,
		state.ThreadTS,
		state.ChannelID,
		state.ProjectName,
		state.SessionName,
		state.LastStatus,
		state.LastRequestID,
		createdAt.UTC().Format(time.RFC3339Nano),
		state.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save thread state %q: %w", state.ThreadTS, err)
	}

	return nil
}

func (r *RuntimeRepository) LoadThreadState(ctx context.Context, threadTS string) (runtimemodel.ThreadState, error) {
	if err := ctx.Err(); err != nil {
		return runtimemodel.ThreadState{}, err
	}
	if threadTS == "" {
		return runtimemodel.ThreadState{}, fmt.Errorf("thread timestamp is required")
	}

	row := r.db.QueryRowContext(ctx, `
		SELECT thread_ts, channel_id, project_name, session_name, last_status, last_request_id, updated_at
		FROM threads
		WHERE thread_ts = ?
	`, threadTS)

	var state runtimemodel.ThreadState
	var updatedAtText string
	if err := row.Scan(
		&state.ThreadTS,
		&state.ChannelID,
		&state.ProjectName,
		&state.SessionName,
		&state.LastStatus,
		&state.LastRequestID,
		&updatedAtText,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimemodel.ThreadState{}, ErrNotFound
		}
		return runtimemodel.ThreadState{}, fmt.Errorf("load thread state %q: %w", threadTS, err)
	}

	updatedAt, err := parseTime(updatedAtText)
	if err != nil {
		return runtimemodel.ThreadState{}, fmt.Errorf("parse updated_at for thread %q: %w", threadTS, err)
	}
	state.UpdatedAt = updatedAt

	return state, nil
}

func (r *RuntimeRepository) CountThreads(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	row := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads`)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count threads: %w", err)
	}

	return count, nil
}

func (r *RuntimeRepository) SaveEventDedupe(ctx context.Context, dedupe runtimemodel.EventDedupe) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dedupe.SourceType == "" {
		return fmt.Errorf("slack source type is required")
	}
	if dedupe.DeliveryID == "" {
		return fmt.Errorf("slack delivery id is required")
	}

	if dedupe.ReceivedAt.IsZero() {
		dedupe.ReceivedAt = time.Now().UTC()
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO events_dedupe (
			source_type, delivery_id, received_at, processed_at, status
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(source_type, delivery_id) DO UPDATE SET
			received_at = excluded.received_at,
			processed_at = excluded.processed_at,
			status = excluded.status
	`,
		dedupe.SourceType,
		dedupe.DeliveryID,
		dedupe.ReceivedAt.UTC().Format(time.RFC3339Nano),
		formatNullableTime(dedupe.ProcessedAt),
		dedupe.Status,
	)
	if err != nil {
		return fmt.Errorf("save event dedupe %s/%q: %w", dedupe.SourceType, dedupe.DeliveryID, err)
	}

	return nil
}

func (r *RuntimeRepository) SaveClaimedExecution(ctx context.Context, dedupe runtimemodel.EventDedupe, state runtimemodel.ExecutionState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dedupe.SourceType == "" {
		return fmt.Errorf("slack source type is required")
	}
	if dedupe.DeliveryID == "" {
		return fmt.Errorf("slack delivery id is required")
	}
	if state.ExecutionID == "" {
		return fmt.Errorf("execution id is required")
	}
	if state.SourceType == "" {
		return fmt.Errorf("execution source type is required")
	}
	if state.DeliveryID == "" {
		return fmt.Errorf("execution delivery id is required")
	}
	if state.ProjectName == "" {
		return fmt.Errorf("execution project name is required")
	}
	if state.ChannelID == "" {
		return fmt.Errorf("execution channel id is required")
	}
	if state.Status == "" {
		return fmt.Errorf("execution status is required")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin claimed execution transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := saveEventDedupeTx(ctx, tx, dedupe); err != nil {
		return err
	}
	if err := saveExecutionStateTx(ctx, tx, state); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit claimed execution transaction: %w", err)
	}
	return nil
}

func (r *RuntimeRepository) LoadEventDedupe(ctx context.Context, sourceType, deliveryID string) (runtimemodel.EventDedupe, error) {
	if err := ctx.Err(); err != nil {
		return runtimemodel.EventDedupe{}, err
	}
	if sourceType == "" {
		return runtimemodel.EventDedupe{}, fmt.Errorf("slack source type is required")
	}
	if deliveryID == "" {
		return runtimemodel.EventDedupe{}, fmt.Errorf("slack delivery id is required")
	}

	row := r.db.QueryRowContext(ctx, `
		SELECT source_type, delivery_id, received_at, processed_at, status
		FROM events_dedupe
		WHERE source_type = ? AND delivery_id = ?
	`, sourceType, deliveryID)

	var dedupe runtimemodel.EventDedupe
	var receivedAtText string
	var processedAtText sql.NullString
	if err := row.Scan(&dedupe.SourceType, &dedupe.DeliveryID, &receivedAtText, &processedAtText, &dedupe.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimemodel.EventDedupe{}, ErrNotFound
		}
		return runtimemodel.EventDedupe{}, fmt.Errorf("load event dedupe %s/%q: %w", sourceType, deliveryID, err)
	}

	receivedAt, err := parseTime(receivedAtText)
	if err != nil {
		return runtimemodel.EventDedupe{}, fmt.Errorf("parse received_at for %s/%q: %w", sourceType, deliveryID, err)
	}
	dedupe.ReceivedAt = receivedAt

	if processedAtText.Valid {
		processedAt, err := parseTime(processedAtText.String)
		if err != nil {
			return runtimemodel.EventDedupe{}, fmt.Errorf("parse processed_at for %s/%q: %w", sourceType, deliveryID, err)
		}
		dedupe.ProcessedAt = &processedAt
	}

	return dedupe, nil
}

func (r *RuntimeRepository) CountEventDedupe(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	row := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_dedupe`)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count event dedupe rows: %w", err)
	}

	return count, nil
}

func (r *RuntimeRepository) SaveExecutionState(ctx context.Context, state runtimemodel.ExecutionState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.ExecutionID == "" {
		return fmt.Errorf("execution id is required")
	}
	if state.SourceType == "" {
		return fmt.Errorf("execution source type is required")
	}
	if state.DeliveryID == "" {
		return fmt.Errorf("execution delivery id is required")
	}
	if state.ProjectName == "" {
		return fmt.Errorf("execution project name is required")
	}
	if state.ChannelID == "" {
		return fmt.Errorf("execution channel id is required")
	}
	if state.Status == "" {
		return fmt.Errorf("execution status is required")
	}
	if state.QueuedAt.IsZero() {
		state.QueuedAt = time.Now().UTC()
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = state.QueuedAt
	}

	if err := saveExecutionStateTx(ctx, r.db, state); err != nil {
		return err
	}

	return nil
}

func saveExecutionStateTx(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, state runtimemodel.ExecutionState) error {
	_, err := execer.ExecContext(ctx, `
		INSERT INTO executions (
			execution_id, source_type, delivery_id, project_name, channel_id, thread_ts, session_name,
			status, queued_at, started_at, rendering_started_at, completed_at, cancelled_at, updated_at,
			last_error, publisher_checkpoint_kind, publisher_checkpoint_summary, publisher_checkpoint_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(execution_id) DO UPDATE SET
			source_type = excluded.source_type,
			delivery_id = excluded.delivery_id,
			project_name = excluded.project_name,
			channel_id = excluded.channel_id,
			thread_ts = excluded.thread_ts,
			session_name = excluded.session_name,
			status = excluded.status,
			queued_at = excluded.queued_at,
			started_at = excluded.started_at,
			rendering_started_at = excluded.rendering_started_at,
			completed_at = excluded.completed_at,
			cancelled_at = excluded.cancelled_at,
			updated_at = excluded.updated_at,
			last_error = excluded.last_error,
			publisher_checkpoint_kind = excluded.publisher_checkpoint_kind,
			publisher_checkpoint_summary = excluded.publisher_checkpoint_summary,
			publisher_checkpoint_at = excluded.publisher_checkpoint_at
	`,
		state.ExecutionID,
		state.SourceType,
		state.DeliveryID,
		state.ProjectName,
		state.ChannelID,
		state.ThreadTS,
		state.SessionName,
		state.Status,
		state.QueuedAt.UTC().Format(time.RFC3339Nano),
		formatNullableTime(state.StartedAt),
		formatNullableTime(state.RenderingStartedAt),
		formatNullableTime(state.CompletedAt),
		formatNullableTime(state.CancelledAt),
		state.UpdatedAt.UTC().Format(time.RFC3339Nano),
		state.LastError,
		state.PublisherCheckpointKind,
		state.PublisherCheckpointSummary,
		formatNullableTime(state.PublisherCheckpointAt),
	)
	if err != nil {
		return fmt.Errorf("save execution state %q: %w", state.ExecutionID, err)
	}

	return nil
}

func saveEventDedupeTx(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, dedupe runtimemodel.EventDedupe) error {
	if dedupe.ReceivedAt.IsZero() {
		dedupe.ReceivedAt = time.Now().UTC()
	}

	_, err := execer.ExecContext(ctx, `
		INSERT INTO events_dedupe (
			source_type, delivery_id, received_at, processed_at, status
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(source_type, delivery_id) DO UPDATE SET
			received_at = excluded.received_at,
			processed_at = excluded.processed_at,
			status = excluded.status
	`,
		dedupe.SourceType,
		dedupe.DeliveryID,
		dedupe.ReceivedAt.UTC().Format(time.RFC3339Nano),
		formatNullableTime(dedupe.ProcessedAt),
		dedupe.Status,
	)
	if err != nil {
		return fmt.Errorf("save event dedupe %s/%q: %w", dedupe.SourceType, dedupe.DeliveryID, err)
	}

	return nil
}

func (r *RuntimeRepository) LoadExecutionState(ctx context.Context, executionID string) (runtimemodel.ExecutionState, error) {
	if err := ctx.Err(); err != nil {
		return runtimemodel.ExecutionState{}, err
	}
	if executionID == "" {
		return runtimemodel.ExecutionState{}, fmt.Errorf("execution id is required")
	}

	row := r.db.QueryRowContext(ctx, executionSelectSQL(`WHERE execution_id = ?`), executionID)
	state, err := scanExecutionState(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimemodel.ExecutionState{}, ErrNotFound
		}
		return runtimemodel.ExecutionState{}, fmt.Errorf("load execution state %q: %w", executionID, err)
	}
	return state, nil
}

func (r *RuntimeRepository) LoadExecutionStateByDelivery(ctx context.Context, sourceType, deliveryID string) (runtimemodel.ExecutionState, error) {
	if err := ctx.Err(); err != nil {
		return runtimemodel.ExecutionState{}, err
	}
	if sourceType == "" {
		return runtimemodel.ExecutionState{}, fmt.Errorf("execution source type is required")
	}
	if deliveryID == "" {
		return runtimemodel.ExecutionState{}, fmt.Errorf("execution delivery id is required")
	}

	row := r.db.QueryRowContext(ctx, executionSelectSQL(`WHERE source_type = ? AND delivery_id = ?`), sourceType, deliveryID)
	state, err := scanExecutionState(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimemodel.ExecutionState{}, ErrNotFound
		}
		return runtimemodel.ExecutionState{}, fmt.Errorf("load execution state for %s/%q: %w", sourceType, deliveryID, err)
	}
	return state, nil
}

func (r *RuntimeRepository) LoadLatestExecutionByThread(ctx context.Context, threadTS string) (runtimemodel.ExecutionState, error) {
	if err := ctx.Err(); err != nil {
		return runtimemodel.ExecutionState{}, err
	}
	if threadTS == "" {
		return runtimemodel.ExecutionState{}, fmt.Errorf("thread timestamp is required")
	}

	row := r.db.QueryRowContext(ctx, executionSelectSQL(`WHERE thread_ts = ? ORDER BY queued_at DESC, updated_at DESC LIMIT 1`), threadTS)
	state, err := scanExecutionState(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimemodel.ExecutionState{}, ErrNotFound
		}
		return runtimemodel.ExecutionState{}, fmt.Errorf("load latest execution state for thread %q: %w", threadTS, err)
	}
	return state, nil
}

func (r *RuntimeRepository) ListExecutionStatesByStatus(ctx context.Context, statuses ...string) ([]runtimemodel.ExecutionState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(statuses) == 0 {
		return nil, fmt.Errorf("at least one execution status is required")
	}

	args := make([]any, 0, len(statuses))
	placeholders := make([]string, 0, len(statuses))
	for _, status := range statuses {
		status = strings.TrimSpace(status)
		if status == "" {
			return nil, fmt.Errorf("execution status is required")
		}
		args = append(args, status)
		placeholders = append(placeholders, "?")
	}

	rows, err := r.db.QueryContext(
		ctx,
		executionSelectSQL(
			fmt.Sprintf(
				"WHERE status IN (%s) ORDER BY queued_at ASC, updated_at ASC",
				strings.Join(placeholders, ", "),
			),
		),
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("list execution states by status: %w", err)
	}
	defer rows.Close()

	states := make([]runtimemodel.ExecutionState, 0)
	for rows.Next() {
		state, err := scanExecutionState(rows)
		if err != nil {
			return nil, fmt.Errorf("scan execution state row: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate execution states by status: %w", err)
	}

	return states, nil
}

func migrateEventDedupeTable(ctx context.Context, tx *sql.Tx) error {
	hasSourceType, err := tableColumnExists(ctx, tx, "events_dedupe", "source_type")
	if err != nil {
		return fmt.Errorf("inspect events_dedupe source_type column: %w", err)
	}
	hasDeliveryID, err := tableColumnExists(ctx, tx, "events_dedupe", "delivery_id")
	if err != nil {
		return fmt.Errorf("inspect events_dedupe delivery_id column: %w", err)
	}
	if hasSourceType && hasDeliveryID {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS events_dedupe_v2 (
			source_type TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			received_at TEXT NOT NULL,
			processed_at TEXT,
			status TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(source_type, delivery_id)
		)
	`); err != nil {
		return fmt.Errorf("create events_dedupe_v2: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO events_dedupe_v2 (source_type, delivery_id, received_at, processed_at, status)
		SELECT 'mention', slack_event_id, received_at, processed_at, status
		FROM events_dedupe
	`); err != nil {
		return fmt.Errorf("copy events_dedupe rows into v2: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE events_dedupe`); err != nil {
		return fmt.Errorf("drop legacy events_dedupe: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE events_dedupe_v2 RENAME TO events_dedupe`); err != nil {
		return fmt.Errorf("rename events_dedupe_v2: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_events_dedupe_source_delivery ON events_dedupe(source_type, delivery_id)`); err != nil {
		return fmt.Errorf("create events_dedupe source/delivery index: %w", err)
	}

	return nil
}

func tableColumnExists(ctx context.Context, tx *sql.Tx, tableName, columnName string) (bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			valueType string
			notNull   int
			defaults  sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &valueType, &notNull, &defaults, &pk); err != nil {
			return false, err
		}
		if name == columnName {
			return true, nil
		}
	}

	if err := rows.Err(); err != nil {
		return false, err
	}

	return false, nil
}

func (r *RuntimeRepository) SaveThreadLock(ctx context.Context, lock ThreadLock) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lock.ThreadTS == "" {
		return fmt.Errorf("thread timestamp is required")
	}
	if lock.LockOwner == "" {
		return fmt.Errorf("lock owner is required")
	}
	if lock.LockedAt.IsZero() {
		lock.LockedAt = time.Now().UTC()
	}
	if lock.UpdatedAt.IsZero() {
		lock.UpdatedAt = lock.LockedAt
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO thread_locks (
			thread_ts, lock_owner, locked_at, lease_expires_at, updated_at
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(thread_ts) DO UPDATE SET
			lock_owner = excluded.lock_owner,
			locked_at = excluded.locked_at,
			lease_expires_at = excluded.lease_expires_at,
			updated_at = excluded.updated_at
	`,
		lock.ThreadTS,
		lock.LockOwner,
		lock.LockedAt.UTC().Format(time.RFC3339Nano),
		formatNullableTime(lock.LeaseExpiresAt),
		lock.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save thread lock %q: %w", lock.ThreadTS, err)
	}

	return nil
}

func (r *RuntimeRepository) LoadThreadLock(ctx context.Context, threadTS string) (ThreadLock, error) {
	if err := ctx.Err(); err != nil {
		return ThreadLock{}, err
	}
	if threadTS == "" {
		return ThreadLock{}, fmt.Errorf("thread timestamp is required")
	}

	row := r.db.QueryRowContext(ctx, `
		SELECT thread_ts, lock_owner, locked_at, lease_expires_at, updated_at
		FROM thread_locks
		WHERE thread_ts = ?
	`, threadTS)

	var lock ThreadLock
	var lockedAtText, updatedAtText string
	var leaseExpiresAt sql.NullString
	if err := row.Scan(&lock.ThreadTS, &lock.LockOwner, &lockedAtText, &leaseExpiresAt, &updatedAtText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ThreadLock{}, ErrNotFound
		}
		return ThreadLock{}, fmt.Errorf("load thread lock %q: %w", threadTS, err)
	}

	lockedAt, err := parseTime(lockedAtText)
	if err != nil {
		return ThreadLock{}, fmt.Errorf("parse locked_at for thread %q: %w", threadTS, err)
	}
	lock.LockedAt = lockedAt

	updatedAt, err := parseTime(updatedAtText)
	if err != nil {
		return ThreadLock{}, fmt.Errorf("parse updated_at for thread %q: %w", threadTS, err)
	}
	lock.UpdatedAt = updatedAt

	if leaseExpiresAt.Valid {
		parsed, err := parseTime(leaseExpiresAt.String)
		if err != nil {
			return ThreadLock{}, fmt.Errorf("parse lease_expires_at for thread %q: %w", threadTS, err)
		}
		lock.LeaseExpiresAt = &parsed
	}

	return lock, nil
}

func (r *RuntimeRepository) DeleteThreadLock(ctx context.Context, threadTS string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if threadTS == "" {
		return fmt.Errorf("thread timestamp is required")
	}

	if _, err := r.db.ExecContext(ctx, `DELETE FROM thread_locks WHERE thread_ts = ?`, threadTS); err != nil {
		return fmt.Errorf("delete thread lock %q: %w", threadTS, err)
	}

	return nil
}

func executionSelectSQL(suffix string) string {
	return `
		SELECT execution_id, source_type, delivery_id, project_name, channel_id, thread_ts, session_name,
			status, queued_at, started_at, rendering_started_at, completed_at, cancelled_at, updated_at,
			last_error, publisher_checkpoint_kind, publisher_checkpoint_summary, publisher_checkpoint_at
		FROM executions
	` + suffix
}

func scanExecutionState(scanner interface {
	Scan(dest ...any) error
}) (runtimemodel.ExecutionState, error) {
	var state runtimemodel.ExecutionState
	var (
		queuedAtText              string
		startedAtText             sql.NullString
		renderingStartedAtText    sql.NullString
		completedAtText           sql.NullString
		cancelledAtText           sql.NullString
		updatedAtText             string
		publisherCheckpointAtText sql.NullString
	)

	if err := scanner.Scan(
		&state.ExecutionID,
		&state.SourceType,
		&state.DeliveryID,
		&state.ProjectName,
		&state.ChannelID,
		&state.ThreadTS,
		&state.SessionName,
		&state.Status,
		&queuedAtText,
		&startedAtText,
		&renderingStartedAtText,
		&completedAtText,
		&cancelledAtText,
		&updatedAtText,
		&state.LastError,
		&state.PublisherCheckpointKind,
		&state.PublisherCheckpointSummary,
		&publisherCheckpointAtText,
	); err != nil {
		return runtimemodel.ExecutionState{}, err
	}

	var err error
	state.QueuedAt, err = parseTime(queuedAtText)
	if err != nil {
		return runtimemodel.ExecutionState{}, fmt.Errorf("parse queued_at for execution %q: %w", state.ExecutionID, err)
	}
	if state.StartedAt, err = parseNullableTime(startedAtText); err != nil {
		return runtimemodel.ExecutionState{}, fmt.Errorf("parse started_at for execution %q: %w", state.ExecutionID, err)
	}
	if state.RenderingStartedAt, err = parseNullableTime(renderingStartedAtText); err != nil {
		return runtimemodel.ExecutionState{}, fmt.Errorf("parse rendering_started_at for execution %q: %w", state.ExecutionID, err)
	}
	if state.CompletedAt, err = parseNullableTime(completedAtText); err != nil {
		return runtimemodel.ExecutionState{}, fmt.Errorf("parse completed_at for execution %q: %w", state.ExecutionID, err)
	}
	if state.CancelledAt, err = parseNullableTime(cancelledAtText); err != nil {
		return runtimemodel.ExecutionState{}, fmt.Errorf("parse cancelled_at for execution %q: %w", state.ExecutionID, err)
	}
	state.UpdatedAt, err = parseTime(updatedAtText)
	if err != nil {
		return runtimemodel.ExecutionState{}, fmt.Errorf("parse updated_at for execution %q: %w", state.ExecutionID, err)
	}
	if state.PublisherCheckpointAt, err = parseNullableTime(publisherCheckpointAtText); err != nil {
		return runtimemodel.ExecutionState{}, fmt.Errorf("parse publisher_checkpoint_at for execution %q: %w", state.ExecutionID, err)
	}

	return state, nil
}

func parseNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}

	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func scanProject(scanner interface {
	Scan(dest ...any) error
}) (registry.Project, error) {
	var project registry.Project
	var createdAtText string
	var updatedAtText string
	if err := scanner.Scan(
		&project.Name,
		&project.GitRemote,
		&project.LocalPath,
		&project.SlackChannelName,
		&project.SlackChannelID,
		&createdAtText,
		&updatedAtText,
	); err != nil {
		return registry.Project{}, err
	}

	createdAt, err := parseTime(createdAtText)
	if err != nil {
		return registry.Project{}, fmt.Errorf("parse created_at for project %q: %w", project.Name, err)
	}
	updatedAt, err := parseTime(updatedAtText)
	if err != nil {
		return registry.Project{}, fmt.Errorf("parse updated_at for project %q: %w", project.Name, err)
	}

	project.CreatedAt = createdAt
	project.UpdatedAt = updatedAt

	return project, nil
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func formatNullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func storageDSN(path string) string {
	return path + "?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL"
}

func ensureFilePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat storage database: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("storage database path %q is a directory", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set storage database permissions: %w", err)
	}
	return nil
}
