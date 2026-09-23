package swarmrunner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type Journal struct {
	db   *sql.DB
	lock *os.File
}
type pending struct {
	ID     int64
	Seq    int64
	Kind   string
	Path   string
	Body   json.RawMessage
	Status string
	Code   string
}
type InputRecord struct {
	Seq      int64  `json:"mailbox_seq"`
	Type     string `json:"type"`
	State    string `json:"state"`
	TurnID   string `json:"turn_id"`
	Launches int    `json:"launch_count"`
	Error    string `json:"error"`
}
type OutboxRecord struct {
	Seq       int64  `json:"mailbox_seq"`
	Kind      string `json:"kind"`
	Type      string `json:"type,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Status    string `json:"status"`
	Code      string `json:"error_code,omitempty"`
}
type JournalHistory struct {
	Outbox  []OutboxRecord `json:"outbox"`
	Inputs  []InputRecord  `json:"inputs"`
	Pending int            `json:"pending_outbox"`
}

func OpenJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, e := os.OpenFile(filepath.Join(dir, "runner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("runner instance already active")
	}
	db, e := sql.Open("sqlite3", "file:"+filepath.Join(dir, "runner.db")+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=5000")
	if e != nil {
		f.Close()
		return nil, e
	}
	db.SetMaxOpenConns(1)
	j := &Journal{db: db, lock: f}
	_, e = db.Exec(`CREATE TABLE IF NOT EXISTS schema_version(version INTEGER NOT NULL); INSERT INTO schema_version SELECT 1 WHERE NOT EXISTS(SELECT 1 FROM schema_version); CREATE TABLE IF NOT EXISTS inbox(seq INTEGER PRIMARY KEY,body BLOB NOT NULL,type TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'received',turn_id TEXT NOT NULL DEFAULT '',launches INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',output BLOB,model_output TEXT); CREATE TABLE IF NOT EXISTS outbox(id INTEGER PRIMARY KEY AUTOINCREMENT,seq INTEGER NOT NULL REFERENCES inbox(seq),kind TEXT NOT NULL,path TEXT NOT NULL,body BLOB NOT NULL,status TEXT NOT NULL DEFAULT 'pending',code TEXT NOT NULL DEFAULT '',receipt BLOB,UNIQUE(kind,path,body)); CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL); CREATE TABLE IF NOT EXISTS instances(instance_id TEXT PRIMARY KEY);`)
	var version int
	if e == nil {
		e = db.QueryRow(`SELECT version FROM schema_version`).Scan(&version)
		if e == nil && version != 1 {
			e = errors.New("runner schema mismatch")
		}
	}
	if e != nil {
		j.Close()
		return nil, e
	}
	return j, nil
}
func (j *Journal) Close() error {
	e := j.db.Close()
	_ = syscall.Flock(int(j.lock.Fd()), syscall.LOCK_UN)
	_ = j.lock.Close()
	return e
}
func (j *Journal) bind(instance string) error {
	var count int
	if e := j.db.QueryRow(`SELECT count(*) FROM instances WHERE instance_id=?`, instance).Scan(&count); e != nil {
		return e
	}
	if count > 0 {
		return errors.New("instance_id already used; offline reconciliation and a new instance_id required")
	}
	_, e := j.db.Exec(`INSERT INTO instances(instance_id) VALUES(?)`, instance)
	return e
}
func (j *Journal) identity(c Config) error {
	value := c.TenantID + "/" + c.ProjectID + "/" + c.AgentID + "/" + c.Role
	if c.WireVersion == 2 {
		value += "/wire2"
	}
	_, e := j.db.Exec(`INSERT OR IGNORE INTO metadata(key,value) VALUES('identity',?)`, value)
	if e != nil {
		return e
	}
	var actual string
	if e = j.db.QueryRow(`SELECT value FROM metadata WHERE key='identity'`).Scan(&actual); e != nil {
		return e
	}
	if actual != value {
		return errors.New("journal belongs to another scope or agent")
	}
	return nil
}

func (j *Journal) recover() error {
	_, e := j.db.Exec(`UPDATE inbox SET state='interrupted',error='unknown_launch_requires_reconciliation' WHERE state='starting'`)
	return e
}
func (j *Journal) receive(d swarm.Delivery) error {
	b, e := json.Marshal(d)
	if e != nil {
		return e
	}
	_, e = j.db.Exec(`INSERT OR IGNORE INTO inbox(seq,body,type) VALUES(?,?,?)`, d.MailboxSeq, b, d.Type)
	if e != nil {
		return e
	}
	var saved []byte
	e = j.db.QueryRow(`SELECT body FROM inbox WHERE seq=?`, d.MailboxSeq).Scan(&saved)
	if e != nil {
		return e
	}
	var prior swarm.Delivery
	if e = json.Unmarshal(saved, &prior); e != nil {
		return e
	}
	if string(saved) != string(b) {
		return errors.New("mailbox_seq payload changed")
	}
	return nil
}
func (j *Journal) next() (swarm.Delivery, bool, error) {
	var b []byte
	e := j.db.QueryRow(`SELECT body FROM inbox WHERE state='received' AND type NOT IN ('task.cancel','turn.cancel') ORDER BY seq LIMIT 1`).Scan(&b)
	if e == sql.ErrNoRows {
		return swarm.Delivery{}, false, nil
	}
	if e != nil {
		return swarm.Delivery{}, false, e
	}
	var d swarm.Delivery
	e = json.Unmarshal(b, &d)
	return d, true, e
}
func (j *Journal) state(seq int64, state, code string) error {
	_, e := j.db.Exec(`UPDATE inbox SET state=?,error=? WHERE seq=?`, state, code, seq)
	return e
}
func (j *Journal) starting(seq int64, turn string) error {
	r, e := j.db.Exec(`UPDATE inbox SET state='starting',turn_id=? WHERE seq=? AND state='received'`, turn, seq)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e == nil && n != 1 {
		return errors.New("input already started")
	}
	return e
}
func (j *Journal) launch(seq int64) error {
	_, e := j.db.Exec(`UPDATE inbox SET launches=launches+1 WHERE seq=? AND state='starting'`, seq)
	return e
}
func (j *Journal) modelOutput(seq int64, raw string) error {
	if len(raw) > swarm.MaxEnvelopeBytes {
		raw = "[model_output_too_large]"
	}
	_, err := j.db.Exec(`UPDATE inbox SET model_output=? WHERE seq=?`, raw, seq)
	return err
}
func (j *Journal) queue(seq int64, kind, path string, body any) error {
	b, e := json.Marshal(body)
	if e != nil {
		return e
	}
	_, e = j.db.Exec(`INSERT OR IGNORE INTO outbox(seq,kind,path,body) VALUES(?,?,?,?)`, seq, kind, path, b)
	return e
}
func (j *Journal) output(seq int64, output any, messages []swarm.Envelope, finish *swarm.OwnerFinishRequest, turn string) error {
	b, e := json.Marshal(output)
	if e != nil {
		return e
	}
	tx, e := j.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`UPDATE inbox SET state='output',output=? WHERE seq=?`, b, seq); e != nil {
		return e
	}
	for _, m := range messages {
		raw, e := json.Marshal(m)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(`INSERT INTO outbox(seq,kind,path,body) VALUES(?,'message','/messages',?)`, seq, raw); e != nil {
			return e
		}
	}
	if finish != nil {
		raw, e := json.Marshal(finish)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(`INSERT INTO outbox(seq,kind,path,body) VALUES(?,'finish',?,?)`, seq, "/owner-turns/"+turn+"/finish", raw); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (j *Journal) pending() (pending, bool, error) {
	var p pending
	e := j.db.QueryRow(`SELECT id,seq,kind,path,body,status,code FROM outbox WHERE status='pending' ORDER BY id LIMIT 1`).Scan(&p.ID, &p.Seq, &p.Kind, &p.Path, &p.Body, &p.Status, &p.Code)
	if e == sql.ErrNoRows {
		return p, false, nil
	}
	return p, e == nil, e
}
func (j *Journal) settle(id int64, status, code string, receipt any) error {
	b, e := json.Marshal(receipt)
	if e != nil {
		return e
	}
	_, e = j.db.Exec(`UPDATE outbox SET status=?,code=?,receipt=? WHERE id=?`, status, code, b, id)
	return e
}
func (j *Journal) actions(seq int64) ([]swarm.ActionReceipt, error) {
	rows, e := j.db.Query(`SELECT body,status,code FROM outbox WHERE seq=? AND kind='message' ORDER BY id`, seq)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []swarm.ActionReceipt{}
	for rows.Next() {
		var raw []byte
		var status, code string
		if e = rows.Scan(&raw, &status, &code); e != nil {
			return nil, e
		}
		var m swarm.Envelope
		if e = json.Unmarshal(raw, &m); e != nil {
			return nil, e
		}
		if m.OwnerTurnID == "" {
			continue
		}
		r := swarm.ActionReceipt{MessageID: m.MessageID, Status: status}
		if code != "" {
			r.ErrorCode = &code
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (j *Journal) finishOutput(seq int64) error {
	_, e := j.db.Exec(`UPDATE inbox SET state='applied' WHERE seq=? AND state='output' AND NOT EXISTS(SELECT 1 FROM outbox WHERE seq=? AND status='pending')`, seq, seq)
	return e
}
func (j *Journal) cancelFor(d swarm.Delivery) (*swarm.Delivery, error) {
	rows, e := j.db.Query(`SELECT body FROM inbox WHERE type IN ('task.cancel','turn.cancel') ORDER BY seq DESC`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		var x swarm.Delivery
		if e = json.Unmarshal(b, &x); e != nil {
			return nil, e
		}
		if x.FeatureID == d.FeatureID && ((x.Type == "task.cancel" && x.AttemptID == d.AttemptID) || (x.Type == "turn.cancel" && x.OwnerTurnID == d.OwnerTurnID)) {
			return &x, nil
		}
	}
	return nil, rows.Err()
}
func (j *Journal) History() (JournalHistory, error) { return readHistory(j.db) }
func ReadHistory(dir string) (JournalHistory, error) {
	db, e := sql.Open("sqlite3", "file:"+filepath.Join(dir, "runner.db")+"?mode=ro&_busy_timeout=5000")
	if e != nil {
		return JournalHistory{}, e
	}
	defer db.Close()
	return readHistory(db)
}
func readHistory(db *sql.DB) (JournalHistory, error) {
	h := JournalHistory{Inputs: []InputRecord{}}
	rows, e := db.Query(`SELECT seq,type,state,turn_id,launches,error FROM inbox ORDER BY seq`)
	if e != nil {
		return h, e
	}
	for rows.Next() {
		var r InputRecord
		if e = rows.Scan(&r.Seq, &r.Type, &r.State, &r.TurnID, &r.Launches, &r.Error); e != nil {
			rows.Close()
			return h, e
		}
		h.Inputs = append(h.Inputs, r)
	}
	e = rows.Err()
	rows.Close()
	if e == nil {
		e = db.QueryRow(`SELECT count(*) FROM outbox WHERE status='pending'`).Scan(&h.Pending)
	}
	if e != nil {
		return h, e
	}
	out, e := db.Query(`SELECT seq,kind,body,status,code FROM outbox ORDER BY id`)
	if e != nil {
		return h, e
	}
	defer out.Close()
	h.Outbox = []OutboxRecord{}
	for out.Next() {
		var x OutboxRecord
		var raw []byte
		if e = out.Scan(&x.Seq, &x.Kind, &raw, &x.Status, &x.Code); e != nil {
			return h, e
		}
		if x.Kind == "message" {
			var m swarm.Envelope
			if e = json.Unmarshal(raw, &m); e != nil {
				return h, e
			}
			x.Type = m.Type
			x.MessageID = m.MessageID
		}
		h.Outbox = append(h.Outbox, x)
	}
	return h, out.Err()
}
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
