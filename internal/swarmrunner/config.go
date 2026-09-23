// Package swarmrunner runs one text-only Pi participant over the durable P2 API.
package swarmrunner

import (
	"bytes"
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
	ProfileFile string `json:"profile_file"`
}
type Config struct {
	CoordinatorURL string   `json:"coordinator_url"`
	CAFile         string   `json:"ca_file"`
	CredentialFile string   `json:"credential_file"`
	TenantID       string   `json:"tenant_id"`
	ProjectID      string   `json:"project_id"`
	AgentID        string   `json:"agent_id"`
	InstanceID     string   `json:"instance_id"`
	Role           string   `json:"role"`
	ProfileFile    string   `json:"profile_file"`
	StateDirectory string   `json:"state_directory"`
	Workspace      string   `json:"workspace"`
	PiBinary       string   `json:"pi_binary"`
	Targets        []Target `json:"targets"`
}
type profile struct {
	swarm.TextProfile
	Revision string
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
	for _, p := range []string{c.CAFile, c.CredentialFile, c.ProfileFile, c.StateDirectory, c.Workspace} {
		if !filepath.IsAbs(p) {
			return errors.New("config paths must be absolute")
		}
	}
	seen := map[string]bool{}
	for _, t := range c.Targets {
		if t.AgentID == "" || seen[t.AgentID] || !filepath.IsAbs(t.ProfileFile) {
			return errors.New("invalid target")
		}
		seen[t.AgentID] = true
	}
	if c.Role == "worker" && len(c.Targets) > 0 {
		return errors.New("worker cannot configure dispatch targets")
	}
	return nil
}
func loadProfile(path string) (profile, error) {
	var p profile
	b, e := os.ReadFile(path)
	if e != nil {
		return p, e
	}
	p.TextProfile, e = swarm.ValidateTextProfile(b)
	if e != nil {
		return p, e
	}

	provider, model, ok := strings.Cut(p.Model, "/")
	if !ok || provider == "" || model == "" {
		return p, fmt.Errorf("profile_unavailable: provider/model required")
	}
	p.Revision = swarm.Digest(b)
	return p, nil
}
func (p profile) wire() swarm.Profile {
	return swarm.Profile{ID: p.ID, Revision: p.Revision, Model: p.Model, Reasoning: p.Reasoning}
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
