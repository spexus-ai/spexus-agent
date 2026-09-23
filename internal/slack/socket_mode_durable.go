package slack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// RunDurable is the wire-v2 ingress path. The legacy Events channel retains
// its previous semantics for non-swarm consumers.
func (c *SocketModeClient) RunDurable(ctx context.Context, connected, disconnected func(context.Context) error, handle func(context.Context, Event) error) error {
	if c == nil || strings.TrimSpace(c.appToken) == "" || handle == nil {
		return fmt.Errorf("durable Slack source is not configured")
	}
	backoff := time.Second
	for ctx.Err() == nil {
		url, err := c.openConnection(ctx)
		if err != nil {
			c.logf("durable Slack connection pending: %v", err)
		} else {
			err = c.consumeDurable(ctx, url, connected, handle)
			if disconnected != nil && ctx.Err() == nil {
				closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				closeErr := disconnected(closeCtx)
				cancel()
				if closeErr != nil {
					return fmt.Errorf("close Slack recovery barrier: %w", closeErr)
				}
			}
			if err != nil && ctx.Err() == nil {
				c.logf("durable Slack socket disconnected: %v", err)
			}
		}
		if !sleepWithContext(ctx, backoff) {
			break
		}
		backoff = nextBackoff(backoff)
	}
	return ctx.Err()
}

func (c *SocketModeClient) consumeDurable(ctx context.Context, socketURL string, connected func(context.Context) error, handle func(context.Context, Event) error) error {
	conn, _, err := c.dialerOrDefault().DialContext(ctx, socketURL, nil)
	if err != nil {
		return fmt.Errorf("dial Slack Socket Mode: %w", err)
	}
	c.setConn(conn)
	defer c.Close()
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
	}()
	if connected != nil {
		if err := connected(connCtx); err != nil {
			return err
		}
	}
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read Slack Socket Mode: %w", err)
		}
		envelope, err := decodeSocketModeEnvelope(data)
		if err != nil {
			return fmt.Errorf("decode Slack envelope: %w", err)
		}
		if envelope.EnvelopeID == "" {
			if envelope.Type == "hello" {
				continue
			}
			return fmt.Errorf("Slack envelope missing envelope_id")
		}
		event, ok, err := eventFromSocketModeEnvelope(envelope)
		if err != nil {
			return fmt.Errorf("normalize Slack envelope: %w", err)
		}
		if ok {
			if err := handle(connCtx, event); err != nil {
				return fmt.Errorf("commit Slack envelope: %w", err)
			}
		}
		if err := acknowledge(conn, envelope.EnvelopeID); err != nil {
			return err
		}
		if envelope.Type == "disconnect" {
			return nil
		}
	}
}

func acknowledge(conn *websocket.Conn, id string) error {
	if err := conn.WriteJSON(map[string]string{"envelope_id": id}); err != nil {
		return fmt.Errorf("ack Slack envelope: %w", err)
	}
	return nil
}
