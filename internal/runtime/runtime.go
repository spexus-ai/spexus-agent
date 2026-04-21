package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/registry"
)

var ErrNotFound = errors.New("record not found")

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
	SlackEventID string     `json:"slackEventId"`
	ReceivedAt   time.Time  `json:"receivedAt"`
	ProcessedAt  *time.Time `json:"processedAt,omitempty"`
	Status       string     `json:"status,omitempty"`
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
	SaveThreadState(context.Context, ThreadState) error
	LoadThreadState(context.Context, string) (ThreadState, error)
	SaveEventDedupe(context.Context, EventDedupe) error
	LoadEventDedupe(context.Context, string) (EventDedupe, error)
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
