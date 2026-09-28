# Prototype 2 coordinator and Slack boundary

Build the local stand image with `docker build -f Dockerfile.swarm -t spexus-swarm-p2:<source-sha> .`. The independently cacheable `pi-runtime` stage pins Node 22.19.0 and Pi 0.84.2. The final image contains CGO/SQLite coordinator and runner executables, uses UID/GID 10001, and includes curl for TLS readiness checks. Supply secrets only as private runtime mounts; no configuration, tokens, session files or state are copied into the build stages. Compose resource/network/volume policy belongs to the infra component.

Coordinator command (inside its private network):

```sh
/usr/local/bin/spexus-swarm-coordinator serve \
  --config /config/coordinator.json --state /state/coordinator.db \
  --tls-cert /tls/server.crt --tls-key /tls/server.key \
  --slack-config /secrets/slack.json --listen :8443
```

The coordinator JSON includes exactly three bound profile IDs (`orchestrator`, `worker-a`, `worker-b`) and `agent_profiles: {base_url, ca_file, token_file, allowed_models}`. This block points to the backend profile API and a scoped runtime service JWT outside the browser. Profile snapshots are not embedded in coordinator JSON. Startup preflights all three backend snapshots; dispatch and launch fail closed if the backend, digest, schema, slot, or model policy is invalid. `slack.json` is the existing `config.SlackAuth` object (`botToken`, `appToken`, `workspaceId`). Only the coordinator gets Slack and backend credentials. Runners get separate hashed-bound runtime credentials and private Pi auth mounts.

For an isolated preview fixture, start with `--local-fixture` instead of `--slack-config`. This mode opens no Slack Socket Mode connection and sends no Slack messages. Stop the coordinator before each offline fixture command, then restart `serve --local-fixture` to process a released input. Every fixture command requires a stable `--fixture-event-id`; repeat the same command with the same ID and payload to receive a duplicate receipt without another owner delivery. A different payload under the same ID fails with `source_conflict`. The fixture uses the configured feature's channel, thread, and first allowed actor.

```sh
/usr/local/bin/spexus-swarm-coordinator inject-fixture --config /config/coordinator.json --state /state/coordinator.db --local-fixture --feature-id FEATURE_UUID --fixture-event-id input-1 --fixture-text 'First request'
/usr/local/bin/spexus-swarm-coordinator stop-fixture --config /config/coordinator.json --state /state/coordinator.db --local-fixture --feature-id FEATURE_UUID --fixture-event-id stop-1
/usr/local/bin/spexus-swarm-coordinator inject-fixture --config /config/coordinator.json --state /state/coordinator.db --local-fixture --feature-id FEATURE_UUID --fixture-event-id input-while-stopped --fixture-text 'Continue after stop'
/usr/local/bin/spexus-swarm-coordinator continue-fixture --config /config/coordinator.json --state /state/coordinator.db --local-fixture --feature-id FEATURE_UUID --fixture-event-id continue-1
```

The stopped input returns `status:"buffered"` and creates no owner mailbox delivery. Continue requires zero unresolved active work; it supersedes old queued inputs, opens the feature, and releases buffered fixture inputs once. JSON receipts contain the fixture source ID, status, duplicate flag, and delivered message/mailbox IDs. Wire version 1 has no human-request or human-decision protocol; a genuine decision preview requires a separate wire-version-2 feature and human backend. The local fixture does not authorize a production Slack rollout.

For that separate wire-version-2 fixture, configure the real scoped human backend, let a blocked worker and owner create an open backend human request, and use the resulting `request_id`. With the coordinator stopped, submit an offline source ID in Slack timestamp form, later than the fixture question, through the same validated coordinator decision path:

```sh
/usr/local/bin/spexus-swarm-coordinator decide-fixture --config /config/coordinator.json --state /state/coordinator.db --local-fixture --feature-id FEATURE_UUID --fixture-event-id 1790000001.000001 --request-id REQUEST_UUID --decision-kind answer --decision-option-id approved --decision-text 'Approved'
```

The command commits the synthetic source with the feature's configured workspace, thread and allowed actor, then calls the existing human answer, backend sync and local application operations. Its JSON receipt includes the source ID, request and operation IDs, duplicate flag, backend sync status and local application status. Repeating the exact command uses the same source and decision operation; conflicting source content is rejected. It rejects wire version 1 or a missing genuine request. No Slack message is posted, including the question; inspect the backend request and coordinator history to verify the decision and single owner continuation.

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
