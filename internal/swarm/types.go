// Package swarm implements the prototype-2 durable coordinator and wire contract.
package swarm

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const APIPrefix = "/internal/agent/v1"
const MaxEnvelopeBytes = 128 * 1024

type Config struct {
	TenantID    string            `json:"tenant_id"`
	ProjectID   string            `json:"project_id"`
	WireVersion int               `json:"wire_version,omitempty"`
	Human       *HumanConfig      `json:"human,omitempty"`
	Agents      []AgentConfig     `json:"agents"`
	Features    []Feature         `json:"features"`
	Profiles    []ProfileSnapshot `json:"profiles"`
}

// HumanConfig is coordinator-only. Runners never receive backend credentials.
type HumanConfig struct {
	BaseURL     string `json:"base_url"`
	CAFile      string `json:"ca_file"`
	TokenFile   string `json:"token_file"`
	EpicID      string `json:"epic_id"`
	WriterID    string `json:"writer_id"`
	WorkspaceID string `json:"workspace_id"`
}
type AgentConfig struct {
	AgentID          string `json:"agent_id"`
	Role             string `json:"role"` // owner or worker
	CredentialSHA256 string `json:"credential_sha256"`
	ProfileID        string `json:"profile_id"`
}

// Bytes is the exact UTF-8 JSON snapshot, not reserialized before hashing.
type ProfileSnapshot struct {
	Bytes json.RawMessage `json:"bytes"`
}
type TextProfile struct {
	ID         string   `json:"id"`
	Model      string   `json:"model"`
	Reasoning  string   `json:"reasoning"`
	Prompt     string   `json:"prompt"`
	Tools      []string `json:"tools"`
	Extensions []string `json:"extensions"`
}
type Profile struct {
	ID        string `json:"id"`
	Revision  string `json:"revision"`
	Model     string `json:"model"`
	Reasoning string `json:"reasoning"`
}

func Digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type Feature struct {
	FeatureID       string   `json:"feature_id"`
	TenantID        string   `json:"tenant_id"`
	ProjectID       string   `json:"project_id"`
	OwnerAgentID    string   `json:"owner_agent_id"`
	ChannelID       string   `json:"channel_id"`
	ThreadTS        string   `json:"thread_ts"`
	AllowedActorIDs []string `json:"allowed_actor_ids"`
	Stopped         bool     `json:"stopped"`
}
type Principal struct{ AgentID, InstanceID string }
type Envelope struct {
	ProtocolVersion int             `json:"protocol_version"`
	MessageID       string          `json:"message_id"`
	Type            string          `json:"type"`
	TenantID        string          `json:"tenant_id"`
	ProjectID       string          `json:"project_id"`
	FeatureID       string          `json:"feature_id"`
	FromAgentID     string          `json:"from_agent_id"`
	ToAgentID       string          `json:"to_agent_id"`
	OwnerTurnID     string          `json:"owner_turn_id,omitempty"`
	JobID           string          `json:"job_id,omitempty"`
	AttemptID       string          `json:"attempt_id,omitempty"`
	CausationID     *string         `json:"causation_id"`
	SentAt          string          `json:"sent_at"`
	Payload         json.RawMessage `json:"payload"`
}
type ContextRef struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	Label string `json:"label"`
}
type TaskContext struct {
	Text string       `json:"text"`
	Refs []ContextRef `json:"refs"`
}
type DispatchPayload struct {
	Goal              string      `json:"goal"`
	Scope             string      `json:"scope"`
	ExpectedResult    []string    `json:"expected_result"`
	Context           TaskContext `json:"context"`
	Profile           Profile     `json:"profile"`
	AcceptBy          string      `json:"accept_by"`
	RunTimeoutSeconds int         `json:"run_timeout_seconds"`
}
type AcceptedPayload struct {
	DispatchMessageID string `json:"dispatch_message_id"`
	ProfileRevision   string `json:"profile_revision"`
}
type StartedPayload struct {
	AcceptedMessageID string `json:"accepted_message_id"`
}
type Evidence struct {
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	ContentOrRef string `json:"content_or_ref"`
	SHA256       string `json:"sha256,omitempty"`
}
type TaskError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
type Observation struct {
	Outcome  string     `json:"outcome"`
	Summary  string     `json:"summary"`
	Evidence []Evidence `json:"evidence"`
	Error    *TaskError `json:"error"`
}
type ResultPayload struct {
	Outcome     string       `json:"outcome"`
	Summary     string       `json:"summary"`
	Evidence    []Evidence   `json:"evidence"`
	Error       *TaskError   `json:"error"`
	Origin      string       `json:"origin"`
	Observation *Observation `json:"observation,omitempty"`
	Blocker     *Blocker     `json:"blocker,omitempty"`
}
type HumanOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
type Blocker struct {
	Reason         string        `json:"reason"`
	Context        string        `json:"context"`
	Question       string        `json:"question"`
	Options        []HumanOption `json:"options"`
	Recommendation string        `json:"recommendation"`
	Kind           string        `json:"kind"`
}
type HumanRequestPayload struct {
	DependencyID string `json:"dependency_id,omitempty"`
	StepKey      string `json:"step_key,omitempty"`
	BlockedWork  string `json:"blocked_work,omitempty"`
	Blocker
}
type ResolveDependencyPayload struct {
	DependencyID string     `json:"dependency_id"`
	Resolution   string     `json:"resolution"`
	Evidence     []Evidence `json:"evidence"`
}
type ResumeTaskPayload struct {
	DependencyID  string          `json:"dependency_id"`
	DecisionID    string          `json:"decision_id,omitempty"`
	WorkerAgentID string          `json:"worker_agent_id"`
	Dispatch      DispatchPayload `json:"dispatch"`
}
type CompleteStepPayload struct {
	DependencyID string `json:"dependency_id"`
	DecisionID   string `json:"decision_id"`
	Summary      string `json:"summary"`
}
type HumanDecisionPayload struct {
	RequestID         string          `json:"request_id"`
	DependencyID      string          `json:"dependency_id"`
	DecisionID        string          `json:"decision_id"`
	State             string          `json:"state"`
	Revision          int             `json:"revision"`
	Response          json.RawMessage `json:"response,omitempty"`
	Source            json.RawMessage `json:"source,omitempty"`
	ApplicationStatus string          `json:"application_status"`
}
type ReviewPayload struct {
	ResultMessageID string     `json:"result_message_id"`
	Verdict         string     `json:"verdict"`
	Reason          string     `json:"reason"`
	Evidence        []Evidence `json:"evidence"`
}
type CancelPayload struct {
	Reason      string `json:"reason"`
	RequestedBy string `json:"requested_by"`
}
type Source struct {
	Kind      string `json:"kind"`
	EventID   string `json:"event_id"`
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
	ActorID   string `json:"actor_id"`
}
type InputPayload struct {
	Text   string `json:"text"`
	Source Source `json:"source"`
}
type Receipt struct {
	MessageID  string `json:"message_id"`
	Receipt    string `json:"receipt"`
	MailboxSeq int64  `json:"mailbox_seq"`
	ReceivedAt string `json:"received_at"`
}
type Delivery struct {
	Envelope
	MailboxSeq int64  `json:"mailbox_seq"`
	ReceivedAt string `json:"received_at"`
}
type MailboxResponse struct {
	Messages []Delivery `json:"messages"`
}
type AckRequest struct {
	MailboxSeqs []int64 `json:"mailbox_seqs"`
}
type AckResponse struct {
	Acked []int64 `json:"acked"`
}
type HeartbeatRequest struct {
	InstanceID        string  `json:"instance_id"`
	ActiveAttemptID   *string `json:"active_attempt_id"`
	ActiveOwnerTurnID *string `json:"active_owner_turn_id"`
}
type HeartbeatResponse struct {
	AgentID    string `json:"agent_id"`
	InstanceID string `json:"instance_id"`
	Status     string `json:"status"`
	ServerTime string `json:"server_time"`
}
type Attempt struct {
	AttemptID         string         `json:"attempt_id"`
	AssignedAgentID   string         `json:"assigned_agent_id"`
	State             string         `json:"state"`
	CancelRequested   bool           `json:"cancel_requested"`
	Review            string         `json:"review"`
	StartedAt         *string        `json:"started_at"`
	CompletedAt       *string        `json:"completed_at"`
	Error             *TaskError     `json:"error"`
	Result            *ResultPayload `json:"result,omitempty"`
	DispatchMessageID string         `json:"dispatch_message_id"`
	AcceptedMessageID string         `json:"accepted_message_id,omitempty"`
	ResultMessageID   string         `json:"result_message_id,omitempty"`
	CancelMessageID   string         `json:"cancel_message_id,omitempty"`
	DependencyID      string         `json:"dependency_id,omitempty"`
}
type JobView struct {
	JobID            string      `json:"job_id"`
	FeatureID        string      `json:"feature_id"`
	CurrentAttemptID string      `json:"current_attempt_id"`
	Attempts         []Attempt   `json:"attempts"`
	NextCursor       *string     `json:"next_cursor"`
	Dependency       *Dependency `json:"dependency,omitempty"`
}
type OwnerStartRequest struct {
	TurnID          string `json:"turn_id"`
	FeatureID       string `json:"feature_id"`
	InputMailboxSeq int64  `json:"input_mailbox_seq"`
}
type OwnerStartReceipt struct {
	TurnID          string `json:"turn_id"`
	State           string `json:"state"`
	InputMailboxSeq int64  `json:"input_mailbox_seq"`
}
type ActionReceipt struct {
	MessageID string  `json:"message_id"`
	Status    string  `json:"status"`
	ErrorCode *string `json:"error_code"`
}
type OwnerFinishRequest struct {
	Outcome     string          `json:"outcome"`
	Reply       string          `json:"reply"`
	Actions     []ActionReceipt `json:"actions"`
	Error       *TaskError      `json:"error"`
	Observation json.RawMessage `json:"observation"`
}
type OwnerTurn struct {
	TurnID          string     `json:"turn_id"`
	FeatureID       string     `json:"feature_id"`
	InputMailboxSeq int64      `json:"input_mailbox_seq"`
	State           string     `json:"state"`
	CancelRequested bool       `json:"cancel_requested"`
	ReplyStatus     string     `json:"reply_status"`
	Error           *TaskError `json:"error"`
}
type OwnerFinishReceipt struct {
	TurnID      string `json:"turn_id"`
	State       string `json:"state"`
	ReplyStatus string `json:"reply_status"`
}
type SlackDelivery struct {
	ID        string         `json:"id"`
	FeatureID string         `json:"feature_id"`
	TurnID    string         `json:"turn_id"`
	ChannelID string         `json:"channel_id"`
	ThreadTS  string         `json:"thread_ts"`
	Text      string         `json:"text"`
	Status    string         `json:"status"`
	SlackTS   string         `json:"slack_ts"`
	Question  *HumanQuestion `json:"question,omitempty"`
}
type HumanQuestion struct {
	Options []HumanOption `json:"options"`
}
type AuditEvent struct {
	Seq        int64  `json:"seq"`
	At         string `json:"at"`
	AgentID    string `json:"agent_id"`
	InstanceID string `json:"instance_id"`
	FeatureID  string `json:"feature_id"`
	MessageID  string `json:"message_id"`
	JobID      string `json:"job_id"`
	AttemptID  string `json:"attempt_id"`
	TurnID     string `json:"turn_id"`
	Event      string `json:"event"`
	Code       string `json:"code"`
}
type AgentStatus struct {
	AgentID       string  `json:"agent_id"`
	InstanceID    string  `json:"instance_id"`
	Status        string  `json:"status"`
	LastHeartbeat *string `json:"last_heartbeat"`
}
type History struct {
	Feature         Feature           `json:"feature"`
	Jobs            []JobView         `json:"jobs"`
	Turns           []OwnerTurn       `json:"turns"`
	Messages        []Delivery        `json:"messages"`
	Audit           []AuditEvent      `json:"audit"`
	Agents          []AgentStatus     `json:"agents"`
	SlackOutbox     []SlackDelivery   `json:"slack_outbox"`
	Dependencies    []Dependency      `json:"dependencies,omitempty"`
	HumanRequests   []HumanProjection `json:"human_requests,omitempty"`
	HumanSync       []HumanSyncStatus `json:"human_sync,omitempty"`
	RecoveryBarrier string            `json:"recovery_barrier,omitempty"`
}
type HumanSyncStatus struct {
	OperationID string `json:"operation_id"`
	RequestID   string `json:"request_id"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Attempts    int    `json:"attempts"`
	NextAt      string `json:"next_at,omitempty"`
	Reason      string `json:"reason,omitempty"`
}
type Dependency struct {
	ID                    string  `json:"id"`
	FeatureID             string  `json:"feature_id"`
	Kind                  string  `json:"kind"`
	JobID                 string  `json:"job_id,omitempty"`
	AttemptID             string  `json:"attempt_id,omitempty"`
	StepKey               string  `json:"step_key,omitempty"`
	BlockedWork           string  `json:"blocked_work,omitempty"`
	OriginTurnID          string  `json:"origin_turn_id,omitempty"`
	SourceMessageID       string  `json:"source_message_id"`
	State                 string  `json:"state"`
	Blocker               Blocker `json:"blocker"`
	RequestID             string  `json:"request_id,omitempty"`
	DecisionID            string  `json:"decision_id,omitempty"`
	Resolution            string  `json:"resolution,omitempty"`
	ContinuationAttemptID string  `json:"continuation_attempt_id,omitempty"`
	CreatedAt             string  `json:"created_at"`
	UpdatedAt             string  `json:"updated_at"`
}
type HumanProjection struct {
	RequestID         string          `json:"request_id"`
	DependencyID      string          `json:"dependency_id"`
	BackendState      string          `json:"backend_state"`
	Revision          int             `json:"revision"`
	ApplicationStatus string          `json:"application_status"`
	SuppressedReason  string          `json:"suppressed_reason,omitempty"`
	View              json.RawMessage `json:"view,omitempty"`
}
type ReconcileRequest struct {
	AgentID          string    `json:"agent_id"`
	OldInstanceID    string    `json:"old_instance_id"`
	NewInstanceID    string    `json:"new_instance_id"`
	Reason           string    `json:"reason"`
	Actor            string    `json:"actor"`
	ContainerID      string    `json:"container_id"`
	ContainerStopped bool      `json:"container_stopped"`
	CheckedAt        time.Time `json:"checked_at"`
}
type APIError struct {
	Retryable bool   `json:"retryable"`
	Status    int    `json:"-"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }
func wireError(status int, code string) error {
	return &APIError{Status: status, Code: code, Message: code}
}
