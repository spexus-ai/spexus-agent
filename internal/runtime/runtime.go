package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/registry"
)

var ErrNotFound = errors.New("record not found")

type ExecutionLifecycleState string

const (
	ExecutionStateQueued    ExecutionLifecycleState = "queued"
	ExecutionStateRunning   ExecutionLifecycleState = "running"
	ExecutionStateSucceeded ExecutionLifecycleState = "succeeded"
	ExecutionStateFailed    ExecutionLifecycleState = "failed"
	ExecutionStateCancelled ExecutionLifecycleState = "cancelled"
)

func isTerminalExecutionState(status ExecutionLifecycleState) bool {
	switch status {
	case ExecutionStateSucceeded, ExecutionStateFailed, ExecutionStateCancelled:
		return true
	default:
		return false
	}
}

type ExecutionRequest struct {
	ExecutionID       string                  `json:"executionId"`
	SourceType        string                  `json:"sourceType"`
	DeliveryID        string                  `json:"deliveryId"`
	ChannelID         string                  `json:"channelId"`
	ProjectName       string                  `json:"projectName"`
	SessionKey        string                  `json:"sessionKey"`
	ThreadTS          string                  `json:"threadTs,omitempty"`
	CommandText       string                  `json:"commandText,omitempty"`
	Status            ExecutionLifecycleState `json:"status"`
	DiagnosticContext string                  `json:"diagnosticContext,omitempty"`
	CreatedAt         time.Time               `json:"createdAt"`
	StartedAt         *time.Time              `json:"startedAt,omitempty"`
	UpdatedAt         time.Time               `json:"updatedAt"`
	CompletedAt       *time.Time              `json:"completedAt,omitempty"`
}

type ExecutionState struct {
	ExecutionID       string                  `json:"executionId"`
	Status            ExecutionLifecycleState `json:"status"`
	DiagnosticContext string                  `json:"diagnosticContext,omitempty"`
	StartedAt         *time.Time              `json:"startedAt,omitempty"`
	UpdatedAt         time.Time               `json:"updatedAt"`
	CompletedAt       *time.Time              `json:"completedAt,omitempty"`
}

type ThreadState struct {
	ThreadTS      string    `json:"threadTs"`
	ChannelID     string    `json:"channelId"`
	ProjectName   string    `json:"projectName"`
	SessionName   string    `json:"sessionName"`
	LastStatus    string    `json:"lastStatus,omitempty"`
	LastRequestID string    `json:"lastRequestId,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt,omitempty"`
}

type EventDedupe struct {
	SourceType        string     `json:"sourceType"`
	DeliveryID        string     `json:"deliveryId"`
	ReceivedAt        time.Time  `json:"receivedAt"`
	ProcessedAt       *time.Time `json:"processedAt,omitempty"`
	Status            string     `json:"status,omitempty"`
	DiagnosticContext string     `json:"diagnosticContext,omitempty"`
}

type ThreadLock struct {
	ThreadTS       string     `json:"threadTs"`
	LockOwner      string     `json:"lockOwner"`
	LockedAt       time.Time  `json:"lockedAt"`
	LeaseExpiresAt *time.Time `json:"leaseExpiresAt,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

type Status struct {
	Running          bool                `json:"running"`
	Healthy          bool                `json:"healthy"`
	Message          string              `json:"message,omitempty"`
	ActiveThreads    int                 `json:"activeThreads"`
	ProjectCount     int                 `json:"projectCount"`
	EventDedupeCount int                 `json:"eventDedupeCount"`
	ConfigPath       string              `json:"configPath,omitempty"`
	StoragePath      string              `json:"storagePath,omitempty"`
	LoadedAt         time.Time           `json:"loadedAt"`
	Config           config.GlobalConfig `json:"config,omitempty"`
	Projects         []registry.Project  `json:"projects,omitempty"`
}

type HealthCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

type DoctorReport struct {
	Healthy bool          `json:"healthy"`
	Message string        `json:"message,omitempty"`
	Checks  []HealthCheck `json:"checks"`
	Status  Status        `json:"status"`
}

type ReloadReport struct {
	ReloadedAt time.Time `json:"reloadedAt"`
	Status     Status    `json:"status"`
}

type ExecutionStatusReport struct {
	RequestedAt time.Time        `json:"requestedAt"`
	Execution   ExecutionRequest `json:"execution"`
}

type CancelReport struct {
	RequestedAt time.Time `json:"requestedAt"`
	ThreadTS    string    `json:"threadTs"`
	SessionName string    `json:"sessionName,omitempty"`
	Result      string    `json:"result"`
	NoOp        bool      `json:"noOp"`
	Message     string    `json:"message,omitempty"`
	Status      Status    `json:"status"`
}

type Store interface {
	CreateExecution(context.Context, ExecutionRequest) error
	LoadExecution(context.Context, string) (ExecutionRequest, error)
	ListExecutions(context.Context, []ExecutionLifecycleState) ([]ExecutionRequest, error)
	UpdateExecutionState(context.Context, ExecutionState) error
	SaveThreadState(context.Context, ThreadState) error
	LoadThreadState(context.Context, string) (ThreadState, error)
	SaveEventDedupe(context.Context, EventDedupe) error
	LoadEventDedupe(context.Context, string, string) (EventDedupe, error)
	SaveThreadLock(context.Context, ThreadLock) error
	LoadThreadLock(context.Context, string) (ThreadLock, error)
	DeleteThreadLock(context.Context, string) error
}

type Coordinator interface {
	Start(context.Context) error
	Reload(context.Context) error
	Status(context.Context) (Status, error)
	Doctor(context.Context) (DoctorReport, error)
	Cancel(context.Context, string) error
}
