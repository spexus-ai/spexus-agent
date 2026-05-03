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
			diagnostic_context TEXT NOT NULL DEFAULT '',
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
			channel_id TEXT NOT NULL,
			project_name TEXT NOT NULL,
			session_key TEXT NOT NULL,
			thread_ts TEXT NOT NULL DEFAULT '',
			command_text TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			diagnostic_context TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			started_at TEXT,
			updated_at TEXT NOT NULL,
			completed_at TEXT,
			FOREIGN KEY(project_name) REFERENCES projects(name) ON UPDATE CASCADE ON DELETE RESTRICT
		)`,
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
	if err := migrateExecutionsTable(ctx, tx); err != nil {
		return err
	}
	if err := ensureExecutionIndexes(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema bootstrap: %w", err)
	}

	return nil
}

func (r *RuntimeRepository) CreateExecution(ctx context.Context, request runtimemodel.ExecutionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.ExecutionID == "" {
		return fmt.Errorf("execution id is required")
	}
	if request.SourceType == "" {
		return fmt.Errorf("source type is required")
	}
	if request.DeliveryID == "" {
		return fmt.Errorf("delivery id is required")
	}
	if request.ChannelID == "" {
		return fmt.Errorf("channel id is required")
	}
	if request.ProjectName == "" {
		return fmt.Errorf("project name is required")
	}
	if request.SessionKey == "" {
		return fmt.Errorf("session key is required")
	}

	if request.Status == "" {
		request.Status = runtimemodel.ExecutionStateQueued
	}
	if request.Status != runtimemodel.ExecutionStateQueued {
		return fmt.Errorf("execution status must start as %q", runtimemodel.ExecutionStateQueued)
	}
	if err := validateExecutionStatus(request.Status); err != nil {
		return err
	}

	now := time.Now().UTC()
	if request.CreatedAt.IsZero() {
		request.CreatedAt = now
	}
	if request.UpdatedAt.IsZero() {
		request.UpdatedAt = request.CreatedAt
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO executions (
			execution_id, source_type, delivery_id, channel_id, project_name, session_key,
			thread_ts, command_text, status, diagnostic_context, created_at, started_at, updated_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		request.ExecutionID,
		request.SourceType,
		request.DeliveryID,
		request.ChannelID,
		request.ProjectName,
		request.SessionKey,
		request.ThreadTS,
		request.CommandText,
		request.Status,
		request.DiagnosticContext,
		request.CreatedAt.UTC().Format(time.RFC3339Nano),
		formatNullableTime(request.StartedAt),
		request.UpdatedAt.UTC().Format(time.RFC3339Nano),
		formatNullableTime(request.CompletedAt),
	)
	if err != nil {
		return fmt.Errorf("create execution %q: %w", request.ExecutionID, err)
	}

	return nil
}

func (r *RuntimeRepository) LoadExecution(ctx context.Context, executionID string) (runtimemodel.ExecutionRequest, error) {
	if err := ctx.Err(); err != nil {
		return runtimemodel.ExecutionRequest{}, err
	}
	if executionID == "" {
		return runtimemodel.ExecutionRequest{}, fmt.Errorf("execution id is required")
	}

	row := r.db.QueryRowContext(ctx, `
		SELECT execution_id, source_type, delivery_id, channel_id, project_name, session_key,
			thread_ts, command_text, status, diagnostic_context, created_at, started_at, updated_at, completed_at
		FROM executions
		WHERE execution_id = ?
	`, executionID)

	execution, err := scanExecution(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimemodel.ExecutionRequest{}, ErrNotFound
		}
		return runtimemodel.ExecutionRequest{}, fmt.Errorf("load execution %q: %w", executionID, err)
	}

	return execution, nil
}

func (r *RuntimeRepository) ListExecutions(ctx context.Context, statuses []runtimemodel.ExecutionLifecycleState) ([]runtimemodel.ExecutionRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
		SELECT execution_id, source_type, delivery_id, channel_id, project_name, session_key,
			thread_ts, command_text, status, diagnostic_context, created_at, started_at, updated_at, completed_at
		FROM executions
	`
	args := make([]any, 0, len(statuses))
	if len(statuses) > 0 {
		placeholders := make([]string, 0, len(statuses))
		for _, status := range statuses {
			if err := validateExecutionStatus(status); err != nil {
				return nil, err
			}
			placeholders = append(placeholders, "?")
			args = append(args, status)
		}
		query += " WHERE status IN (" + strings.Join(placeholders, ", ") + ")"
	}
	query += " ORDER BY created_at ASC, execution_id ASC"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list executions: %w", err)
	}
	defer rows.Close()

	executions := make([]runtimemodel.ExecutionRequest, 0)
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			return nil, fmt.Errorf("list executions: %w", err)
		}
		executions = append(executions, execution)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list executions: %w", err)
	}

	return executions, nil
}

