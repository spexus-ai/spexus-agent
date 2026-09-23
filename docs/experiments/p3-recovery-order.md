# P3 recovery-order experiment

2026-09-23. Design: SP-STD-022, epic SP-EP-026. Base:
`9f811ece1c0807d78a56d0d16dd89fdec983b331` (accepted agent PR #6).
This is a test-only experiment; production code and schemas are unchanged.

## Question and method

Can a stopped owner's committed output be delivered without another model turn,
and does applying a missed stop before replay prevent new dispatches?

The opt-in test starts the real P2 runner in a child OS process against a real
HTTPS coordinator and separate file-backed SQLite journals. A deterministic
Model fixture returns two dispatch actions. The parent blocks an HTTP request
after the runner's output transaction, kills and reaps the child, and reopens
the journal. Recovery uses the existing `flush` and coordinator validators before
`ReconcileOffline` rotates the instance. Each scenario compares durable identities,
receipts, typed output, job count, owner-turn state and a fsynced model-call counter.

The negative control rotates the instance first, reproducing the current P2 gap.
The driver's deliberate ordering is an experimental recovery procedure, not a
new production recovery implementation or a public authority bypass.

## Observed results

| Scenario | Jobs before → after | Final owner state | Pending outbox after |
|---|---:|---|---:|
| Rotate first (negative control) | 0 → 0 | interrupted | 1 |
| Deliver saved output before rotation | 0 → 2 | succeeded | 0 |
| Apply missed stop before delivery | 0 → 0 | interrupted | 0 |
| First action already acknowledged | 1 → 2 | succeeded | 0 |
| First action committed, HTTP receipt lost | 1 → 2 | succeeded | 0 |
| Temporary 503 barrier, journal reopened, retry | 0 → 2 | succeeded | 0 |

All six cases passed their assertions with the race detector. Each case made one
owner model-fixture call, preserved its typed output and original message IDs,
and rejected the old instance with `instance_conflict` after rotation. Repeated
flush did not add jobs; already stored action receipts stayed stored. A temporary
503 left actions pending rather than permanently rejected.

The negative control is an expected failure of the old recovery ordering: the
output still exists in the journal, but rotation removes its delivery authority.
It is not evidence that P2 already implements the P3 recovery contract.

## Reproduce

From the agent repository, with Go and the normal SQLite CGO prerequisites:

```sh
SPEXUS_P3_EVIDENCE_DIR=/absolute/path/to/new-evidence \
  go test -race -tags=p3experiment ./internal/swarmrunner \
  -run '^TestP3CommittedOwnerOutputRecoveryExperiment$' -count=1 -timeout=120s -v
```

The helper test is child-only. The build tag keeps the experiment out of the
ordinary test suite. Evidence directories are unique per run and include child
logs, before/after coordinator and runner snapshots, raw SQLite journals, the
model-call counter and a scenario summary. Credentials in these fixtures are
synthetic test strings; no real provider or Slack credentials are read.

The broader `go test -race -tags=p3experiment ./internal/swarmrunner -count=1`
also passed, including the existing runner tests. The default test target remains
`make tests` / `go test ./...`; this experiment does not require a paid provider.

## Limits and next experiment

- Slack history/catchup and its maintenance barrier are driver-controlled; no
  Slack socket, pagination, retention or late live-event race is validated.
- There are no worker model processes. Two jobs means two durable queued jobs,
  not two successful worker executions or a measured continuation launch count.
- The owner Model is deterministic; Pi/provider interoperability is not tested.
- Process kill/reap is real. Container cessation fields passed to the existing
  library are test fixtures; Docker verification, production wrapper and host
  recovery checkpoints are not tested or bypassed in a deployed service.
- The coordinator runs in the test process. Replay and binding rotation are
  invoked explicitly; no P3 import plan, cross-journal recovery transaction,
  schema migration, restart automation or unknown-output recovery is implemented.
- Spexus backend human request/decision authority, JWT permissions and PostgreSQL
  concurrency remain unimplemented and untested by this experiment.

The result supports settling committed output before rotating its authority, with
stop processed first. The next bounded step is durable human create/decision over
real local HTTP/PostgreSQL, including response loss and exact operation replay.
Full P3 still requires its roadmap/backlog, implementation and independent gate.
