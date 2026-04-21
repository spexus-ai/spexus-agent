package registry

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("project not found")

type Project struct {
	Name             string    `json:"name"`
	GitRemote        string    `json:"gitRemote,omitempty"`
	LocalPath        string    `json:"localPath"`
	SlackChannelName string    `json:"slackChannelName,omitempty"`
	SlackChannelID   string    `json:"slackChannelID,omitempty"`
	CreatedAt        time.Time `json:"createdAt,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt,omitempty"`
}

type Store interface {
	Upsert(context.Context, Project) error
	Get(context.Context, string) (Project, error)
	List(context.Context) ([]Project, error)
	Delete(context.Context, string) error
}
