package swarm

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db             *sql.DB
	lock           *os.File
	cfg            Config
	profiles       map[string]Profile
	now            func() time.Time
	mailboxLimit   int
	mailboxBytes   int
	mailboxReserve int
}

type attemptRecord struct {
	Attempt
	JobID      string          `json:"job_id"`
	FeatureID  string          `json:"feature_id"`
	Dispatch   DispatchPayload `json:"dispatch"`
	AcceptedAt string          `json:"accepted_at"`
	InstanceID string          `json:"instance_id"`
	Reconciled bool            `json:"reconciled"`
}
type turnRecord struct {
	OwnerTurn
	AgentID    string              `json:"agent_id"`
	InstanceID string              `json:"instance_id"`
	StartedAt  string              `json:"started_at"`
	Start      OwnerStartRequest   `json:"start"`
	Finish     *OwnerFinishRequest `json:"finish"`
}

const schema = `
CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS scope (id INTEGER PRIMARY KEY CHECK(id=1),tenant_id TEXT NOT NULL,project_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agents (agent_id TEXT PRIMARY KEY,role TEXT NOT NULL,credential_sha256 TEXT NOT NULL,profile_id TEXT NOT NULL,instance_id TEXT NOT NULL DEFAULT '',heartbeat TEXT);
CREATE TABLE IF NOT EXISTS profiles (id TEXT PRIMARY KEY,revision TEXT NOT NULL,snapshot BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS features (id TEXT PRIMARY KEY,tenant_id TEXT NOT NULL,project_id TEXT NOT NULL,owner_agent_id TEXT NOT NULL REFERENCES agents(agent_id),data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY,feature_id TEXT NOT NULL REFERENCES features(id),current_attempt_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS attempts (seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,job_id TEXT NOT NULL REFERENCES jobs(id),agent_id TEXT NOT NULL REFERENCES agents(agent_id),state TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS owner_turns (id TEXT PRIMARY KEY,feature_id TEXT NOT NULL REFERENCES features(id),agent_id TEXT NOT NULL REFERENCES agents(agent_id),input_seq INTEGER NOT NULL,state TEXT NOT NULL,data BLOB NOT NULL,UNIQUE(agent_id,input_seq));
CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY AUTOINCREMENT,sender TEXT NOT NULL,message_id TEXT NOT NULL,feature_id TEXT NOT NULL REFERENCES features(id),kind TEXT NOT NULL,job_id TEXT NOT NULL,attempt_id TEXT NOT NULL,turn_id TEXT NOT NULL,canonical BLOB NOT NULL,receipt BLOB NOT NULL,UNIQUE(sender,message_id));
CREATE TABLE IF NOT EXISTS message_aliases (sender TEXT NOT NULL,message_id TEXT NOT NULL,canonical BLOB NOT NULL,receipt BLOB NOT NULL,PRIMARY KEY(sender,message_id));
CREATE TABLE IF NOT EXISTS mailbox_counters (agent_id TEXT PRIMARY KEY REFERENCES agents(agent_id),seq INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS mailbox_delivery (agent_id TEXT NOT NULL REFERENCES agents(agent_id),seq INTEGER NOT NULL,message_row INTEGER NOT NULL REFERENCES messages(id),lane TEXT NOT NULL,acked INTEGER NOT NULL DEFAULT 0,superseded INTEGER NOT NULL DEFAULT 0,pending_notification INTEGER NOT NULL DEFAULT 0,bytes INTEGER NOT NULL,PRIMARY KEY(agent_id,seq));
CREATE TABLE IF NOT EXISTS ingress (feature_id TEXT NOT NULL REFERENCES features(id),event_id TEXT NOT NULL,payload BLOB NOT NULL,receipt BLOB NOT NULL,PRIMARY KEY(feature_id,event_id));
CREATE TABLE IF NOT EXISTS slack_outbox (id TEXT PRIMARY KEY,feature_id TEXT NOT NULL REFERENCES features(id),turn_id TEXT UNIQUE REFERENCES owner_turns(id),status TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS audit (seq INTEGER PRIMARY KEY AUTOINCREMENT,at TEXT NOT NULL,agent_id TEXT NOT NULL,instance_id TEXT NOT NULL,feature_id TEXT NOT NULL,message_id TEXT NOT NULL,job_id TEXT NOT NULL,attempt_id TEXT NOT NULL,turn_id TEXT NOT NULL,event TEXT NOT NULL,code TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS pending_mailbox ON mailbox_delivery(agent_id,lane,acked,superseded,seq);
CREATE INDEX IF NOT EXISTS phase_messages ON messages(attempt_id,kind);
`

