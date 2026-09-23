package swarm

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

type jobCursor struct {
	TenantID, ProjectID, AgentID, JobID string
	Seq                                 int64
}

// DependencyForOwner resolves runtime-owned identity for a fresh owner action.
// Workers and stale instances receive no dependency details.
func (s *Store) DependencyForOwner(ctx context.Context, p Principal, id string) (Dependency, error) {
	var out Dependency
	if !uuid(id) {
		return out, wireError(400, "invalid_dependency")
	}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, p); err != nil {
			return err
		}
		d, err := dependency(ctx, tx, id)
		if err != nil {
			return err
		}
		f, err := feature(ctx, tx, d.FeatureID)
		if err != nil {
			return err
		}
		if f.OwnerAgentID != p.AgentID || f.TenantID != s.cfg.TenantID || f.ProjectID != s.cfg.ProjectID {
			return wireError(404, "not_found")
		}
		out = d
		return nil
	})
	return out, err
}

func (s *Store) job(ctx context.Context, p Principal, id string, limit int, cursor string) (JobView, error) {
	var out JobView
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, p); err != nil {
			return err
		}
		var fid string
		if err := tx.QueryRowContext(ctx, "SELECT feature_id FROM jobs WHERE id=?", id).Scan(&fid); err != nil {
			return wireError(404, "not_found")
		}
		f, err := feature(ctx, tx, fid)
		if err != nil {
			return err
		}
		filter := ""
		if f.OwnerAgentID != p.AgentID {
			filter = p.AgentID
		}
		var after int64
		if cursor != "" {
			b, err := base64.RawURLEncoding.DecodeString(cursor)
			if err != nil {
				return wireError(400, "invalid_cursor")
			}
			var c jobCursor
			if err = json.Unmarshal(b, &c); err != nil || c.TenantID != f.TenantID || c.ProjectID != f.ProjectID || c.AgentID != p.AgentID || c.JobID != id {
				return wireError(400, "invalid_cursor")
			}
			after = c.Seq
		}
		out, err = jobView(ctx, tx, id, filter, limit, after)
		if err == nil && filter == "" {
			var raw []byte
			scan := tx.QueryRowContext(ctx, "SELECT data FROM dependencies WHERE feature_id=? AND job_id=? ORDER BY rowid DESC LIMIT 1", fid, id).Scan(&raw)
			if scan == nil {
				var d Dependency
				if json.Unmarshal(raw, &d) == nil {
					out.Dependency = &d
				}
			} else if scan != sql.ErrNoRows {
				return scan
			}
		}
		if err == nil && out.NextCursor != nil {
			var last int64
			_, _ = fmt.Sscan(*out.NextCursor, &last)
			encoded := base64.RawURLEncoding.EncodeToString(mustJSON(jobCursor{f.TenantID, f.ProjectID, p.AgentID, id, last}))
			out.NextCursor = &encoded
		}
		return err
	})
	return out, err
}
func jobView(ctx context.Context, tx *sql.Tx, id, agent string, limit int, after int64) (JobView, error) {
	out := JobView{JobID: id, Attempts: []Attempt{}}
	if err := tx.QueryRowContext(ctx, "SELECT feature_id,current_attempt_id FROM jobs WHERE id=?", id).Scan(&out.FeatureID, &out.CurrentAttemptID); err != nil {
		return out, wireError(404, "not_found")
	}
	// A worker cannot infer the new attempt identity assigned to another worker.
	if agent != "" {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE job_id=? AND agent_id=?", id, agent).Scan(&count); err != nil {
			return out, err
		}
		if count == 0 {
			return out, wireError(404, "not_found")
		}
		var assigned string
		if err := tx.QueryRowContext(ctx, "SELECT agent_id FROM attempts WHERE id=?", out.CurrentAttemptID).Scan(&assigned); err != nil {
			return out, err
		}
		if assigned != agent {
			out.CurrentAttemptID = ""
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT seq,data FROM attempts WHERE job_id=? AND (?='' OR agent_id=?) AND seq>? ORDER BY seq LIMIT ?", id, agent, agent, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var seq int64
		var b []byte
		if err = rows.Scan(&seq, &b); err != nil {
			return out, err
		}
		if len(out.Attempts) == limit {
			c := fmt.Sprint(last)
			out.NextCursor = &c
			break
		}
		var a attemptRecord
		if err = json.Unmarshal(b, &a); err != nil {
			return out, err
		}
		out.Attempts = append(out.Attempts, a.Attempt)
		last = seq
	}
	return out, rows.Err()
}
func (s *Store) History(ctx context.Context, id string) (History, error) {
	return history(ctx, s.db, id, s.now())
}

