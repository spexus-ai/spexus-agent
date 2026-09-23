package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Store) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }
func sendJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func sendError(w http.ResponseWriter, err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		ae = &APIError{Status: 503, Code: "storage_unavailable", Message: "Storage operation failed", Retryable: true}
	}
	ae.Retryable = ae.Status == 429 || ae.Status == 503
	sendJSON(w, ae.Status, struct {
		Error     *APIError `json:"error"`
		RequestID string    `json:"request_id"`
	}{ae, NewID()})
}
func body(r *http.Request, v any, keys ...string) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return wireError(415, "json_required")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxEnvelopeBytes+1))
	if err != nil {
		return wireError(400, "invalid_body")
	}
	if len(b) > MaxEnvelopeBytes {
		return wireError(413, "envelope_too_large")
	}
	if err = decode(b, v); err != nil {
		return err
	}
	return required(b, keys...)
}
func integerQuery(r *http.Request, key string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, wireError(400, "invalid_query")
	}
	return n, nil
}
func (s *Store) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") {
		if err := s.db.PingContext(r.Context()); err != nil {
			sendError(w, err)
			return
		}
		sendJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.TLS == nil {
		sendError(w, wireError(403, "tls_required"))
		return
	}
	if !strings.HasPrefix(r.URL.Path, APIPrefix+"/") {
		sendError(w, wireError(404, "not_found"))
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		sendError(w, wireError(401, "unauthorized"))
		return
	}
	p, err := s.principal(strings.TrimPrefix(auth, "Bearer "), r.Header.Get("X-Agent-Instance-ID"))
	if err != nil {
		sendError(w, err)
		return
	}
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, APIPrefix)
	status := 200
	var out any
	switch {
	case path == "/agents/self/heartbeat" && r.Method == http.MethodPost:
		var v HeartbeatRequest
		err = body(r, &v, "instance_id", "active_attempt_id", "active_owner_turn_id")
		if err == nil {
			out, err = s.heartbeat(ctx, p, v)
		}
	case path == "/messages" && r.Method == http.MethodPost:
		var e Envelope
		err = body(r, &e, "protocol_version", "message_id", "type", "tenant_id", "project_id", "feature_id", "from_agent_id", "to_agent_id", "causation_id", "sent_at", "payload")
		if err == nil {
			var dup bool
			out, dup, err = s.postMessage(ctx, p, e)
			if !dup {
				status = 201
			}
		} else {
			s.reject(ctx, p, e, err)
		}
	case path == "/mailbox" && r.Method == http.MethodGet:
		var limit, wait int
		limit, err = integerQuery(r, "limit", 20, 1, 20)
		if err == nil {
			wait, err = integerQuery(r, "wait_seconds", 20, 0, 20)
		}
		lane := r.URL.Query().Get("lane")
		if lane == "" {
			lane = "normal"
		}
		if lane != "normal" && lane != "control" {
			err = wireError(400, "invalid_lane")
		}
		if err == nil {
			out, err = s.poll(ctx, p, lane, limit, wait)
		}
	case path == "/acks" && r.Method == http.MethodPost:
		var v AckRequest
		err = body(r, &v, "mailbox_seqs")
		if err == nil {
			out, err = s.ack(ctx, p, v)
		}
	case strings.HasPrefix(path, "/jobs/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "/jobs/")
		var limit int
		limit, err = integerQuery(r, "limit", 20, 1, 100)
		if !uuid(id) {
			err = wireError(400, "invalid_job")
		}
		if err == nil {
			out, err = s.job(ctx, p, id, limit, r.URL.Query().Get("cursor"))
		}
	case path == "/owner-turns/start" && r.Method == http.MethodPost:
		var v OwnerStartRequest
		err = body(r, &v, "turn_id", "feature_id", "input_mailbox_seq")
		if err == nil {
			var dup bool
			out, dup, err = s.startOwner(ctx, p, v)
			if !dup {
				status = 201
			}
		}
	case strings.HasPrefix(path, "/owner-turns/"):
		tail := strings.TrimPrefix(path, "/owner-turns/")
		id := strings.TrimSuffix(tail, "/finish")
		if !uuid(id) {
			err = wireError(400, "invalid_turn")
			break
		}
		if tail == id && r.Method == http.MethodGet {
			out, err = s.ownerTurn(ctx, p, id)
		} else if tail == id+"/finish" && r.Method == http.MethodPost {
			var v OwnerFinishRequest
			err = body(r, &v, "outcome", "reply", "actions", "error", "observation")
			if err == nil {
				var dup bool
				out, dup, err = s.finishOwner(ctx, p, id, v)
				if !dup {
					status = 201
				}
			}
		} else {
			err = wireError(404, "not_found")
		}
	default:
		err = wireError(404, "not_found")
	}
	if err != nil {
		sendError(w, err)
		return
	}
	sendJSON(w, status, out)
}
func (s *Store) poll(ctx context.Context, p Principal, lane string, limit, wait int) (MailboxResponse, error) {
	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := s.mailbox(ctx, p, lane, limit)
		if err != nil || len(out.Messages) > 0 || wait == 0 {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-deadline.C:
			return out, nil
		case <-ticker.C:
		}
	}
}
