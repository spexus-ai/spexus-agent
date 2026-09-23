# Prototype 2 coordinator and Slack boundary

Build the local stand image with `docker build -f Dockerfile.swarm -t spexus-swarm-p2:<source-sha> .`. The independently cacheable `pi-runtime` stage pins Node 22.19.0 and Pi 0.84.2. The final image contains CGO/SQLite coordinator and runner executables, uses UID/GID 10001, and includes curl for TLS readiness checks. Supply secrets only as private runtime mounts; no configuration, tokens, session files or state are copied into the build stages. Compose resource/network/volume policy belongs to the infra component.

Coordinator command (inside its private network):

```sh
/usr/local/bin/spexus-swarm-coordinator serve \
  --config /config/coordinator.json --state /state/coordinator.db \
  --tls-cert /tls/server.crt --tls-key /tls/server.key \
  --slack-config /secrets/slack.json --listen :8443
```

The coordinator JSON is exactly `swarm.Config` described in `swarm-api.md`, including static feature UUID, owner, Slack channel/thread anchor and permitted actor IDs. `slack.json` is exactly the existing `config.SlackAuth` object (`botToken`, `appToken`, `workspaceId`), not a wrapper. Only this process gets Slack credentials and Socket Mode. The old P1 consumer of the same Slack app must be stopped for the test and deliberately restored afterward. Runners get separate hashed-bound runtime credentials and private Pi auth mounts, never Slack credentials.

A verified human event in the registered thread enters `agent.input`. Existing Socket Mode normalization excludes bot/app/subtype events. Unknown threads and other actors are ignored. `!status` renders durable job/attempt state; `!stop` closes the feature and requests out-of-band cancellation; `!continue` reopens only after unresolved work is handled and does not replay any model call. Send a new instruction to continue. Replies and safe command errors use a durable Slack outbox; model replies can only appear after an owner finish transaction.

Slack publication attaches the outbox UUID as `client_msg_id` and message metadata. Lost responses become `delivery_unknown`, visible in history. Reconciliation paginates the original thread looking for that UUID; a found message settles the existing record. Only a complete lookup proving absence requeues. Missing Slack history permissions, incomplete pages or transport errors leave the record unresolved for the operator. This does not claim exactly-once publication by Slack. Readiness verifies the state store; Slack connection health is independently observable through source traffic and outbox status.

## Safe history and offline reconciliation

History is a read-only coherent SQLite snapshot, safe while the coordinator runs:

```sh
/usr/local/bin/spexus-swarm-coordinator history \
  --state /state/coordinator.db --feature-id FEATURE_UUID
```

It exports messages, assignments, acceptance/start/result/review transitions, instance diagnostics and Slack outbox status. It omits credentials and full profile prompts. Task payloads/evidence remain operator data and should be handled accordingly.

Recovery requires an OS operator on the VM host with Docker access. The service image deliberately has no Docker socket or Docker CLI. Extract the exact coordinator binary from the tested image using a temporary **unstarted** container (`docker create`, `docker cp`, then `docker rm`); run that binary on the matching Debian/Linux VM host. Stop the affected runner and coordinator first, retaining their containers and volumes. Use their full 64-character IDs, and the coordinator state's host bind-mount path:

```sh
/path/to/extracted/spexus-swarm-coordinator reconcile \
  --config /host/path/coordinator.json --state /host/path/coordinator.db \
  --agent-id AGENT_ID --old-instance OLD_UUID --new-instance NEW_UUID \
  --container-id FULL_STOPPED_RUNNER_ID \
  --coordinator-container-id FULL_STOPPED_COORDINATOR_ID \
  --actor OPERATOR --reason 'Inspected effects; old processes stopped'
```

The command runs Docker inspect itself, verifies exact identities, non-running/non-restarting state and PID zero, then takes the exclusive coordinator state lock. Immutable container labels must match: `io.spexus.swarm.tenant-id` and `io.spexus.swarm.project-id` on all containers; coordinator `io.spexus.swarm.role=coordinator`; runners `io.spexus.swarm.role=owner|worker`, `io.spexus.swarm.agent-id` equal to the configured logical agent, and `io.spexus.swarm.instance-id` equal to the old instance being reconciled. An unrelated stopped container is rejected. When creating a new process generation, update both runner config and instance label. It never accepts a caller-provided boolean as cessation proof. It does not launch Pi or change profiles. Update the runner configuration to the new instance UUID only after successful reconciliation, then restart the retained state. Never remove the database or restore an old state snapshot to retry an unknown launch. Full crash recovery remains P3.

## Verification boundary

Normal scoped check: `go test -race ./internal/swarmslack ./cmd/spexus-swarm-coordinator`.

The permanent integration fixture starts a real HTTPS coordinator handler with file SQLite and three real Runner OS processes. Each uses the real Pi adapter with a deterministic Pi-RPC subprocess helper inside the Go test executable. This checks messaging and process boundaries without a paid provider; it is **not** an installed-Pi or live-Slack test. It verifies two private worker contexts, owner reviews and combined anchored reply, duplicate inputs/dispatch/results, foreign and stale-attempt rejection, unreachable recipient timeout, launch counts, stop/continue guards, and unknown Slack publication reconciliation. Slack HTTP fixtures check correlation and pagination; incomplete history never authorizes resend.

SP-TASK-973 separately supplies actual installed Pi + Docker + Slack evidence, exact deployed source/image identity, five live goal cases and source-thread links. Unit/integration results do not satisfy that live gate.
