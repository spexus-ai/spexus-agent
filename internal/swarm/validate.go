package swarm

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var codePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`)

func uuid(s string) bool { return uuidPattern.MatchString(s) }
func utc(s string) bool {
	t, err := time.Parse(time.RFC3339Nano, s)
	return err == nil && strings.HasSuffix(s, "Z") && t.Location() == time.UTC
}
func decode(data []byte, v any) error {
	if !utf8.Valid(data) {
		return wireError(400, "invalid_utf8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return wireError(400, "invalid_json")
	}
	var x any
	if err := d.Decode(&x); err != io.EOF {
		return wireError(400, "invalid_json")
	}
	return jsonShape(data, reflect.TypeOf(v).Elem())
}

// JSON's null cannot silently become a Go zero value; all non-optional fields
// and nested objects are required even when their zero value is otherwise valid.
func jsonShape(data []byte, typ reflect.Type) error {
	if typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	if typ.Kind() == reflect.Pointer {
		if string(bytes.TrimSpace(data)) == "null" {
			return nil
		}
		return jsonShape(data, typ.Elem())
	}
	if string(bytes.TrimSpace(data)) == "null" {
		return wireError(400, "null_field")
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
			return wireError(400, "invalid_json")
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := field.Tag.Get("json")
			key := strings.Split(tag, ",")[0]
			if key == "-" || key == "" {
				continue
			}
			value, ok := fields[key]
			optional := strings.Contains(tag, ",omitempty") || (typ == reflect.TypeOf(DispatchPayload{}) && (key == "accept_by" || key == "run_timeout_seconds"))
			if !ok {
				if optional {
					continue
				}
				return wireError(400, "missing_field")
			}
			if err := jsonShape(value, field.Type); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var list []json.RawMessage
		if err := json.Unmarshal(data, &list); err != nil {
			return wireError(400, "invalid_json")
		}
		for _, v := range list {
			if err := jsonShape(v, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
func required(data []byte, keys ...string) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return wireError(400, "invalid_json")
	}
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			return wireError(400, "missing_field")
		}
	}
	return nil
}
func canonical(data []byte) ([]byte, error) {
	var value any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
func validateConfig(c Config) error {
	if !uuid(c.TenantID) || !uuid(c.ProjectID) || len(c.Agents) != 3 || len(c.Profiles) == 0 {
		return fmt.Errorf("invalid scope or three-agent configuration")
	}
	profiles := map[string]bool{}
	for _, snap := range c.Profiles {
		p, err := ValidateTextProfile(snap.Bytes)
		if err != nil {
			return err
		}
		if profiles[p.ID] {
			return fmt.Errorf("duplicate profile")
		}
		profiles[p.ID] = true
	}
	agents := map[string]bool{}
	tokens := map[string]bool{}
	owners := 0
	for _, a := range c.Agents {
		digest, err := hex.DecodeString(a.CredentialSHA256)
		if a.AgentID == "" || a.AgentID == "coordinator" || len(a.AgentID) > 128 || agents[a.AgentID] || tokens[a.CredentialSHA256] || err != nil || len(digest) != 32 || !profiles[a.ProfileID] {
			return fmt.Errorf("invalid agent configuration")
		}
		if a.Role == "owner" {
			owners++
		} else if a.Role != "worker" {
			return fmt.Errorf("invalid role")
		}
		agents[a.AgentID] = true
		tokens[a.CredentialSHA256] = true
	}
	if owners != 1 {
		return fmt.Errorf("exactly one owner required")
	}
	return nil
}

// ValidateTextProfile is shared by the trusted runner: P2 has no model tools or extensions.
func ValidateTextProfile(snapshot []byte) (TextProfile, error) {
	var p TextProfile
	if len(snapshot) > 128*1024 {
		return p, wireError(400, "profile_too_large")
	}
	if err := decode(snapshot, &p); err != nil {
		return p, err
	}
	if err := required(snapshot, "id", "model", "reasoning", "prompt", "tools", "extensions"); err != nil {
		return p, err
	}
	if !safeText(p.ID, 128) || !safeText(p.Model, 256) || !strings.Contains(p.Model, "/") || !safeText(p.Reasoning, 32) || !safeText(p.Prompt, 64*1024) || p.Tools == nil || p.Extensions == nil || len(p.Tools) != 0 || len(p.Extensions) != 0 {
		return p, wireError(400, "unsafe_profile")
	}
	return p, nil
}
func validateEvidence(e []Evidence) error {
	if e == nil || len(e) > 32 {
		return wireError(400, "invalid_evidence")
	}
	n := 0
	for _, v := range e {
		if v.Kind != "text" && v.Kind != "ref" || !safeText(v.Label, 256) || !safeText(v.ContentOrRef, 64*1024) {
			return wireError(400, "invalid_evidence")
		}
		n += len(v.ContentOrRef)
		if v.Kind == "ref" && (strings.HasPrefix(v.ContentOrRef, "/") || strings.HasPrefix(v.ContentOrRef, "file:")) {
			return wireError(400, "ephemeral_evidence")
		}
		if v.SHA256 != "" {
			b, err := hex.DecodeString(v.SHA256)
			if err != nil || len(b) != 32 {
				return wireError(400, "invalid_digest")
			}
		}
	}
	if n > 64*1024 {
		return wireError(400, "evidence_too_large")
	}
	return nil
}
func validateError(e *TaskError) error {
	if e == nil {
		return nil
	}
	if !codePattern.MatchString(e.Code) || !safeText(e.Message, 4096) {
		return wireError(400, "invalid_error")
	}
	return nil
}
func validateObservation(o *Observation) error {
	if o == nil {
		return nil
	}
	if o.Outcome != "succeeded" && o.Outcome != "failed" || len(o.Summary) > 16*1024 {
		return wireError(400, "invalid_observation")
	}
	if err := validateEvidence(o.Evidence); err != nil {
		return err
	}
	return validateError(o.Error)
}
func validateResult(r ResultPayload) error {
	if !terminal(r.Outcome) || !safeText(r.Summary, 16*1024) || r.Origin != "worker" && r.Origin != "coordinator" {
		return wireError(400, "invalid_result")
	}
	if r.Outcome == "succeeded" && r.Error != nil || r.Outcome != "succeeded" && r.Error == nil {
		return wireError(400, "invalid_error")
	}
	if err := validateEvidence(r.Evidence); err != nil {
		return err
	}
	if err := validateError(r.Error); err != nil {
		return err
	}
	return validateObservation(r.Observation)
}
func validateEnvelope(e Envelope) error {
	if len(mustJSON(e)) > MaxEnvelopeBytes {
		return wireError(413, "envelope_too_large")
	}
	if e.ProtocolVersion != 1 {
		return wireError(400, "unsupported_version")
	}
	if !uuid(e.MessageID) || !uuid(e.TenantID) || !uuid(e.ProjectID) || !uuid(e.FeatureID) || !utc(e.SentAt) || !safeText(e.FromAgentID, 128) || !safeText(e.ToAgentID, 128) {
		return wireError(400, "invalid_envelope")
	}
	if e.CausationID != nil && !uuid(*e.CausationID) {
		return wireError(400, "invalid_causation")
	}
	task := strings.HasPrefix(e.Type, "task.")
	if task && (!uuid(e.JobID) || !uuid(e.AttemptID)) {
		return wireError(400, "invalid_attempt")
	}
	if !task && (e.JobID != "" || e.AttemptID != "") {
		return wireError(400, "invalid_envelope")
	}
	action := e.Type == "task.dispatch" || e.Type == "task.review" || e.Type == "task.cancel"
	if action && e.FromAgentID != "coordinator" || e.Type == "turn.cancel" {
		if !uuid(e.OwnerTurnID) {
			return wireError(400, "invalid_owner_turn")
		}
	} else if e.OwnerTurnID != "" {
		return wireError(400, "unexpected_owner_turn")
	}
	if e.Type != "task.dispatch" && e.Type != "agent.input" && e.Type != "turn.cancel" && e.CausationID == nil {
		return wireError(400, "missing_causation")
	}
	switch e.Type {
	case "agent.input":
		var p InputPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if err := required(e.Payload, "text", "source"); err != nil {
			return err
		}
		if !safeText(p.Text, 64*1024) || p.Source.Kind != "slack" && p.Source.Kind != "test" || !safeText(p.Source.EventID, 256) || p.Source.ActorID == "" {
			return wireError(400, "invalid_input")
		}
	case "task.dispatch":
		var p DispatchPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if err := required(e.Payload, "goal", "scope", "expected_result", "context", "profile"); err != nil {
			return err
		}
		if !safeText(p.Goal, 8192) || !safeText(p.Scope, 8192) || len(p.ExpectedResult) < 1 || len(p.ExpectedResult) > 16 || len(p.Context.Text) > 64*1024 || p.Context.Refs == nil || len(p.Context.Refs) > 32 || !safeText(p.Profile.ID, 128) || len(p.Profile.Revision) != 64 || !safeText(p.Profile.Model, 256) || !safeText(p.Profile.Reasoning, 32) {
			return wireError(400, "invalid_dispatch")
		}
		for _, v := range p.ExpectedResult {
			if !safeText(v, 1024) {
				return wireError(400, "invalid_dispatch")
			}
		}
		for _, v := range p.Context.Refs {
			if v.Kind != "spexus" && v.Kind != "url" && v.Kind != "artifact" || !safeText(v.Ref, 2048) || len(v.Label) > 256 {
				return wireError(400, "invalid_context")
			}
		}
		if p.AcceptBy != "" && !utc(p.AcceptBy) || p.RunTimeoutSeconds < 0 || p.RunTimeoutSeconds > 3600 {
			return wireError(400, "invalid_deadline")
		}
	case "task.accepted":
		var p AcceptedPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if !uuid(p.DispatchMessageID) || len(p.ProfileRevision) != 64 {
			return wireError(400, "invalid_accepted")
		}
	case "task.started":
		var p StartedPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if !uuid(p.AcceptedMessageID) {
			return wireError(400, "invalid_started")
		}
	case "task.result":
		var p ResultPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if err := required(e.Payload, "outcome", "summary", "evidence", "error", "origin"); err != nil {
			return err
		}
		return validateResult(p)
	case "task.review":
		var p ReviewPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if !uuid(p.ResultMessageID) || p.Verdict != "accepted" && p.Verdict != "revise" || !safeText(p.Reason, 4096) {
			return wireError(400, "invalid_review")
		}
		return validateEvidence(p.Evidence)
	case "task.cancel", "turn.cancel":
		var p CancelPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if !safeText(p.Reason, 4096) || !safeText(p.RequestedBy, 256) {
			return wireError(400, "invalid_cancel")
		}
	default:
		return wireError(400, "unsupported_type")
	}
	return nil
}
