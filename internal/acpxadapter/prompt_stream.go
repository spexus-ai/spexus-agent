package acpxadapter

import "fmt"

type PromptStream interface {
	SessionName() string
	Events() <-chan Event
	Wait() error
	Close() error
}

func CollectPromptStream(stream PromptStream) ([]Event, error) {
	if stream == nil {
		return nil, fmt.Errorf("prompt stream is required")
	}

	events := make([]Event, 0, 16)
	for event := range stream.Events() {
		events = append(events, event)
	}

	return events, stream.Wait()
}
