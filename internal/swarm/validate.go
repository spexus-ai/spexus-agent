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
	if !uuid(c.TenantID) || !uuid(c.ProjectID) || len(c.Agents) != 3 || c.AgentProfiles == nil || len(c.AgentProfiles.AllowedModels) == 0 {
		return fmt.Errorf("invalid scope or three-agent configuration")
	}
	if c.WireVersion != 0 && c.WireVersion != 1 && c.WireVersion != 2 {
		return fmt.Errorf("unsupported wire version")
	}
	if c.WireVersion == 2 && (c.Human == nil || !uuid(c.Human.EpicID) || !uuid(c.Human.WriterID) || c.Human.WorkspaceID == "" || c.Human.BaseURL == "" || c.Human.TokenFile == "") {
		return fmt.Errorf("wire version 2 requires human backend configuration")
	}
	profiles := map[string]bool{}
	for _, id := range []string{"orchestrator", "worker-a", "worker-b"} {
		profiles[id] = true
	}
	agents := map[string]bool{}
	tokens := map[string]bool{}
	owners := 0
	for _, a := range c.Agents {
		digest, err := hex.DecodeString(a.CredentialSHA256)
		if a.AgentID == "" || a.AgentID == "coordinator" || len(a.AgentID) > 128 || agents[a.AgentID] || tokens[a.CredentialSHA256] || err != nil || len(digest) != 32 || !profiles[a.ProfileID] || a.AgentID != a.ProfileID {
			return fmt.Errorf("invalid agent configuration")
		}
		if a.Role == "owner" && a.ProfileID == "orchestrator" {
			owners++
		} else if a.Role != "worker" || a.ProfileID == "orchestrator" {
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

// ValidateTextProfile is shared by the coordinator and runner. Container mounts,
// rather than the tool list, define which files a worker can change.
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
	if !safeText(p.ID, 128) || !safeText(p.Model, 256) || !strings.Contains(p.Model, "/") || !safeText(p.Reasoning, 32) || !safeText(p.Prompt, 64*1024) || p.Tools == nil || p.Extensions == nil || len(p.Extensions) != 0 || len(p.Tools) > 4 {
		return p, wireError(400, "unsafe_profile")
	}
	allowed := map[string]bool{"read": true, "write": true, "edit": true, "bash": true}
	seen := map[string]bool{}
	for _, tool := range p.Tools {
		if !allowed[tool] || seen[tool] {
			return p, wireError(400, "unsafe_profile")
		}
		seen[tool] = true
	}
	return p, nil
}

// ValidateWebTextProfile enforces the canonical, text-only v1 execution
// snapshot independently of the backend that supplied it.
func ValidateWebTextProfile(snapshot []byte) (TextProfile, error) {
	p, err := ValidateTextProfile(snapshot)
	if err != nil {
		return p, err
	}
	if len(p.Tools) != 0 || len(p.Extensions) != 0 || strings.TrimSpace(p.Prompt) == "" {
		return p, wireError(400, "unsafe_profile")
	}
	validReasoning := map[string]bool{"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true, "ultra": true}
	if !validReasoning[p.Reasoning] {
		return p, wireError(400, "unsafe_profile")
	}
	provider, model, ok := strings.Cut(p.Model, "/")
	if !ok || provider == "" || model == "" {
		return p, wireError(400, "unsafe_profile")
	}
	canonical, err := json.Marshal(p)
	if err != nil || !bytes.Equal(canonical, snapshot) {
		return p, wireError(400, "noncanonical_profile")
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
	if r.Outcome == "succeeded" && r.Error != nil || r.Outcome == "failed" && r.Error == nil || r.Outcome == "blocked" && (r.Error != nil || r.Blocker == nil) || r.Outcome != "blocked" && r.Blocker != nil || (r.Outcome == "cancelled" || r.Outcome == "interrupted") && r.Error == nil {
		return wireError(400, "invalid_error")
	}
	if r.Blocker != nil {
		if err := validateBlocker(*r.Blocker); err != nil {
			return err
		}
	}
	if err := validateEvidence(r.Evidence); err != nil {
		return err
	}
	if err := validateError(r.Error); err != nil {
		return err
	}
	return validateObservation(r.Observation)
}
func validateBlocker(b Blocker) error {
	if !safeText(b.Reason, 4096) || !safeText(b.Context, 16*1024) || !safeText(b.Question, 4096) || !safeText(b.Recommendation, 4096) || b.Options == nil || len(b.Options) > 8 {
		return wireError(400, "invalid_blocker")
	}
	switch b.Kind {
	case "clarification", "choice", "permission", "external_action", "blocker":
	default:
		return wireError(400, "invalid_blocker")
	}
	seen := map[string]bool{}
	for _, o := range b.Options {
		if len(o.ID) == 0 || len(o.ID) > 64 || o.ID == "text" || o.ID == "deny" || seen[o.ID] || !safeText(o.Label, 1024) {
			return wireError(400, "invalid_option")
		}
		for i, r := range o.ID {
			if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || i > 0 && (r == '_' || r == '-') {
				continue
			}
			return wireError(400, "invalid_option")
		}
		seen[o.ID] = true
	}
	return nil
}
func stepKey(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == ':' || r == '-' {
			continue
		}
		return false
	}
	return true
}
func validateEnvelope(e Envelope) error {
	if len(mustJSON(e)) > MaxEnvelopeBytes {
		return wireError(413, "envelope_too_large")
	}
	if e.ProtocolVersion != 1 && e.ProtocolVersion != 2 {
		return wireError(400, "unsupported_version")
	}
	if !uuid(e.MessageID) || !uuid(e.TenantID) || !uuid(e.ProjectID) || !uuid(e.FeatureID) || !utc(e.SentAt) || !safeText(e.FromAgentID, 128) || !safeText(e.ToAgentID, 128) {
		return wireError(400, "invalid_envelope")
	}
	if e.CausationID != nil && !uuid(*e.CausationID) {
		return wireError(400, "invalid_causation")
	}
	task := strings.HasPrefix(e.Type, "task.")
	if e.Type == "task.resume" {
		task = true
	}
	if task && (!uuid(e.JobID) || !uuid(e.AttemptID)) {
		return wireError(400, "invalid_attempt")
	}
	if !task && (e.JobID != "" || e.AttemptID != "") {
		return wireError(400, "invalid_envelope")
	}
	action := e.Type == "task.dispatch" || e.Type == "task.review" || e.Type == "task.cancel" || e.Type == "human.request" || e.Type == "human.respond" || e.Type == "dependency.resolve" || e.Type == "task.resume" || e.Type == "step.complete"
	if action && e.FromAgentID != "coordinator" || e.Type == "turn.cancel" {
		if !uuid(e.OwnerTurnID) {
			return wireError(400, "invalid_owner_turn")
		}
	} else if e.OwnerTurnID != "" {
		return wireError(400, "unexpected_owner_turn")
	}
	if e.Type != "task.dispatch" && e.Type != "task.resume" && e.Type != "human.request" && e.Type != "dependency.resolve" && e.Type != "step.complete" && e.Type != "agent.input" && e.Type != "turn.cancel" && e.CausationID == nil {
		return wireError(400, "missing_causation")
	}
	switch e.Type {
	case "human.request":
		if e.ProtocolVersion != 2 || e.JobID != "" || e.AttemptID != "" {
			return wireError(400, "invalid_human_request")
		}
		var p HumanRequestPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if err := validateBlocker(p.Blocker); err != nil {
			return err
		}
		if p.DependencyID == "" {
			if !safeText(p.BlockedWork, 16*1024) || !stepKey(p.StepKey) {
				return wireError(400, "invalid_owner_step")
			}
		} else if !uuid(p.DependencyID) || p.StepKey != "" || p.BlockedWork != "" {
			return wireError(400, "invalid_dependency")
		}
	case "dependency.resolve":
		if e.ProtocolVersion != 2 {
			return wireError(400, "unsupported_version")
		}
		var p ResolveDependencyPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if !uuid(p.DependencyID) || !safeText(p.Resolution, 16*1024) {
			return wireError(400, "invalid_resolution")
		}
		return validateEvidence(p.Evidence)
	case "task.resume":
		if e.ProtocolVersion != 2 {
			return wireError(400, "unsupported_version")
		}
		var p ResumeTaskPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if !uuid(p.DependencyID) || p.DecisionID != "" && !uuid(p.DecisionID) || p.WorkerAgentID != e.ToAgentID {
			return wireError(400, "invalid_resume")
		}
		dispatch := e
		dispatch.Type = "task.dispatch"
		dispatch.Payload = mustJSON(p.Dispatch)
		return validateEnvelope(dispatch)
	case "step.complete":
		if e.ProtocolVersion != 2 {
			return wireError(400, "unsupported_version")
		}
		var p CompleteStepPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if !uuid(p.DependencyID) || !uuid(p.DecisionID) || !safeText(p.Summary, 16*1024) {
			return wireError(400, "invalid_step_completion")
		}
	case "human.decision":
		if e.ProtocolVersion != 2 {
			return wireError(400, "unsupported_version")
		}
		var p HumanDecisionPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if !uuid(p.RequestID) || !uuid(p.DependencyID) || !uuid(p.DecisionID) || p.Revision != 2 {
			return wireError(400, "invalid_human_decision")
		}
	case "human.respond":
		if e.ProtocolVersion != 2 {
			return wireError(400, "unsupported_version")
		}
		var p HumanRespondPayload
		if err := decode(e.Payload, &p); err != nil {
			return err
		}
		if !uuid(p.RequestID) || !validSlackTS(p.SourceMessageTS) || (p.Kind != "answer" && p.Kind != "deny") || len(p.OptionID) > 64 || len(p.Text) > 16*1024 || (p.Kind == "deny" && (p.OptionID != "" || !safeText(p.Text, 16*1024))) {
			return wireError(400, "invalid_human_response")
		}
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
		if p.Source.MessageTS != "" && !validSlackTS(p.Source.MessageTS) || p.HumanAction != nil && (!uuid(p.HumanAction.RequestID) || p.HumanAction.OptionID == "") || p.ActiveHumanRequest != nil && !uuid(p.ActiveHumanRequest.RequestID) {
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
		if !safeText(p.Goal, 8192) || !safeText(p.Scope, 8192) || len(p.ExpectedResult) < 1 || len(p.ExpectedResult) > 16 || len(p.Context.Text) > 64*1024 || p.Context.Refs == nil || len(p.Context.Refs) > 32 || !safeText(p.Profile.ID, 128) || len(p.Profile.Revision) != 64 || p.Profile.Generation < 1 || !safeText(p.Profile.Model, 256) || !safeText(p.Profile.Reasoning, 32) {
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
		if p.Outcome == "blocked" && e.ProtocolVersion != 2 {
			return wireError(400, "unsupported_version")
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
