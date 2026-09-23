package swarm

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type humanBackendView struct {
	ID                string        `json:"id"`
	TenantID          string        `json:"tenant_id"`
	ProjectID         string        `json:"project_id"`
	FeatureID         string        `json:"feature_id"`
	AllowedResponders []string      `json:"allowed_responders"`
	Options           []HumanOption `json:"options"`
	Slack             struct {
		WorkspaceID string `json:"workspace_id"`
		ChannelID   string `json:"channel_id"`
		ThreadTS    string `json:"thread_ts"`
	} `json:"slack"`
	Dependency struct {
		ID string `json:"id"`
	} `json:"dependency"`
	State    string `json:"state"`
	Revision int    `json:"revision"`
	Terminal *struct {
		ID       string          `json:"id"`
		Kind     string          `json:"kind"`
		Response json.RawMessage `json:"response"`
		Source   json.RawMessage `json:"source"`
	} `json:"terminal"`
}

// HumanAnswerInput is trusted normalized Slack provenance, never model text.
type HumanAnswerInput struct {
	RequestID   string `json:"request_id"`
	Kind        string `json:"kind"`
	OptionID    string `json:"option_id,omitempty"`
	Text        string `json:"text"`
	WorkspaceID string `json:"workspace_id"`
	ChannelID   string `json:"channel_id"`
	ThreadTS    string `json:"thread_ts"`
	MessageTS   string `json:"message_ts"`
	ActorID     string `json:"actor_id"`
	EventID     string `json:"event_id,omitempty"`
}

func (s *Store) RecordHumanAnswer(ctx context.Context, in HumanAnswerInput) (string, bool, error) {
	if !uuid(in.RequestID) || in.WorkspaceID == "" || in.ChannelID == "" || in.ThreadTS == "" || in.MessageTS == "" || in.ActorID == "" || len(in.Text) > 16*1024 {
		return "", false, wireError(400, "invalid_answer")
	}
	if in.Kind != "answer" && in.Kind != "deny" {
		return "", false, wireError(400, "invalid_answer")
	}
	var out string
	duplicate := false
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		input, _ := canonical(mustJSON(in))
		var old, receipt []byte
		err := tx.QueryRowContext(ctx, "SELECT payload,receipt FROM source_ingress WHERE workspace_id=? AND channel_id=? AND message_ts=?", in.WorkspaceID, in.ChannelID, in.MessageTS).Scan(&old, &receipt)
		if err == nil {
			if !bytes.Equal(old, input) {
				return wireError(409, "source_conflict")
			}
			duplicate = true
			return json.Unmarshal(receipt, &out)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var raw []byte
		if err = tx.QueryRowContext(ctx, "SELECT data FROM human_projections WHERE request_id=?", in.RequestID).Scan(&raw); err != nil {
			return wireError(404, "request_not_found")
		}
		var p HumanProjection
		if err = json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if p.BackendState != "open" || p.Revision != 1 {
			return wireError(409, "request_closed")
		}
		d, err := dependency(ctx, tx, p.DependencyID)
		if err != nil {
			return err
		}
		if d.State != "human_waiting" {
			return wireError(409, "request_not_waiting")
		}
		var v humanBackendView
		if err = json.Unmarshal(p.View, &v); err != nil {
			return err
		}
		f, err := feature(ctx, tx, d.FeatureID)
		if err != nil {
			return err
		}
		if f.Stopped || !allowedActor(f, in.ActorID) || in.WorkspaceID != v.Slack.WorkspaceID || in.ChannelID != v.Slack.ChannelID || in.ThreadTS != v.Slack.ThreadTS {
			return wireError(403, "untrusted_answer")
		}
		allowed := false
		for _, a := range v.AllowedResponders {
			if a == in.ActorID {
				allowed = true
				break
			}
		}
		if !allowed {
			return wireError(403, "untrusted_answer")
		}
		if in.Kind == "answer" {
			if len(v.Options) > 0 {
				found := false
				for _, o := range v.Options {
					if o.ID == in.OptionID {
						found = true
						break
					}
				}
				if !found {
					return wireError(400, "invalid_option")
				}
			} else if in.OptionID != "" || !safeText(in.Text, 16*1024) {
				return wireError(400, "invalid_answer")
			}
		} else if in.OptionID != "" || !safeText(in.Text, 16*1024) {
			return wireError(400, "invalid_deny")
		}
		var option any
		if in.OptionID != "" {
			option = in.OptionID
		}
		out = NewID()
		body := struct {
			OperationID      string `json:"operation_id"`
			ExpectedRevision int    `json:"expected_revision"`
			Decision         struct {
				Kind     string `json:"kind"`
				OptionID any    `json:"option_id"`
				Text     string `json:"text"`
			} `json:"decision"`
			Source struct {
				WorkspaceID string  `json:"workspace_id"`
				ChannelID   string  `json:"channel_id"`
				ThreadTS    string  `json:"thread_ts"`
				MessageTS   string  `json:"message_ts"`
				ActorID     string  `json:"actor_id"`
				EventID     *string `json:"event_id"`
			} `json:"source"`
			ReceivedAt string `json:"received_at"`
		}{OperationID: out, ExpectedRevision: 1, ReceivedAt: s.stamp()}
		body.Decision.Kind = in.Kind
		body.Decision.OptionID = option
		body.Decision.Text = in.Text
		body.Source.WorkspaceID = in.WorkspaceID
		body.Source.ChannelID = in.ChannelID
		body.Source.ThreadTS = in.ThreadTS
		body.Source.MessageTS = in.MessageTS
		body.Source.ActorID = in.ActorID
		if in.EventID != "" {
			body.Source.EventID = &in.EventID
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO backend_sync_operations(operation_id,request_id,kind,status,payload) VALUES(?,?,?,?,?)", out, in.RequestID, "decision", "pending", mustJSON(body)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO source_ingress(workspace_id,channel_id,message_ts,feature_id,payload,receipt) VALUES(?,?,?,?,?,?)", in.WorkspaceID, in.ChannelID, in.MessageTS, d.FeatureID, input, mustJSON(out))
		return err
	})
	return out, duplicate, err
}

type humanEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Data          json.RawMessage `json:"data"`
}
type humanOperation struct {
	ID, RequestID, Kind, Status string
	Attempts                    int
	Payload                     []byte
	RetryAfter                  time.Duration
}
type gatewayToken struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	RefreshState string `json:"refresh_state,omitempty"`
}

var errGatewayAuth = errors.New("gateway authentication blocked")

func jwtWriter(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		UserID string `json:"user_id"`
	}
	if json.Unmarshal(b, &claims) != nil || !uuid(claims.UserID) {
		return ""
	}
	return claims.UserID
}

func (s *Store) backendClient() (*http.Client, error) {
	if s.cfg.Human == nil {
		return nil, errors.New("human backend unconfigured")
	}
	u, err := url.Parse(s.cfg.Human.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("human backend requires HTTPS URL")
	}
	t := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if s.cfg.Human.CAFile != "" {
		b, err := os.ReadFile(s.cfg.Human.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid backend CA")
		}
		t.TLSClientConfig.RootCAs = pool
	}
	return &http.Client{Transport: t, Timeout: 10 * time.Second}, nil
}
func readGatewayToken(path string) (gatewayToken, error) {
	var t gatewayToken
	info, err := os.Stat(path)
	if err != nil {
		return t, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return t, errors.New("gateway token file permissions must be 0600")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	if err = json.Unmarshal(b, &t); err != nil {
		return t, errors.New("invalid gateway token file")
	}
	if t.Token == "" {
		return t, errors.New("gateway access token missing")
	}
	return t, nil
}
func writeGatewayToken(path string, t gatewayToken) error {
	b := mustJSON(t)
	tmp := path + ".next"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Refresh is deliberately fail-closed. The backend revokes the old refresh
// credential before returning its replacement; an unknown response needs
// operator reprovisioning to the same writer rather than a blind retry.
func (s *Store) refreshGateway(ctx context.Context, c *http.Client, t gatewayToken) error {
	if t.RefreshToken == "" || t.RefreshState != "" {
		return fmt.Errorf("%w: refresh unavailable; reprovision same writer", errGatewayAuth)
	}
	path := s.cfg.Human.TokenFile
	t.RefreshState = "unknown"
	if err := writeGatewayToken(path, t); err != nil {
		return err
	}
	body := mustJSON(struct {
		RefreshToken string `json:"refresh_token"`
	}{t.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.Human.BaseURL, "/")+"/auth/refresh", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%w: refresh outcome unknown; reprovision same writer", errGatewayAuth)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%w: refresh rejected or outcome unknown; reprovision same writer", errGatewayAuth)
	}
	var next struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 128*1024)).Decode(&next); err != nil || next.Token == "" || next.RefreshToken == "" {
		return fmt.Errorf("%w: refresh response unknown; reprovision same writer", errGatewayAuth)
	}
	if jwtWriter(next.Token) != s.cfg.Human.WriterID {
		return fmt.Errorf("%w: refresh writer changed; reprovision same writer", errGatewayAuth)
	}
	return writeGatewayToken(path, gatewayToken{Token: next.Token, RefreshToken: next.RefreshToken})
}
func (s *Store) backendRequest(ctx context.Context, c *http.Client, method, path string, payload []byte) (int, []byte, error) {
	status, body, _, err := s.backendRequestWithRetryAfter(ctx, c, method, path, payload)
	return status, body, err
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		const maxSeconds = int64(1<<63-1) / int64(time.Second)
		if seconds > maxSeconds {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func (s *Store) backendRequestWithRetryAfter(ctx context.Context, c *http.Client, method, path string, payload []byte) (int, []byte, time.Duration, error) {
	t, err := readGatewayToken(s.cfg.Human.TokenFile)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("%w: credential unavailable", errGatewayAuth)
	}
	if t.RefreshState != "" {
		return 0, nil, 0, fmt.Errorf("%w: refresh outcome unknown; reprovision same writer", errGatewayAuth)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if jwtWriter(t.Token) != s.cfg.Human.WriterID {
			return 0, nil, 0, fmt.Errorf("%w: credential writer mismatch", errGatewayAuth)
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.cfg.Human.BaseURL, "/")+path, bytes.NewReader(payload))
		if err != nil {
			return 0, nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+t.Token)
		req.Header.Set("X-Tenant-ID", s.cfg.TenantID)
		req.Header.Set("X-Project-ID", s.cfg.ProjectID)
		if len(payload) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			return 0, nil, 0, err
		}
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), s.now())
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxEnvelopeBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return 0, nil, 0, readErr
		}
		if len(b) > MaxEnvelopeBytes {
			return 0, nil, 0, errors.New("backend response too large")
		}
		if resp.StatusCode == 401 && attempt == 0 {
			if err = s.refreshGateway(ctx, c, t); err != nil {
				return 0, nil, 0, err
			}
			t, err = readGatewayToken(s.cfg.Human.TokenFile)
			if err != nil {
				return 0, nil, 0, err
			}
			continue
		}
		return resp.StatusCode, b, retryAfter, nil
	}
	return 0, nil, 0, errGatewayAuth
}
func (s *Store) HumanPreflight(ctx context.Context) error {
	if s.cfg.Human == nil {
		return nil
	}
	c, err := s.backendClient()
	if err != nil {
		return err
	}
	identityStatus, identityBody, identityErr := s.backendRequest(ctx, c, http.MethodGet, "/api/v1/users/me", nil)
	if identityErr != nil {
		return identityErr
	}
	var identity struct {
		ID string `json:"id"`
	}
	if identityStatus != 200 || json.Unmarshal(identityBody, &identity) != nil || identity.ID != s.cfg.Human.WriterID {
		return errors.New("human gateway writer identity mismatch")
	}
	q := url.Values{"epic_id": {s.cfg.Human.EpicID}, "limit": {"1"}}
	status, b, err := s.backendRequest(ctx, c, http.MethodGet, "/api/v1/human-requests?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("human provider preflight HTTP %d", status)
	}
	var env struct {
		SchemaVersion int               `json:"schema_version"`
		Data          []json.RawMessage `json:"data"`
		NextCursor    *string           `json:"next_cursor"`
	}
	if json.Unmarshal(b, &env) != nil || env.SchemaVersion != 1 || env.Data == nil {
		return errors.New("human provider contract mismatch")
	}
	for _, raw := range env.Data {
		var v humanBackendView
		if json.Unmarshal(raw, &v) != nil || v.TenantID != s.cfg.TenantID || v.ProjectID != s.cfg.ProjectID {
			return errors.New("human provider scope mismatch")
		}
	}
	return nil
}
func (s *Store) claimHumanOperation(ctx context.Context) (humanOperation, error) {
	var op humanOperation
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT operation_id,request_id,kind,status,attempts,payload FROM backend_sync_operations o WHERE status IN ('pending','retry') AND (next_at='' OR next_at<=?) AND (kind='create' OR NOT EXISTS(SELECT 1 FROM backend_sync_operations c WHERE c.request_id=o.request_id AND c.kind='create' AND c.status!='done')) ORDER BY rowid LIMIT 1`, s.stamp()).Scan(&op.ID, &op.RequestID, &op.Kind, &op.Status, &op.Attempts, &op.Payload)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE backend_sync_operations SET status='inflight' WHERE operation_id=?", op.ID)
		return err
	})
	return op, err
}
func (s *Store) settleHumanOperation(ctx context.Context, op humanOperation, status, reason string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		attempts := op.Attempts + 1
		next := ""
		if status == "retry" {
			delay := time.Second * time.Duration(1<<min(attempts-1, 4))
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			delay += time.Duration(rand.Int63n(int64(time.Second)))
			if op.RetryAfter > delay {
				delay = op.RetryAfter
			}
			next = s.now().Add(delay).UTC().Format(time.RFC3339Nano)
		}
		_, err := tx.ExecContext(ctx, "UPDATE backend_sync_operations SET status=?,attempts=?,next_at=?,reason=? WHERE operation_id=?", status, attempts, next, reason, op.ID)
		return err
	})
}
func (s *Store) syncHumanOperation(ctx context.Context, client *http.Client, op humanOperation) error {
	if op.ID == "" {
		return nil
	}
	method := http.MethodPost
	path := "/api/v1/human-requests/" + op.RequestID
	switch op.Kind {
	case "create":
		method = http.MethodPut
	case "decision":
		path += "/decision"
	case "cancel":
		path += "/cancel"
	default:
		return errors.New("unknown backend operation")
	}
	status, b, retryAfter, err := s.backendRequestWithRetryAfter(ctx, client, method, path, op.Payload)
	op.RetryAfter = retryAfter
	if err != nil {
		if errors.Is(err, errGatewayAuth) {
			return s.settleHumanOperation(ctx, op, "blocked", "auth_blocked")
		}
		return s.settleHumanOperation(ctx, op, "retry", "transport_unknown")
	}
	if status == 409 { // A competing terminal may have committed: read the canonical fact.
		getStatus, getBody, getErr := s.backendRequest(ctx, client, http.MethodGet, "/api/v1/human-requests/"+op.RequestID, nil)
		if getErr == nil && getStatus == 200 {
			if err = s.acceptHumanEnvelope(ctx, op.RequestID, getBody); err != nil {
				return err
			}
		}
		return s.settleHumanOperation(ctx, op, "blocked", "backend_conflict")
	}
	if status == 429 || status >= 500 {
		return s.settleHumanOperation(ctx, op, "retry", fmt.Sprintf("http_%d", status))
	}
	if status != 200 && status != 201 {
		return s.settleHumanOperation(ctx, op, "blocked", fmt.Sprintf("http_%d", status))
	}
	if err = s.acceptHumanEnvelope(ctx, op.RequestID, b); err != nil {
		return s.settleHumanOperation(ctx, op, "blocked", "invalid_backend_contract")
	}
	return s.settleHumanOperation(ctx, op, "done", "")
}
func (s *Store) acceptHumanEnvelope(ctx context.Context, requestID string, b []byte) error {
	var envelope humanEnvelope
	if err := json.Unmarshal(b, &envelope); err != nil || envelope.SchemaVersion != 1 || len(envelope.Data) == 0 {
		return errors.New("human schema mismatch")
	}
	var v humanBackendView
	if err := json.Unmarshal(envelope.Data, &v); err != nil {
		return err
	}
	if v.ID != requestID || v.TenantID != s.cfg.TenantID || v.ProjectID != s.cfg.ProjectID || !uuid(v.Dependency.ID) || v.Revision < 1 || v.Revision > 2 {
		return errors.New("human backend scope/revision mismatch")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		d, err := dependency(ctx, tx, v.Dependency.ID)
		if err != nil {
			return err
		}
		if d.RequestID != requestID || d.FeatureID != v.FeatureID {
			return errors.New("human backend dependency mismatch")
		}
		var oldRev int
		var oldRaw []byte
		err = tx.QueryRowContext(ctx, "SELECT revision,data FROM human_projections WHERE request_id=?", requestID).Scan(&oldRev, &oldRaw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if oldRev > v.Revision {
			return errors.New("human backend revision regressed")
		}
		p := HumanProjection{RequestID: requestID, DependencyID: d.ID, BackendState: v.State, Revision: v.Revision, ApplicationStatus: "pending", View: envelope.Data}
		if oldRev == v.Revision && len(oldRaw) > 0 {
			var prior HumanProjection
			if err = json.Unmarshal(oldRaw, &prior); err != nil {
				return err
			}
			p.ApplicationStatus = prior.ApplicationStatus
			p.SuppressedReason = prior.SuppressedReason
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO human_projections(request_id,dependency_id,state,revision,data) VALUES(?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET state=excluded.state,revision=excluded.revision,data=excluded.data", p.RequestID, p.DependencyID, p.BackendState, p.Revision, mustJSON(p)); err != nil {
			return err
		}
		if v.State == "open" {
			if d.State == "human_pending" {
				d.State = "human_waiting"
				d.UpdatedAt = s.stamp()
				if err = saveDependency(ctx, tx, d); err != nil {
					return err
				}
				f, err := feature(ctx, tx, d.FeatureID)
				if err != nil {
					return err
				}
				out := SlackDelivery{ID: d.RequestID, FeatureID: d.FeatureID, ChannelID: f.ChannelID, ThreadTS: f.ThreadTS, Text: questionText(d), Status: "queued"}
				_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO slack_outbox(id,feature_id,turn_id,status,data) VALUES(?,?,NULL,?,?)", out.ID, out.FeatureID, out.Status, mustJSON(out))
				return err
			}
			return nil
		}
		return s.applyHumanTerminal(ctx, tx, d, p, v)
	})
}

// ApplyPendingHuman retries local application after source history catchup.
func (s *Store) ApplyPendingHuman(ctx context.Context) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT data FROM human_projections WHERE revision=2")
		if err != nil {
			return err
		}
		var pending []HumanProjection
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				break
			}
			var p HumanProjection
			if err = json.Unmarshal(raw, &p); err != nil {
				break
			}
			if p.ApplicationStatus == "pending" {
				pending = append(pending, p)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, p := range pending {
			d, err := dependency(ctx, tx, p.DependencyID)
			if err != nil {
				return err
			}
			var v humanBackendView
			if err = json.Unmarshal(p.View, &v); err != nil {
				return err
			}
			if err = s.applyHumanTerminal(ctx, tx, d, p, v); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Store) applyHumanTerminal(ctx context.Context, tx *sql.Tx, d Dependency, p HumanProjection, v humanBackendView) error {
	if v.Terminal == nil || !uuid(v.Terminal.ID) || v.Revision != 2 {
		return errors.New("invalid terminal fact")
	}
	var present int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM decision_applications WHERE request_id=? AND revision=?", p.RequestID, p.Revision).Scan(&present); err != nil {
		return err
	}
	if present != 0 {
		return nil
	}
	f, err := feature(ctx, tx, d.FeatureID)
	if err != nil {
		return err
	}
	status := "applied"
	reason := ""
	if f.Stopped || d.State == "cancelled" {
		status = "suppressed"
		reason = "feature_stopped_or_cancelled"
	}
	var barrier string
	err = tx.QueryRowContext(ctx, "SELECT reason FROM recovery_barriers WHERE feature_id=?", f.FeatureID).Scan(&barrier)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var src struct {
		ActorID string `json:"actor_id"`
	}
	if err = json.Unmarshal(v.Terminal.Source, &src); err != nil {
		return err
	}
	if v.Terminal.Kind == "answer" || v.Terminal.Kind == "deny" {
		if !allowedActor(f, src.ActorID) || !containsResponder(v.AllowedResponders, src.ActorID) {
			status = "suppressed"
			reason = "actor_revoked"
		}
	}
	// Keep the canonical terminal identity on every dependency state. The
	// owner runner verifies human.decision against this local projection even
	// when a denial grants no continuation authority.
	d.DecisionID = v.Terminal.ID
	if v.Terminal.Kind == "answer" && status == "applied" {
		d.State = "resolved"
	} else if v.Terminal.Kind == "answer" {
		d.State = "cancelled"
	} else if v.Terminal.Kind == "deny" {
		d.State = "denied"
	} else if v.Terminal.Kind == "cancel" {
		d.State = "cancelled"
	} else {
		return errors.New("unknown terminal kind")
	}
	d.UpdatedAt = s.stamp()
	if err = saveDependency(ctx, tx, d); err != nil {
		return err
	}
	p.ApplicationStatus = status
	p.SuppressedReason = reason
	if _, err = tx.ExecContext(ctx, "UPDATE human_projections SET data=? WHERE request_id=?", mustJSON(p), p.RequestID); err != nil {
		return err
	}
	var mailboxSeq any
	if status == "applied" {
		payload := HumanDecisionPayload{RequestID: p.RequestID, DependencyID: d.ID, DecisionID: v.Terminal.ID, State: v.State, Revision: v.Revision, Response: v.Terminal.Response, Source: v.Terminal.Source, ApplicationStatus: status}
		e := Envelope{ProtocolVersion: 2, MessageID: NewID(), Type: "human.decision", TenantID: f.TenantID, ProjectID: f.ProjectID, FeatureID: f.FeatureID, FromAgentID: "coordinator", ToAgentID: f.OwnerAgentID, CausationID: cause(p.RequestID), SentAt: s.stamp(), Payload: mustJSON(payload)}
		r, _, err := s.applyMessage(ctx, tx, Principal{AgentID: "coordinator"}, e, true)
		if err != nil {
			return err
		}
		mailboxSeq = r.MailboxSeq
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO decision_applications(request_id,revision,status,reason,mailbox_seq) VALUES(?,?,?,?,?)", p.RequestID, p.Revision, status, reason, mailboxSeq)
	return err
}

// SyncHuman performs bounded network work outside SQLite transactions. It is
// safe to call repeatedly after crashes; operation IDs and bodies never change.
func (s *Store) SyncHuman(ctx context.Context) error {
	if s.cfg.Human == nil {
		return nil
	}
	client, err := s.backendClient()
	if err != nil {
		return err
	}
	for i := 0; i < 4; i++ {
		op, err := s.claimHumanOperation(ctx)
		if err != nil {
			return err
		}
		if op.ID == "" {
			break
		}
		if err = s.syncHumanOperation(ctx, client, op); err != nil {
			return err
		}
	}
	return s.PollHuman(ctx, client)
}
func (s *Store) PollHuman(ctx context.Context, client *http.Client) error {
	var ids []string
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT request_id FROM human_projections WHERE state='open' AND request_id>? ORDER BY request_id LIMIT 20", s.humanPollAfter)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if len(ids) == 0 && s.humanPollAfter != "" {
		s.humanPollAfter = ""
		return nil
	}
	if len(ids) > 0 {
		s.humanPollAfter = ids[len(ids)-1]
	}
	for _, id := range ids {
		status, b, err := s.backendRequest(ctx, client, http.MethodGet, "/api/v1/human-requests/"+id, nil)
		if err != nil {
			return fmt.Errorf("human read %s: %w", id, err)
		}
		if status != 200 {
			return fmt.Errorf("human read %s: HTTP %d", id, status)
		}
		if err = s.acceptHumanEnvelope(ctx, id, b); err != nil {
			return err
		}
	}
	return nil
}
