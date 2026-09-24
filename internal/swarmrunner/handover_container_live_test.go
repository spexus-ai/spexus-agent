//go:build handoverlive

package swarmrunner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func newHandoverServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "host.docker.internal"},
		DNSNames:     []string{"host.docker.internal"},
		NotBefore:    time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKey}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener, err = net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return server, "https://host.docker.internal:" + port
}

func runContainerHandover(t *testing.T, ctx context.Context, root, workspace, marker, featureID string, cfg swarm.Config, store *swarm.Store, caPath, coordinatorURL, image string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	uid := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	testID := strings.ReplaceAll(swarm.NewID()[:8], "-", "")
	name := func(id string) string { return "spexus-handover-" + testID + "-" + id }
	docker := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "docker", args...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	started := []string{}
	defer func() {
		for _, id := range started {
			_, _ = exec.Command("docker", "rm", "-f", name(id)).CombinedOutput()
		}
	}()
	for _, id := range []string{"orchestrator", "coder", "reviewer"} {
		for _, path := range []string{filepath.Join(root, "state", id), filepath.Join(root, "pi", id), filepath.Join(root, "home", id)} {
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		for _, file := range []string{"auth.json", "models-store.json"} {
			content, err := os.ReadFile(filepath.Join(home, ".pi", "agent", file))
			if err != nil {
				t.Fatalf("Pi credential source %s: %v", file, err)
			}
			if err := os.WriteFile(filepath.Join(root, "pi", id, file), content, 0600); err != nil {
				t.Fatal(err)
			}
		}
		tokenPath := filepath.Join(root, id+".token")
		if err := os.WriteFile(tokenPath, []byte("handover-token-"+id), 0600); err != nil {
			t.Fatal(err)
		}
		role := "worker"
		var targets []Target
		if id == "orchestrator" {
			role = "owner"
			targets = []Target{{AgentID: "coder", ProfileFile: "/profiles/coder.json"}, {AgentID: "reviewer", ProfileFile: "/profiles/reviewer.json"}}
		}
		config := Config{WireVersion: 2, CoordinatorURL: coordinatorURL, CAFile: "/tls/ca.crt", CredentialFile: "/secrets/runtime.token", TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, AgentID: id, InstanceID: swarm.NewID(), Role: role, ProfileFile: "/profiles/" + id + ".json", StateDirectory: "/state", Workspace: "/workspace", PiBinary: "pi", Targets: targets}
		b, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, id+".config.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ownerWorkspace := filepath.Join(root, "owner-workspace")
	if err := os.MkdirAll(ownerWorkspace, 0700); err != nil {
		t.Fatal(err)
	}
	start := func(id string, writable bool) {
		t.Helper()
		mountWorkspace := workspace
		if id == "orchestrator" {
			mountWorkspace = ownerWorkspace
		}
		workspaceMount := "type=bind,src=" + mountWorkspace + ",dst=/workspace"
		if !writable {
			workspaceMount += ",readonly"
		}
		args := []string{"run", "-d", "--pull", "never", "--name", name(id), "--init", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "64", "--memory", "768m", "--cpus", "1", "--user", uid, "--tmpfs", "/tmp:size=32m,mode=1777", "--env", "HOME=/home/swarm", "--env", "PI_CODING_AGENT_DIR=/pi",
			"--mount", "type=bind,src=" + filepath.Join(root, "profiles") + ",dst=/profiles,readonly",
			"--mount", "type=bind,src=" + caPath + ",dst=/tls/ca.crt,readonly",
			"--mount", "type=bind,src=" + filepath.Join(root, id+".token") + ",dst=/secrets/runtime.token,readonly",
			"--mount", "type=bind,src=" + filepath.Join(root, id+".config.json") + ",dst=/config/runner.json,readonly",
			"--mount", "type=bind,src=" + filepath.Join(root, "state", id) + ",dst=/state",
			"--mount", "type=bind,src=" + filepath.Join(root, "pi", id) + ",dst=/pi",
			"--mount", "type=bind,src=" + filepath.Join(root, "home", id) + ",dst=/home/swarm",
			"--mount", workspaceMount,
			"--entrypoint", "/usr/local/bin/spexus-swarm-runner", image, "--config", "/config/runner.json"}
		if out, err := docker(args...); err != nil {
			t.Fatalf("start %s: %v: %s", id, err, out)
		}
		started = append(started, id)
	}
	status := func(id string) string {
		out, _ := docker("inspect", "--format", "{{.State.Status}}", name(id))
		return out
	}
	logs := func(id string) string {
		out, _ := docker("logs", "--tail", "30", name(id))
		return out
	}
	mountWritable := func(id string) bool {
		t.Helper()
		out, err := docker("inspect", "--format", "{{json .Mounts}}", name(id))
		if err != nil {
			t.Fatalf("inspect %s: %v: %s", id, err, out)
		}
		var mounts []struct {
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}
		if err := json.Unmarshal([]byte(out), &mounts); err != nil {
			t.Fatal(err)
		}
		for _, mount := range mounts {
			if mount.Destination == "/workspace" {
				return mount.RW
			}
		}
		t.Fatalf("%s has no workspace mount", id)
		return false
	}
	start("orchestrator", true)
	start("coder", true)
	if !mountWritable("coder") {
		t.Fatal("coder workspace is not writable")
	}
	_, _, err = store.Ingest(ctx, featureID, swarm.InputPayload{Text: "Fix the prepared checkout, then hand its files to a separate reviewer.", Source: swarm.Source{Kind: "slack", EventID: "handover-container-1", ChannelID: "C1234567890", ThreadTS: "1234567890.000001", ActorID: "U1234567890"}})
	if err != nil {
		t.Fatal(err)
	}
	wait := func(stage string, ready func(swarm.History) bool) swarm.History {
		t.Helper()
		for ctx.Err() == nil {
			h, err := store.History(ctx, featureID)
			if err != nil {
				t.Fatal(err)
			}
			if ready(h) {
				return h
			}
			if status("orchestrator") != "running" || stage == "coder" && status("coder") != "running" || stage == "reviewer" && status("reviewer") != "running" {
				t.Fatalf("container stopped during %s: owner=%s coder=%s reviewer=%s; logs owner=%q coder=%q reviewer=%q", stage, status("orchestrator"), status("coder"), status("reviewer"), logs("orchestrator"), logs("coder"), logs("reviewer"))
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("timed out during %s: %v", stage, ctx.Err())
		return swarm.History{}
	}
	first := wait("coder", func(h swarm.History) bool {
		return len(h.Jobs) == 2 && len(h.Jobs[0].Attempts) == 1 && h.Jobs[0].Attempts[0].AssignedAgentID == "coder" && h.Jobs[0].Attempts[0].Review == "accepted" && len(h.Jobs[1].Attempts) == 1 && h.Jobs[1].Attempts[0].AssignedAgentID == "reviewer"
	})
	if first.Jobs[0].Attempts[0].Result == nil || first.Jobs[0].Attempts[0].Result.Outcome != "succeeded" {
		t.Fatalf("coder did not succeed: %+v", first.Jobs[0].Attempts[0])
	}
	code, err := os.ReadFile(filepath.Join(workspace, "calc.js"))
	if err != nil || !strings.Contains(string(code), marker) || !strings.Contains(string(code), "a + b") {
		t.Fatalf("coder file change missing: %v, %s", err, code)
	}
	if out, err := docker("stop", "--time", "10", name("coder")); err != nil {
		t.Fatalf("stop coder: %v: %s", err, out)
	}
	if status("coder") == "running" {
		t.Fatal("coder retained its writer mount")
	}
	start("reviewer", false)
	if mountWritable("reviewer") {
		t.Fatal("reviewer workspace is writable")
	}
	if out, err := docker("exec", name("reviewer"), "sh", "-c", "touch /workspace/forbidden"); err == nil {
		t.Fatalf("reviewer unexpectedly wrote to workspace: %s", out)
	}
	final := wait("reviewer", func(h swarm.History) bool {
		return len(h.Jobs) == 2 && len(h.Jobs[1].Attempts) == 1 && h.Jobs[1].Attempts[0].Review == "accepted" && len(h.SlackOutbox) > 0
	})
	result := final.Jobs[1].Attempts[0].Result
	if result == nil || result.Outcome != "succeeded" {
		t.Fatalf("reviewer did not succeed: %+v", final.Jobs[1].Attempts[0])
	}
	evidence, _ := json.Marshal(result.Evidence)
	if !strings.Contains(string(evidence), marker) {
		t.Fatalf("reviewer did not read checkpoint %q: %s", marker, evidence)
	}
	if len(final.Jobs) != 2 || len(final.Jobs[0].Attempts) != 1 || len(final.Jobs[1].Attempts) != 1 {
		t.Fatalf("duplicate execution: %+v", final.Jobs)
	}
	t.Logf("container handover verified: coder job=%s reviewer job=%s checkpoint=%s final_reply=%q", final.Jobs[0].JobID, final.Jobs[1].JobID, marker, final.SlackOutbox[len(final.SlackOutbox)-1].Text)
}