func Open(ctx context.Context, path string, cfg Config) (*Store, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("persistent state path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("coordinator state already open: %w", err)
	}
	uri := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite3", uri.String()+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, lock: lock, cfg: cfg, profiles: map[string]Profile{}, now: time.Now, mailboxLimit: 1000, mailboxBytes: 10 * 1024 * 1024, mailboxReserve: 40}
	if err = s.bootstrap(ctx); err != nil {
		s.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		if e := s.lock.Close(); err == nil {
			err = e
		}
		s.lock = nil
	}
	return err
}
func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339Nano) }
func (s *Store) bootstrap(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	var version int
	err = tx.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, "INSERT INTO schema_version VALUES(1)")
		version = 1
	}
	if err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO scope VALUES(1,?,?)", s.cfg.TenantID, s.cfg.ProjectID); err != nil {
		return err
	}
	var tenant, project string
	if err = tx.QueryRowContext(ctx, "SELECT tenant_id,project_id FROM scope WHERE id=1").Scan(&tenant, &project); err != nil {
		return err
	}
	if tenant != s.cfg.TenantID || project != s.cfg.ProjectID {
		return fmt.Errorf("immutable state scope changed")
	}
	for _, a := range s.cfg.Agents {
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO agents(agent_id,role,credential_sha256,profile_id) VALUES(?,?,?,?)", a.AgentID, a.Role, a.CredentialSHA256, a.ProfileID)
		if err != nil {
			return err
		}
		var role, digest, profile string
		if err = tx.QueryRowContext(ctx, "SELECT role,credential_sha256,profile_id FROM agents WHERE agent_id=?", a.AgentID).Scan(&role, &digest, &profile); err != nil {
			return err
		}
		if role != a.Role || digest != a.CredentialSHA256 || profile != a.ProfileID {
			return fmt.Errorf("agent configuration changed: %s", a.AgentID)
		}
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM agents").Scan(&count); err != nil {
		return err
	}
	if count != len(s.cfg.Agents) {
		return fmt.Errorf("agent configuration changed")
	}
	for _, snap := range s.cfg.Profiles {
		var p TextProfile
		_ = json.Unmarshal(snap.Bytes, &p)
		ref := Profile{ID: p.ID, Revision: Digest(snap.Bytes), Model: p.Model, Reasoning: p.Reasoning}
		s.profiles[p.ID] = ref
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO profiles(id,revision,snapshot) VALUES(?,?,?)", p.ID, ref.Revision, []byte(snap.Bytes)); err != nil {
			return err
		}
		var rev string
		if err = tx.QueryRowContext(ctx, "SELECT revision FROM profiles WHERE id=?", p.ID).Scan(&rev); err != nil {
			return err
		}
		if rev != ref.Revision {
			return fmt.Errorf("profile snapshot changed: %s", p.ID)
		}
	}
	for _, f := range s.cfg.Features {
		if err = s.registerFeature(ctx, tx, f); err != nil {
			return err
		}
	}
	// A send accepted by Slack but not settled locally must never be blindly replayed.
	rows, err := tx.QueryContext(ctx, "SELECT data FROM slack_outbox WHERE status='sending'")
	if err != nil {
		return err
	}
	var uncertain []SlackDelivery
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return err
		}
		var d SlackDelivery
		_ = json.Unmarshal(b, &d)
		d.Status = "delivery_unknown"
		uncertain = append(uncertain, d)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, d := range uncertain {
		if d.TurnID != "" {
			t, err := turn(ctx, tx, d.TurnID)
			if err != nil {
				return err
			}
			t.ReplyStatus = "delivery_unknown"
			if err = saveTurn(ctx, tx, t); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE slack_outbox SET status=?,data=? WHERE id=?", d.Status, mustJSON(d), d.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func (s *Store) registerFeature(ctx context.Context, tx *sql.Tx, f Feature) error {
	if !uuid(f.FeatureID) || f.TenantID != s.cfg.TenantID || f.ProjectID != s.cfg.ProjectID || f.ChannelID == "" || f.ThreadTS == "" || len(f.AllowedActorIDs) == 0 {
		return wireError(400, "invalid_feature")
	}
	var role string
	if err := tx.QueryRowContext(ctx, "SELECT role FROM agents WHERE agent_id=?", f.OwnerAgentID).Scan(&role); err != nil || role != "owner" {
		return wireError(400, "invalid_owner")
	}
	var raw []byte
	err := tx.QueryRowContext(ctx, "SELECT data FROM features WHERE id=?", f.FeatureID).Scan(&raw)
	if err == nil {
		var old Feature
		_ = json.Unmarshal(raw, &old)
		f.Stopped = old.Stopped
		if string(mustJSON(old)) != string(mustJSON(f)) {
			return wireError(409, "feature_conflict")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO features VALUES(?,?,?,?,?)", f.FeatureID, f.TenantID, f.ProjectID, f.OwnerAgentID, mustJSON(f))
	return err
}
func (s *Store) RegisterFeature(ctx context.Context, f Feature) error {
	return s.transaction(ctx, func(tx *sql.Tx) error { return s.registerFeature(ctx, tx, f) })
}
func (s *Store) transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func feature(ctx context.Context, tx *sql.Tx, id string) (Feature, error) {
	var f Feature
	var b []byte
	err := tx.QueryRowContext(ctx, "SELECT data FROM features WHERE id=?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return f, wireError(404, "not_found")
	}
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	return f, err
}
func attempt(ctx context.Context, tx *sql.Tx, id string) (attemptRecord, error) {
	var a attemptRecord
	var b []byte
	err := tx.QueryRowContext(ctx, "SELECT data FROM attempts WHERE id=?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return a, wireError(404, "not_found")
	}
	if err == nil {
		err = json.Unmarshal(b, &a)
	}
	return a, err
}
func turn(ctx context.Context, tx *sql.Tx, id string) (turnRecord, error) {
	var t turnRecord
	var b []byte
	err := tx.QueryRowContext(ctx, "SELECT data FROM owner_turns WHERE id=?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return t, wireError(404, "not_found")
	}
	if err == nil {
		err = json.Unmarshal(b, &t)
	}
	return t, err
}
func saveAttempt(ctx context.Context, tx *sql.Tx, a attemptRecord) error {
	_, err := tx.ExecContext(ctx, "UPDATE attempts SET state=?,data=? WHERE id=?", a.State, mustJSON(a), a.AttemptID)
	return err
}
func saveTurn(ctx context.Context, tx *sql.Tx, t turnRecord) error {
	_, err := tx.ExecContext(ctx, "UPDATE owner_turns SET state=?,data=? WHERE id=?", t.State, mustJSON(t), t.TurnID)
	return err
}
func terminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled" || state == "interrupted"
}
func (s *Store) audit(ctx context.Context, tx *sql.Tx, p Principal, e Envelope, event, code string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO audit(at,agent_id,instance_id,feature_id,message_id,job_id,attempt_id,turn_id,event,code) VALUES(?,?,?,?,?,?,?,?,?,?)", s.stamp(), p.AgentID, p.InstanceID, e.FeatureID, e.MessageID, e.JobID, e.AttemptID, e.OwnerTurnID, event, code)
	return err
}
func (s *Store) reject(ctx context.Context, p Principal, e Envelope, err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		return
	}
	// Only safe identifiers are retained; no untrusted error text, prompt or credentials.
	if !uuid(e.MessageID) {
		e.MessageID = ""
	}
	if !uuid(e.FeatureID) {
		e.FeatureID = ""
	}
	if !uuid(e.JobID) {
		e.JobID = ""
	}
	if !uuid(e.AttemptID) {
		e.AttemptID = ""
	}
	if !uuid(e.OwnerTurnID) {
		e.OwnerTurnID = ""
	}
	_ = s.transaction(ctx, func(tx *sql.Tx) error { return s.audit(ctx, tx, p, e, "rejected", ae.Code) })
}
func (s *Store) principal(token, instance string) (Principal, error) {
	if !uuid(instance) {
		return Principal{}, wireError(401, "invalid_instance")
	}
	digest := Digest([]byte(token))
	if token == "" {
		return Principal{}, wireError(401, "unauthorized")
	}
	for _, a := range s.cfg.Agents {
		if subtle.ConstantTimeCompare([]byte(a.CredentialSHA256), []byte(digest)) == 1 {
			return Principal{a.AgentID, instance}, nil
		}
	}
	return Principal{}, wireError(401, "unauthorized")
}
func bound(ctx context.Context, tx *sql.Tx, p Principal) error {
	var instance string
	if err := tx.QueryRowContext(ctx, "SELECT instance_id FROM agents WHERE agent_id=?", p.AgentID).Scan(&instance); err != nil {
		return wireError(401, "unauthorized")
	}
	if instance == "" {
		return wireError(409, "heartbeat_required")
	}
	if instance != p.InstanceID {
		return wireError(409, "instance_conflict")
	}
	return nil
}
func (s *Store) heartbeat(ctx context.Context, p Principal, r HeartbeatRequest) (HeartbeatResponse, error) {
	var out HeartbeatResponse
	if r.InstanceID != p.InstanceID || r.ActiveAttemptID != nil && !uuid(*r.ActiveAttemptID) || r.ActiveOwnerTurnID != nil && !uuid(*r.ActiveOwnerTurnID) {
		return out, wireError(400, "invalid_heartbeat")
	}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var instance, role string
		if err := tx.QueryRowContext(ctx, "SELECT instance_id,role FROM agents WHERE agent_id=?", p.AgentID).Scan(&instance, &role); err != nil {
			return err
		}
		if instance != "" && instance != p.InstanceID {
			return wireError(409, "instance_conflict")
		}
		if r.ActiveAttemptID != nil {
			a, err := attempt(ctx, tx, *r.ActiveAttemptID)
			if err != nil || a.AssignedAgentID != p.AgentID {
				return wireError(403, "foreign_attempt")
			}
		}
		if r.ActiveOwnerTurnID != nil {
			t, err := turn(ctx, tx, *r.ActiveOwnerTurnID)
			if err != nil || t.AgentID != p.AgentID {
				return wireError(403, "foreign_turn")
			}
		}
		now := s.stamp()
		if _, err := tx.ExecContext(ctx, "UPDATE agents SET instance_id=?,heartbeat=? WHERE agent_id=?", p.InstanceID, now, p.AgentID); err != nil {
			return err
		}
		if instance == "" {
			if err := s.audit(ctx, tx, p, Envelope{}, "instance_bound", ""); err != nil {
				return err
			}
		}
		out = HeartbeatResponse{p.AgentID, p.InstanceID, "online", now}
		return nil
	})
	return out, err
}
func allowedActor(f Feature, actor string) bool {
	for _, a := range f.AllowedActorIDs {
		if a == actor {
			return true
		}
	}
	return false
}
func safeText(s string, max int) bool { return strings.TrimSpace(s) != "" && len(s) <= max }
