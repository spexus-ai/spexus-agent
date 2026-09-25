// Package swarmrunner runs one text-only Pi participant over the durable P2 API.
package swarmrunner

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type Target struct {
	AgentID     string `json:"agent_id"`
	ProfileID   string `json:"profile_id"`
	ProfileFile string `json:"-"` // offline fixture only; runtime JSON rejects profile_file
}
type Config struct {
	WireVersion     int      `json:"wire_version,omitempty"`
	CoordinatorURL  string   `json:"coordinator_url"`
	CAFile          string   `json:"ca_file"`
	CredentialFile  string   `json:"credential_file"`
	TenantID        string   `json:"tenant_id"`
	ProjectID       string   `json:"project_id"`
	AgentID         string   `json:"agent_id"`
	InstanceID      string   `json:"instance_id"`
	Role            string   `json:"role"`
	ProfileID       string   `json:"profile_id"`
	ProfileFile     string   `json:"-"` // offline fixture only; runtime JSON rejects profile_file
	StateDirectory  string   `json:"state_directory"`
	Workspace       string   `json:"workspace"`
	PiBinary        string   `json:"pi_binary"`
	AvailableModels []string `json:"available_models"`
	Targets         []Target `json:"targets"`
}
type profile struct {
	swarm.TextProfile
	Revision   string
	Generation int64
	Bytes      []byte
}
type targetProfile struct {
	AgentID string        `json:"agent_id"`
	Profile swarm.Profile `json:"profile"`
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func decode(b []byte, out any) error {
	if len(b) > swarm.MaxEnvelopeBytes {
		return errors.New("JSON too large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}
func LoadConfig(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	if e = decode(b, &c); e != nil {
		return c, e
	}
	return c, c.validate()
}
func (c Config) validate() error {
	if c.WireVersion != 0 && c.WireVersion != 1 && c.WireVersion != 2 {
		return errors.New("unsupported wire version")
	}
	u, e := url.Parse(c.CoordinatorURL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("coordinator_url must be an HTTPS origin")
	}
	if !uuid.MatchString(c.TenantID) || !uuid.MatchString(c.ProjectID) || !uuid.MatchString(c.InstanceID) || strings.TrimSpace(c.AgentID) == "" {
		return errors.New("invalid identity/scope")
	}
	if c.Role != "owner" && c.Role != "worker" {
		return errors.New("invalid role")
	}
	for _, p := range []string{c.CAFile, c.CredentialFile, c.StateDirectory, c.Workspace} {
		if !filepath.IsAbs(p) {
			return errors.New("config paths must be absolute")
		}
	}
	if c.Role == "owner" && c.ProfileID != "orchestrator" || c.Role == "worker" && c.ProfileID != c.AgentID || c.ProfileID == "" {
		return errors.New("invalid profile binding")
	}
	if len(c.AvailableModels) == 0 {
		return errors.New("runtime model allowlist required")
	}
	seen := map[string]bool{}
	for _, t := range c.Targets {
		if t.AgentID == "" || seen[t.AgentID] || t.ProfileID != t.AgentID || t.ProfileID != "worker-a" && t.ProfileID != "worker-b" {
			return errors.New("invalid target")
		}
		seen[t.AgentID] = true
	}
	if c.Role == "worker" && len(c.Targets) > 0 {
		return errors.New("worker cannot configure dispatch targets")
	}
	return nil
}
func profileFromActive(active swarm.ActiveProfile, expectedID string, available []string) (profile, error) {
	var p profile
	b, e := base64.StdEncoding.DecodeString(active.SnapshotBytesBase64)
	if e != nil || active.ProfileID != expectedID || active.Generation < 1 || !active.Enabled || swarm.Digest(b) != active.ActiveRevision || !bytes.Equal(b, active.SnapshotJSON) {
		return p, errors.New("profile_unavailable")
	}
	p.TextProfile, e = swarm.ValidateWebTextProfile(b)
	if e != nil {
		return p, e
	}
	if p.ID != expectedID || len(p.Tools) != 0 || len(p.Extensions) != 0 {
		return p, errors.New("profile_unavailable")
	}
	provider, model, ok := strings.Cut(p.Model, "/")
	if !ok || provider == "" || model == "" {
		return p, fmt.Errorf("profile_unavailable: provider/model required")
	}
	allowed := false
	for _, candidate := range available {
		if p.Model == candidate {
			allowed = true
			break
		}
	}
	if !allowed {
		return p, errors.New("profile_unavailable: model not installed")
	}
	p.Revision = swarm.Digest(b)
	p.Generation = active.Generation
	p.Bytes = b
	return p, nil
}

// loadProfile validates one operator import file; the runner never loads it.
func loadProfile(path string) (profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return profile{}, err
	}
	p, err := swarm.ValidateTextProfile(b)
	if err != nil {
		return profile{}, err
	}
	return profile{TextProfile: p, Revision: swarm.Digest(b), Bytes: b}, nil
}
func (p profile) wire() swarm.Profile {
	return swarm.Profile{ID: p.ID, Revision: p.Revision, Generation: p.Generation, Model: p.Model, Reasoning: p.Reasoning}
}

func requiredKeys(raw []byte, keys ...string) error {
	var m map[string]json.RawMessage
	if e := json.Unmarshal(raw, &m); e != nil {
		return e
	}
	if m == nil {
		return errors.New("object required")
	}
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			return fmt.Errorf("missing field %s", key)
		}
	}
	return nil
}
