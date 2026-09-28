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
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type ExecutionRef struct {
	WorkerAttemptID string `json:"worker_attempt_id,omitempty"`
	OwnerTurnID     string `json:"owner_turn_id,omitempty"`
}
type ActiveProfile struct {
	Slot                string          `json:"slot"`
	ProfileID           string          `json:"profile_id"`
	Role                string          `json:"role"`
	Enabled             bool            `json:"enabled"`
	Generation          int64           `json:"generation"`
	ActiveRevision      string          `json:"active_revision"`
	SnapshotJSON        json.RawMessage `json:"snapshot_json"`
	SnapshotBytesBase64 string          `json:"snapshot_bytes_base64"`
	AllowedModels       []string        `json:"allowed_models"`
}
type LaunchClaim struct {
	ClaimID             string          `json:"claim_id"`
	ExecutionRef        ExecutionRef    `json:"execution_ref"`
	ProfileID           string          `json:"profile_id"`
	Generation          int64           `json:"generation"`
	Revision            string          `json:"revision"`
	SnapshotJSON        json.RawMessage `json:"snapshot_json"`
	SnapshotBytesBase64 string          `json:"snapshot_bytes_base64"`
	State               string          `json:"state"`
}
type LaunchClaimRequest struct {
	SchemaVersion      int          `json:"schema_version"`
	ExecutionRef       ExecutionRef `json:"execution_ref"`
	ExpectedGeneration int64        `json:"expected_generation"`
	ExpectedRevision   string       `json:"expected_revision"`
}
type LaunchObservationRequest struct {
	SchemaVersion int          `json:"schema_version"`
	ExecutionRef  ExecutionRef `json:"execution_ref"`
	ClaimID       string       `json:"claim_id"`
	Revision      string       `json:"revision"`
	Outcome       string       `json:"outcome"`
}

type profileBackend struct {
	http                             *http.Client
	base, tokenFile, tenant, project string
}