func (r *RuntimeRepository) UpdateExecutionState(ctx context.Context, state runtimemodel.ExecutionState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.ExecutionID == "" {
		return fmt.Errorf("execution id is required")
	}
	if err := validateExecutionStatus(state.Status); err != nil {
		return err
	}

	now := time.Now().UTC()
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = now
	}
	if state.Status == runtimemodel.ExecutionStateRunning && state.StartedAt == nil {
		state.StartedAt = timePtr(state.UpdatedAt)
	}
	if isTerminalExecutionState(state.Status) && state.CompletedAt == nil {
		state.CompletedAt = timePtr(state.UpdatedAt)
	}

	result, err := r.db.ExecContext(ctx, `
		UPDATE executions
		SET status = ?, diagnostic_context = ?, started_at = COALESCE(?, started_at), updated_at = ?, completed_at = COALESCE(?, completed_at)
		WHERE execution_id = ?
	`,
		state.Status,
		state.DiagnosticContext,
		formatNullableTime(state.StartedAt),
		state.UpdatedAt.UTC().Format(time.RFC3339Nano),
		formatNullableTime(state.CompletedAt),
		state.ExecutionID,
	)
	if err != nil {
		return fmt.Errorf("update execution state %q: %w", state.ExecutionID, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update execution state %q rows affected: %w", state.ExecutionID, err)
	}
	if rowsAffected == 0 {
		return ErrNotFound
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
			source_type, delivery_id, received_at, processed_at, status, diagnostic_context
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_type, delivery_id) DO UPDATE SET
			received_at = excluded.received_at,
			processed_at = excluded.processed_at,
			status = excluded.status,
			diagnostic_context = excluded.diagnostic_context
	`,
		dedupe.SourceType,
		dedupe.DeliveryID,
		dedupe.ReceivedAt.UTC().Format(time.RFC3339Nano),
		formatNullableTime(dedupe.ProcessedAt),
		dedupe.Status,
		dedupe.DiagnosticContext,
	)
	if err != nil {
		return fmt.Errorf("save event dedupe %s/%q: %w", dedupe.SourceType, dedupe.DeliveryID, err)
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
		SELECT source_type, delivery_id, received_at, processed_at, status, diagnostic_context
		FROM events_dedupe
		WHERE source_type = ? AND delivery_id = ?
	`, sourceType, deliveryID)

	var dedupe runtimemodel.EventDedupe
	var receivedAtText string
	var processedAtText sql.NullString
	if err := row.Scan(&dedupe.SourceType, &dedupe.DeliveryID, &receivedAtText, &processedAtText, &dedupe.Status, &dedupe.DiagnosticContext); err != nil {
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

func migrateEventDedupeTable(ctx context.Context, tx *sql.Tx) error {
	hasSourceType, err := tableColumnExists(ctx, tx, "events_dedupe", "source_type")
	if err != nil {
		return fmt.Errorf("inspect events_dedupe source_type column: %w", err)
	}
	hasDeliveryID, err := tableColumnExists(ctx, tx, "events_dedupe", "delivery_id")
	if err != nil {
		return fmt.Errorf("inspect events_dedupe delivery_id column: %w", err)
	}
	hasDiagnosticContext, err := tableColumnExists(ctx, tx, "events_dedupe", "diagnostic_context")
	if err != nil {
		return fmt.Errorf("inspect events_dedupe diagnostic_context column: %w", err)
	}
	if hasSourceType && hasDeliveryID {
		if hasDiagnosticContext {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE events_dedupe ADD COLUMN diagnostic_context TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add events_dedupe diagnostic_context column: %w", err)
		}
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS events_dedupe_v2 (
			source_type TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			received_at TEXT NOT NULL,
			processed_at TEXT,
			status TEXT NOT NULL DEFAULT '',
			diagnostic_context TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(source_type, delivery_id)
		)
	`); err != nil {
		return fmt.Errorf("create events_dedupe_v2: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO events_dedupe_v2 (source_type, delivery_id, received_at, processed_at, status, diagnostic_context)
		SELECT 'mention', slack_event_id, received_at, processed_at, status, ''
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

func migrateExecutionsTable(ctx context.Context, tx *sql.Tx) error {
	columns, err := tableColumns(ctx, tx, "executions")
	if err != nil {
		return fmt.Errorf("inspect executions schema: %w", err)
	}
	if len(columns) == 0 {
		return nil
	}
	if columns["session_key"] && columns["diagnostic_context"] {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS executions_v2 (
			execution_id TEXT PRIMARY KEY,
			source_type TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			project_name TEXT NOT NULL,
			session_key TEXT NOT NULL,
			thread_ts TEXT NOT NULL DEFAULT '',
			command_text TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			diagnostic_context TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			started_at TEXT,
			updated_at TEXT NOT NULL,
			completed_at TEXT,
			FOREIGN KEY(project_name) REFERENCES projects(name) ON UPDATE CASCADE ON DELETE RESTRICT
		)
	`); err != nil {
		return fmt.Errorf("create executions_v2 table: %w", err)
	}

	sessionKeyExpr := `CASE
		WHEN TRIM(COALESCE(thread_ts, '')) <> '' THEN 'slack-' || TRIM(thread_ts)
		WHEN TRIM(COALESCE(channel_id, '')) <> '' THEN 'legacy-channel-' || TRIM(channel_id)
		ELSE 'legacy-execution-' || execution_id
	END`
	if columns["session_key"] {
		sessionKeyExpr = `COALESCE(NULLIF(TRIM(session_key), ''), ` + sessionKeyExpr + `)`
	}

	diagnosticContextExpr := `''`
	if columns["diagnostic_context"] {
		diagnosticContextExpr = `COALESCE(diagnostic_context, '')`
	}

	threadTSExpr := `''`
	if columns["thread_ts"] {
		threadTSExpr = `COALESCE(thread_ts, '')`
	}

	commandTextExpr := `''`
	if columns["command_text"] {
		commandTextExpr = `COALESCE(command_text, '')`
	}

	statusExpr := `'queued'`
	if columns["status"] {
		statusExpr = `status`
	}

	startedAtExpr := `NULL`
	if columns["started_at"] {
		startedAtExpr = `started_at`
	}

	updatedAtExpr := `created_at`
	if columns["updated_at"] {
		updatedAtExpr = `updated_at`
	}

	completedAtExpr := `NULL`
	if columns["completed_at"] {
		completedAtExpr = `completed_at`
	}

	insertStmt := fmt.Sprintf(`
		INSERT INTO executions_v2 (
			execution_id, source_type, delivery_id, channel_id, project_name, session_key,
			thread_ts, command_text, status, diagnostic_context, created_at, started_at, updated_at, completed_at
		)
		SELECT
			execution_id,
			source_type,
			delivery_id,
			channel_id,
			project_name,
			%s,
			%s,
			%s,
			%s,
			%s,
			created_at,
			%s,
			%s,
			%s
		FROM executions
	`, sessionKeyExpr, threadTSExpr, commandTextExpr, statusExpr, diagnosticContextExpr, startedAtExpr, updatedAtExpr, completedAtExpr)
	if _, err := tx.ExecContext(ctx, insertStmt); err != nil {
		return fmt.Errorf("copy legacy executions into executions_v2: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE executions`); err != nil {
		return fmt.Errorf("drop legacy executions table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE executions_v2 RENAME TO executions`); err != nil {
		return fmt.Errorf("rename executions_v2 table: %w", err)
	}

	return nil
}

func ensureExecutionIndexes(ctx context.Context, tx *sql.Tx) error {
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_executions_project_name ON executions(project_name)`,
		`CREATE INDEX IF NOT EXISTS idx_executions_session_key ON executions(session_key)`,
		`CREATE INDEX IF NOT EXISTS idx_executions_status ON executions(status)`,
		`CREATE INDEX IF NOT EXISTS idx_executions_source_delivery ON executions(source_type, delivery_id)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("ensure execution index: %w", err)
		}
	}
	return nil
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return columns, nil
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

func scanExecution(scanner interface {
	Scan(dest ...any) error
}) (runtimemodel.ExecutionRequest, error) {
	var execution runtimemodel.ExecutionRequest
	var createdAtText string
	var updatedAtText string
	var startedAtText sql.NullString
	var completedAtText sql.NullString

	if err := scanner.Scan(
		&execution.ExecutionID,
		&execution.SourceType,
		&execution.DeliveryID,
		&execution.ChannelID,
		&execution.ProjectName,
		&execution.SessionKey,
		&execution.ThreadTS,
		&execution.CommandText,
		&execution.Status,
		&execution.DiagnosticContext,
		&createdAtText,
		&startedAtText,
		&updatedAtText,
		&completedAtText,
	); err != nil {
		return runtimemodel.ExecutionRequest{}, err
	}

	createdAt, err := parseTime(createdAtText)
	if err != nil {
		return runtimemodel.ExecutionRequest{}, fmt.Errorf("parse created_at for execution %q: %w", execution.ExecutionID, err)
	}
	updatedAt, err := parseTime(updatedAtText)
	if err != nil {
		return runtimemodel.ExecutionRequest{}, fmt.Errorf("parse updated_at for execution %q: %w", execution.ExecutionID, err)
	}

	execution.CreatedAt = createdAt
	execution.UpdatedAt = updatedAt

	if startedAtText.Valid {
		startedAt, err := parseTime(startedAtText.String)
		if err != nil {
			return runtimemodel.ExecutionRequest{}, fmt.Errorf("parse started_at for execution %q: %w", execution.ExecutionID, err)
		}
		execution.StartedAt = &startedAt
	}
	if completedAtText.Valid {
		completedAt, err := parseTime(completedAtText.String)
		if err != nil {
			return runtimemodel.ExecutionRequest{}, fmt.Errorf("parse completed_at for execution %q: %w", execution.ExecutionID, err)
		}
		execution.CompletedAt = &completedAt
	}

	return execution, nil
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

func validateExecutionStatus(status runtimemodel.ExecutionLifecycleState) error {
	switch status {
	case runtimemodel.ExecutionStateQueued,
		runtimemodel.ExecutionStateRunning,
		runtimemodel.ExecutionStateSucceeded,
		runtimemodel.ExecutionStateFailed,
		runtimemodel.ExecutionStateCancelled:
		return nil
	case "":
		return fmt.Errorf("execution status is required")
	default:
		return fmt.Errorf("unsupported execution status %q", status)
	}
}

func isTerminalExecutionState(status runtimemodel.ExecutionLifecycleState) bool {
	switch status {
	case runtimemodel.ExecutionStateSucceeded,
		runtimemodel.ExecutionStateFailed,
		runtimemodel.ExecutionStateCancelled:
		return true
	default:
		return false
	}
}

func timePtr(value time.Time) *time.Time {
	value = value.UTC()
	return &value
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