// ReadHistory opens a coherent read-only snapshot while the coordinator owns its state lock.
// It is an operator filesystem API, deliberately not exposed over the agent HTTP API.
func ReadHistory(ctx context.Context, path, id string) (History, error) {
	uri := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite3", uri.String()+"?mode=ro&_query_only=1&_busy_timeout=5000")
	if err != nil {
		return History{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return history(ctx, db, id, time.Now())
}
func history(ctx context.Context, db *sql.DB, id string, now time.Time) (History, error) {
	out := History{Jobs: []JobView{}, Turns: []OwnerTurn{}, Messages: []Delivery{}, Audit: []AuditEvent{}, Agents: []AgentStatus{}, SlackOutbox: []SlackDelivery{}}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out.Feature, err = feature(ctx, tx, id)
	if err != nil {
		return out, err
	}
	var version int
	if err = tx.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&version); err != nil {
		return out, err
	}
	if version != 1 && version != 2 {
		return out, fmt.Errorf("unsupported schema version")
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM jobs WHERE feature_id=? ORDER BY rowid", id)
	if err != nil {
		return out, err
	}
	var jobs []string
	for rows.Next() {
		var jid string
		if err = rows.Scan(&jid); err != nil {
			rows.Close()
			return out, err
		}
		jobs = append(jobs, jid)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	for _, jid := range jobs {
		j, err := jobView(ctx, tx, jid, "", int(^uint(0)>>2), 0)
		if err != nil {
			return out, err
		}
		out.Jobs = append(out.Jobs, j)
	}
	rows, err = tx.QueryContext(ctx, "SELECT data FROM owner_turns WHERE feature_id=? ORDER BY rowid", id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return out, err
		}
		var t turnRecord
		if err = json.Unmarshal(b, &t); err != nil {
			rows.Close()
			return out, err
		}
		out.Turns = append(out.Turns, t.OwnerTurn)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, "SELECT canonical,receipt FROM messages WHERE feature_id=? ORDER BY id", id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var b, r []byte
		if err = rows.Scan(&b, &r); err != nil {
			rows.Close()
			return out, err
		}
		var e Envelope
		var receipt Receipt
		if err = json.Unmarshal(b, &e); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal(r, &receipt); err != nil {
			rows.Close()
			return out, err
		}
		out.Messages = append(out.Messages, Delivery{e, receipt.MailboxSeq, receipt.ReceivedAt})
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, "SELECT seq,at,agent_id,instance_id,feature_id,message_id,job_id,attempt_id,turn_id,event,code FROM audit WHERE feature_id=? OR feature_id='' ORDER BY seq", id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e AuditEvent
		if err = rows.Scan(&e.Seq, &e.At, &e.AgentID, &e.InstanceID, &e.FeatureID, &e.MessageID, &e.JobID, &e.AttemptID, &e.TurnID, &e.Event, &e.Code); err != nil {
			rows.Close()
			return out, err
		}
		out.Audit = append(out.Audit, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, "SELECT agent_id,instance_id,heartbeat FROM agents ORDER BY agent_id")
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var a AgentStatus
		if err = rows.Scan(&a.AgentID, &a.InstanceID, &a.LastHeartbeat); err != nil {
			rows.Close()
			return out, err
		}
		a.Status = "unreachable"
		if a.LastHeartbeat != nil {
			last, _ := time.Parse(time.RFC3339Nano, *a.LastHeartbeat)
			if now.Sub(last) < 30*time.Second {
				a.Status = "online"
			}
		}
		out.Agents = append(out.Agents, a)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, "SELECT data FROM slack_outbox WHERE feature_id=? ORDER BY rowid", id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return out, err
		}
		var d SlackDelivery
		if err = json.Unmarshal(b, &d); err != nil {
			rows.Close()
			return out, err
		}
		out.SlackOutbox = append(out.SlackOutbox, d)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, "SELECT data FROM dependencies WHERE feature_id=? ORDER BY rowid", id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var d Dependency
		if err = json.Unmarshal(raw, &d); err != nil {
			break
		}
		out.Dependencies = append(out.Dependencies, d)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT p.data FROM human_projections p JOIN dependencies d ON d.id=p.dependency_id WHERE d.feature_id=? ORDER BY p.rowid", id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var p HumanProjection
		if err = json.Unmarshal(raw, &p); err != nil {
			break
		}
		out.HumanRequests = append(out.HumanRequests, p)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	if version == 1 {
		return out, tx.Commit()
	}
	var barrierReason string
	barrierErr := tx.QueryRowContext(ctx, "SELECT reason FROM recovery_barriers WHERE feature_id=?", id).Scan(&barrierReason)
	if barrierErr == nil {
		out.RecoveryBarrier = barrierReason
	} else if barrierErr != sql.ErrNoRows {
		return out, barrierErr
	}
	requestIDs := map[string]bool{}
	for _, d := range out.Dependencies {
		if d.RequestID != "" {
			requestIDs[d.RequestID] = true
		}
	}
	rows, err = tx.QueryContext(ctx, "SELECT operation_id,request_id,kind,status,attempts,next_at,reason FROM backend_sync_operations ORDER BY rowid")
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v HumanSyncStatus
		if err = rows.Scan(&v.OperationID, &v.RequestID, &v.Kind, &v.Status, &v.Attempts, &v.NextAt, &v.Reason); err != nil {
			break
		}
		if requestIDs[v.RequestID] {
			out.HumanSync = append(out.HumanSync, v)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}