func newProfileBackend(cfg *AgentProfileBackend, tenant, project string) (*profileBackend, error) {
	if cfg == nil || cfg.BaseURL == "" || cfg.TokenFile == "" || len(cfg.AllowedModels) == 0 {
		return nil, errors.New("agent profile backend configuration required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("agent profile backend URL must be an HTTPS origin")
	}
	if _, err := readProfileToken(cfg.TokenFile); err != nil {
		return nil, err
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if cfg.CAFile != "" {
		ca, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("invalid agent profile backend CA")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	return &profileBackend{http: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: strings.TrimRight(cfg.BaseURL, "/") + "/api/v1/agent-profiles", tokenFile: cfg.TokenFile, tenant: tenant, project: project}, nil
}
func readProfileToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("invalid agent profile backend credential")
	}
	return token, nil
}

func (b *profileBackend) request(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, b.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	token, err := readProfileToken(b.tokenFile)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Tenant-ID", b.tenant)
	r.Header.Set("X-Project-ID", b.project)
	if in != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.http.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024+1))
	if err != nil {
		return err
	}
	if len(raw) > 256*1024 {
		return errors.New("agent profile response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &failure) != nil || failure.Error.Code == "" {
			failure.Error.Code = "UNAVAILABLE"
		}
		return &APIError{Status: resp.StatusCode, Code: failure.Error.Code, Message: "Agent profile backend rejected request"}
	}
	var envelope struct {
		SchemaVersion int             `json:"schema_version"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.SchemaVersion != 1 || len(envelope.Data) == 0 {
		return errors.New("invalid agent profile response envelope")
	}
	return json.Unmarshal(envelope.Data, out)
}

func (s *Store) backendProfile(ctx context.Context, id string) (ActiveProfile, error) {
	var p ActiveProfile
	if err := s.profileBackend.request(ctx, "GET", "/"+id+"/active", nil, &p); err != nil {
		return p, err
	}
	if err := s.checkProfile(p, id); err != nil {
		return p, err
	}
	return p, nil
}
func (s *Store) ProfilePreflight(ctx context.Context) error {
	for _, id := range []string{"orchestrator", "worker-a", "worker-b"} {
		if _, err := s.backendProfile(ctx, id); err != nil {
			return fmt.Errorf("profile preflight %s: %w", id, err)
		}
	}
	return nil
}
func (s *Store) checkProfile(p ActiveProfile, id string) error {
	if p.ProfileID != id || p.Generation < 1 || p.ActiveRevision == "" || p.SnapshotBytesBase64 == "" {
		return errors.New("invalid active profile identity")
	}
	snapshotBytes, err := base64.StdEncoding.DecodeString(p.SnapshotBytesBase64)
	if err != nil || Digest(snapshotBytes) != p.ActiveRevision {
		return errors.New("active profile digest mismatch")
	}
	profile, err := ValidateWebTextProfile(snapshotBytes)
	if err != nil || profile.ID != id || len(profile.Tools) != 0 || len(profile.Extensions) != 0 {
		return errors.New("invalid executable profile")
	}
	allowed := false
	for _, model := range s.cfg.AgentProfiles.AllowedModels {
		if profile.Model == model {
			allowed = true
			break
		}
	}
	if !allowed {
		return errors.New("agent profile model unavailable")
	}
	if id == "orchestrator" && (p.Role != "owner" || p.Slot != "owner") || id != "orchestrator" && (p.Role != "worker" || p.Slot != id) {
		return errors.New("agent profile role mismatch")
	}
	if !bytes.Equal(p.SnapshotJSON, snapshotBytes) { // Both are canonical bytes in this contract.
		return errors.New("agent profile snapshot mismatch")
	}
	return nil
}

func (s *Store) profileFor(ctx context.Context, principal Principal, profileID string) (ActiveProfile, error) {
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, principal); err != nil {
			return err
		}
		var ownID, role string
		if err := tx.QueryRowContext(ctx, "SELECT profile_id,role FROM agents WHERE agent_id=?", principal.AgentID).Scan(&ownID, &role); err != nil {
			return wireError(404, "profile_not_found")
		}
		if ownID != profileID && role != "owner" {
			return wireError(403, "profile_not_bound")
		}
		return nil
	})
	if err != nil {
		return ActiveProfile{}, err
	}
	return s.backendProfile(ctx, profileID)
}
func (s *Store) agentProfileBinding(ctx context.Context, principal Principal, profileID string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, principal); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRowContext(ctx, "SELECT profile_id FROM agents WHERE agent_id=?", principal.AgentID).Scan(&id); err != nil {
			return wireError(404, "profile_not_found")
		}
		if id != profileID {
			return wireError(403, "profile_not_bound")
		}
		return nil
	})
}

func (s *Store) claimProfile(ctx context.Context, principal Principal, profileID string, request LaunchClaimRequest) (LaunchClaim, error) {
	var out LaunchClaim
	if request.SchemaVersion != 1 || request.ExpectedGeneration < 1 || request.ExpectedRevision == "" {
		return out, wireError(400, "invalid_claim")
	}
	if err := s.verifyExecution(ctx, principal, profileID, request.ExecutionRef); err != nil {
		return out, err
	}
	if err := s.profileBackend.request(ctx, "POST", "/"+profileID+"/launch-claims", request, &out); err != nil {
		return out, err
	}
	if out.ProfileID != profileID || out.Revision != request.ExpectedRevision || out.Generation != request.ExpectedGeneration || out.ExecutionRef != request.ExecutionRef || out.State != "starting" {
		return out, errors.New("claim response mismatch")
	}
	active := ActiveProfile{ProfileID: out.ProfileID, Role: "worker", Slot: out.ProfileID, Generation: out.Generation, ActiveRevision: out.Revision, SnapshotJSON: out.SnapshotJSON, SnapshotBytesBase64: out.SnapshotBytesBase64}
	if profileID == "orchestrator" {
		active.Role = "owner"
		active.Slot = "owner"
	}
	if err := s.checkProfile(active, profileID); err != nil {
		return out, err
	}
	return out, nil
}
func (s *Store) verifyExecution(ctx context.Context, principal Principal, profileID string, ref ExecutionRef) error {
	if err := s.agentProfileBinding(ctx, principal, profileID); err != nil {
		return err
	}
	if (ref.WorkerAttemptID == "") == (ref.OwnerTurnID == "") {
		return wireError(400, "invalid_execution_ref")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, principal); err != nil {
			return err
		}
		if ref.WorkerAttemptID != "" {
			if !uuid(ref.WorkerAttemptID) {
				return wireError(400, "invalid_execution_ref")
			}
			a, err := attempt(ctx, tx, ref.WorkerAttemptID)
			if err != nil || a.AssignedAgentID != principal.AgentID || a.InstanceID != principal.InstanceID || a.State != "accepted" {
				return wireError(409, "attempt_not_accepted")
			}
			return nil
		}
		if !uuid(ref.OwnerTurnID) {
			return wireError(400, "invalid_execution_ref")
		}
		t, err := turn(ctx, tx, ref.OwnerTurnID)
		if err != nil || t.AgentID != principal.AgentID || t.InstanceID != principal.InstanceID || t.State != "running" {
			return wireError(409, "turn_not_running")
		}
		return nil
	})
}

func (s *Store) observeProfile(ctx context.Context, principal Principal, profileID string, request LaunchObservationRequest) (json.RawMessage, error) {
	if request.SchemaVersion != 1 || request.Outcome != "launched" && request.Outcome != "failed" || request.ClaimID == "" {
		return nil, wireError(400, "invalid_observation")
	}
	if err := s.agentProfileBinding(ctx, principal, profileID); err != nil {
		return nil, err
	}
	var out json.RawMessage
	if err := s.profileBackend.request(ctx, "POST", "/"+profileID+"/observations", request, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func profileIDFromPath(path string) (string, string, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/agent-profiles/"), "/")
	if len(parts) < 2 || (parts[0] != "orchestrator" && parts[0] != "worker-a" && parts[0] != "worker-b") {
		return "", "", fmt.Errorf("invalid profile path")
	}
	return parts[0], strings.Join(parts[1:], "/"), nil
}

// requeueOwnerPrelaunch retains the immutable source message and creates a new
// mailbox delivery only when the backend proves no claim exists for this turn.
func (s *Store) requeueOwnerPrelaunch(ctx context.Context, principal Principal, id string) (map[string]any, error) {
	if !uuid(id) {
		return nil, wireError(400, "invalid_turn")
	}
	var existing LaunchClaim
	err := s.profileBackend.request(ctx, "GET", "/orchestrator/launch-claims/owner_turn/"+id, nil, &existing)
	if err == nil {
		return nil, wireError(409, "claim_already_exists")
	}
	var api *APIError
	if !errors.As(err, &api) || api.Status != 404 {
		return nil, err
	}
	var newSeq int64
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, principal); err != nil {
			return err
		}
		t, err := turn(ctx, tx, id)
		if err != nil {
			return err
		}
		if t.AgentID != principal.AgentID || t.InstanceID != principal.InstanceID || t.State != "running" || t.AgentID != "orchestrator" {
			return wireError(409, "turn_not_requeueable")
		}
		var actions int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE sender=? AND turn_id=?", principal.AgentID, id).Scan(&actions); err != nil {
			return err
		}
		if actions != 0 {
			return wireError(409, "turn_has_actions")
		}
		var messageRow int64
		var size int
		if err := tx.QueryRowContext(ctx, "SELECT message_row,bytes FROM mailbox_delivery WHERE agent_id=? AND seq=?", principal.AgentID, t.InputMailboxSeq).Scan(&messageRow, &size); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_counters(agent_id,seq) VALUES(?,1) ON CONFLICT(agent_id) DO UPDATE SET seq=seq+1`, principal.AgentID); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT seq FROM mailbox_counters WHERE agent_id=?", principal.AgentID).Scan(&newSeq); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_delivery(agent_id,seq,message_row,lane,pending_notification,bytes) VALUES(?,?,?,'normal',0,?)`, principal.AgentID, newSeq, messageRow, size); err != nil {
			return err
		}
		t.State = "failed"
		t.Error = &TaskError{Code: "profile_prelaunch_denied", Message: "profile_prelaunch_denied", Retryable: true}
		if err := saveTurn(ctx, tx, t); err != nil {
			return err
		}
		return s.audit(ctx, tx, principal, Envelope{FeatureID: t.FeatureID, OwnerTurnID: id}, "owner_profile_requeued", "profile_prelaunch_denied")
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"turn_id": id, "new_mailbox_seq": newSeq, "status": "queued"}, nil
}
