package slack

import (
	"context"
	"errors"
	"testing"
)

type fakeChannelClient struct {
	requests []CreateChannelRequest
	channel  Channel
	found    Channel
	err      error
}

func (f *fakeChannelClient) PostMessage(context.Context, Message) (PostedMessage, error) {
	return PostedMessage{ChannelID: "C12345678", Timestamp: "1713686400.000100"}, nil
}

func (f *fakeChannelClient) PostThreadMessage(context.Context, Message) error { return nil }

func (f *fakeChannelClient) CreateChannel(_ context.Context, request CreateChannelRequest) (Channel, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return Channel{}, f.err
	}
	return f.channel, nil
}

func (f *fakeChannelClient) FindChannelByName(_ context.Context, name string) (Channel, error) {
	if f.found.ID == "" && f.found.Name == "" {
		return Channel{}, ErrChannelNotFound
	}
	if f.found.Name != name {
		return Channel{}, ErrChannelNotFound
	}
	return f.found, nil
}

func (f *fakeChannelClient) Close() error { return nil }

// Test: project channel provisioner creates the expected Slack channel name and returns the created metadata.
// Validates: AC-1776 (REQ-1138 - imported projects receive Slack channels named spexus-${PROJECT_NAME})
func TestProjectChannelProvisionerCreatesExpectedChannelName(t *testing.T) {
	t.Parallel()

	client := &fakeChannelClient{
		channel: Channel{ID: "C12345678", Name: "spexus-backend-notification-app"},
	}
	provisioner := ProjectChannelProvisioner{Client: client}

	channel, err := provisioner.ProvisionProjectChannel(context.Background(), "backend-notification-app")
	if err != nil {
		t.Fatalf("ProvisionProjectChannel() error = %v", err)
	}

	if channel.ID != "C12345678" {
		t.Fatalf("ProvisionProjectChannel() id = %q, want %q", channel.ID, "C12345678")
	}
	if channel.Name != "spexus-backend-notification-app" {
		t.Fatalf("ProvisionProjectChannel() name = %q, want %q", channel.Name, "spexus-backend-notification-app")
	}
	if len(client.requests) != 1 {
		t.Fatalf("CreateChannel() calls = %d, want 1", len(client.requests))
	}
	if got, want := client.requests[0].Name, "spexus-backend-notification-app"; got != want {
		t.Fatalf("CreateChannel() name = %q, want %q", got, want)
	}
}

// Test: project channel provisioner rejects partial Slack metadata before the import can be registered.
// Validates: AC-1779 (REQ-1140 - do not allow partial project registration when Slack channel creation fails)
func TestProjectChannelProvisionerRejectsMissingChannelID(t *testing.T) {
	t.Parallel()

	client := &fakeChannelClient{
		channel: Channel{Name: "spexus-backend-notification-app"},
	}
	provisioner := ProjectChannelProvisioner{Client: client}

	if _, err := provisioner.ProvisionProjectChannel(context.Background(), "backend-notification-app"); err == nil {
		t.Fatalf("ProvisionProjectChannel() error = nil, want non-nil")
	}
}

// Test: project channel helper normalizes the default prefix for project channel names.
// Validates: AC-1776 (REQ-1138 - imported projects receive Slack channels named spexus-${PROJECT_NAME})
func TestProjectChannelNameUsesDefaultPrefix(t *testing.T) {
	t.Parallel()

	if got, want := ProjectChannelName("backend-notification-app"), "spexus-backend-notification-app"; got != want {
		t.Fatalf("ProjectChannelName() = %q, want %q", got, want)
	}
}

// Test: project channel provisioner fails when the client rejects channel creation.
// Validates: AC-1779 (REQ-1140 - failed Slack channel creation aborts the import)
func TestProjectChannelProvisionerPropagatesClientErrors(t *testing.T) {
	t.Parallel()

	client := &fakeChannelClient{err: errors.New("slack unavailable")}
	provisioner := ProjectChannelProvisioner{Client: client}

	if _, err := provisioner.ProvisionProjectChannel(context.Background(), "backend-notification-app"); err == nil {
		t.Fatalf("ProvisionProjectChannel() error = nil, want non-nil")
	}
}

// Test: project channel provisioner reuses an existing Slack channel when create returns name_taken.
// Validates: AC-1768 (REQ-1129 - imported projects may bind to the required spexus-${PROJECT_NAME} channel), AC-1770 (REQ-1131 - repeated imports do not fail with partial registration if the channel already exists)
func TestProjectChannelProvisionerReusesExistingChannelOnNameTaken(t *testing.T) {
	t.Parallel()

	client := &fakeChannelClient{
		err:   ErrChannelNameTaken,
		found: Channel{ID: "C12345678", Name: "spexus-product-requirements-front"},
	}
	provisioner := ProjectChannelProvisioner{Client: client}

	channel, err := provisioner.ProvisionProjectChannel(context.Background(), "product-requirements-front")
	if err != nil {
		t.Fatalf("ProvisionProjectChannel() error = %v", err)
	}

	if channel.ID != "C12345678" {
		t.Fatalf("ProvisionProjectChannel() id = %q, want %q", channel.ID, "C12345678")
	}
	if channel.Name != "spexus-product-requirements-front" {
		t.Fatalf("ProvisionProjectChannel() name = %q, want %q", channel.Name, "spexus-product-requirements-front")
	}
}
