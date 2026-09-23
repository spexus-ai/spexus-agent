package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
	"github.com/spexus-ai/spexus-agent/internal/swarmslack"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func load(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 2*1024*1024))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return fmt.Errorf("invalid configuration in %s", path)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing configuration data")
	}
	return nil
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: spexus-swarm-coordinator serve|history|reconcile")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	configPath := fs.String("config", "", "coordinator JSON configuration")
	state := fs.String("state", "", "SQLite state path")
	listen := fs.String("listen", ":8443", "private HTTPS address")
	cert := fs.String("tls-cert", "", "TLS certificate")
	key := fs.String("tls-key", "", "TLS key")
	slackPath := fs.String("slack-config", "", "private SlackAuth JSON file")
	feature := fs.String("feature-id", "", "feature UUID for history")
	agent := fs.String("agent-id", "", "agent UUID for offline reconciliation")
	oldID := fs.String("old-instance", "", "old instance UUID")
	newID := fs.String("new-instance", "", "new instance UUID")
	container := fs.String("container-id", "", "exact stopped runner container ID")
	coordinator := fs.String("coordinator-container-id", "", "exact stopped coordinator container ID")
	reason := fs.String("reason", "", "operator reason")
	actor := fs.String("actor", "", "operator identity")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *state == "" {
		return errors.New("--state is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "history" {
		h, err := swarm.ReadHistory(ctx, *state, *feature)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(h)
	}
	var cfg swarm.Config
	if err := load(*configPath, &cfg); err != nil {
		return err
	}
	if args[0] == "reconcile" {
		if *coordinator == *container {
			return errors.New("coordinator and runner container IDs must differ")
		}
		coordinatorLabels := map[string]string{"io.spexus.swarm.tenant-id": cfg.TenantID, "io.spexus.swarm.project-id": cfg.ProjectID, "io.spexus.swarm.role": "coordinator"}
		role := ""
		for _, a := range cfg.Agents {
			if a.AgentID == *agent {
				role = a.Role
			}
		}
		if role == "" {
			return errors.New("agent is not configured")
		}
		runnerLabels := map[string]string{"io.spexus.swarm.tenant-id": cfg.TenantID, "io.spexus.swarm.project-id": cfg.ProjectID, "io.spexus.swarm.role": role, "io.spexus.swarm.agent-id": *agent, "io.spexus.swarm.instance-id": *oldID}
		if err := verifyStopped(ctx, *coordinator, coordinatorLabels); err != nil {
			return fmt.Errorf("coordinator: %w", err)
		}
		if err := verifyStopped(ctx, *container, runnerLabels); err != nil {
			return fmt.Errorf("runner: %w", err)
		}
		return swarm.ReconcileOffline(ctx, *state, cfg, swarm.ReconcileRequest{AgentID: *agent, OldInstanceID: *oldID, NewInstanceID: *newID, Reason: *reason, Actor: *actor, ContainerID: *container, ContainerStopped: true, CheckedAt: time.Now().UTC()})
	}
	if args[0] != "serve" {
		return errors.New("unknown command")
	}
	if *cert == "" || *key == "" || *slackPath == "" {
		return errors.New("serve requires --tls-cert, --tls-key, --slack-config")
	}
	var auth config.SlackAuth
	if err := load(*slackPath, &auth); err != nil {
		return err
	}
	if auth.BotToken == "" || auth.AppToken == "" || auth.WorkspaceID == "" {
		return errors.New("SlackAuth botToken, appToken and workspaceId required")
	}
	store, err := swarm.Open(ctx, *state, cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	if cfg.WireVersion == 2 {
		if err = store.HumanPreflight(ctx); err != nil {
			return fmt.Errorf("human provider preflight: %w", err)
		}
		for _, f := range cfg.Features {
			if _, err = store.SlackWatermark(ctx, f.FeatureID); err != nil {
				return fmt.Errorf("initialize Slack catchup: %w", err)
			}
			if err = store.SetRecoveryBarrier(ctx, f.FeatureID, "slack_startup"); err != nil {
				return fmt.Errorf("close Slack startup barrier: %w", err)
			}
		}
	}
	source := slack.NewSocketModeClient(auth.AppToken)
	bridge := &swarmslack.Bridge{Store: store, Features: cfg.Features, API: swarmslack.NewAPI(auth.BotToken), Logf: log.Printf}
	server := &http.Server{Addr: *listen, Handler: store.Handler(), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second}
	errorsCh := make(chan error, 4)
	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		err := server.ListenAndServeTLS(*cert, *key)
		if !errors.Is(err, http.ErrServerClosed) {
			errorsCh <- err
		}
	}()
	go func() {
		defer workers.Done()
		if cfg.WireVersion == 2 {
			errorsCh <- bridge.RunHuman(ctx, source, auth.WorkspaceID)
		} else {
			errorsCh <- bridge.Run(ctx, source)
		}
	}()
	go func() {
		defer workers.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := store.Sweep(ctx); err != nil {
					errorsCh <- err
					return
				}
			}
		}
	}()
	go func() {
		defer workers.Done()
		if cfg.WireVersion != 2 {
			return
		}
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if e := store.SyncHuman(ctx); e != nil {
					log.Printf("human sync: %v", e)
				}
				if e := store.ApplyPendingHuman(ctx); e != nil {
					log.Printf("human application: %v", e)
				}
			}
		}
	}()
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
		cancel()
	}
	cancel()
	source.Close()
	shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	shutdownErr := server.Shutdown(shutdownCtx)
	workers.Wait()
	if err != nil {
		return err
	}
	return shutdownErr
}

// Recovery is an OS-operator operation, performed outside the service container.
// The service image intentionally has neither Docker CLI nor Docker socket.
func verifyStopped(ctx context.Context, id string, labels map[string]string) error {
	if len(id) != 64 || strings.IndexFunc(id, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) >= 0 {
		return errors.New("full container ID required")
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .}}", id).Output()
	if err != nil {
		return errors.New("cannot verify container state using Docker")
	}
	return validateStopped(out, id, labels)
}
func validateStopped(out []byte, id string, labels map[string]string) error {
	var inspected struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Running    bool   `json:"Running"`
			Paused     bool   `json:"Paused"`
			Restarting bool   `json:"Restarting"`
			PID        int    `json:"Pid"`
			Status     string `json:"Status"`
		} `json:"State"`
	}
	if json.Unmarshal(out, &inspected) != nil || inspected.ID != id {
		return errors.New("container identity mismatch")
	}
	for key, value := range labels {
		if value == "" || inspected.Config.Labels[key] != value {
			return errors.New("container does not match configured scope/agent/old instance")
		}
	}
	if inspected.State.Running || inspected.State.Paused || inspected.State.Restarting || inspected.State.PID != 0 || (inspected.State.Status != "exited" && inspected.State.Status != "dead" && inspected.State.Status != "created") {
		return errors.New("container is not verifiably stopped")
	}
	return nil
}
