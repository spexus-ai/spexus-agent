# Workspace handover experiment

This opt-in experiment exercises the wire-v2 three-agent protocol with real Pi
sessions: orchestrator → coder → reviewer. It uses a disposable JavaScript
checkout and `openai-codex/gpt-6-luna` with `low` reasoning. The orchestrator
dispatches the coding task, accepts its result, and dispatches a separate review
task. A random `Checkpoint` constant proves that the reviewer read the changed
file rather than merely repeating the coder's summary.

Run the container variant with a locally built image and working Pi credentials:

```sh
docker build -f Dockerfile.swarm -t spexus-swarm:handover-local .
SPEXUS_TEST_HANDOVER=1 \
SPEXUS_TEST_HANDOVER_CONTAINER_IMAGE=spexus-swarm:handover-local \
go test -tags handoverlive ./internal/swarmrunner \
  -run '^TestRealPiWorkspaceHandover$' -count=1 -timeout=8m -v
```

The test copies only the Pi authentication files needed by each runner into
separate temporary directories. It starts the orchestrator and coder containers
with a prepared workspace mounted writable for the coder. After the coder's
result is accepted, it stops that container before starting the reviewer with
the same workspace mounted read-only. It checks Docker's mount mode, rejects an
actual write attempt from inside the reviewer container, checks the reviewer's
reported checkpoint, and checks that each job had one attempt. Cleanup removes
the containers and temporary data even if the test fails. No Slack message or
real repository is involved. The fixture establishes an empty Slack catchup
watermark before releasing wire-v2's startup barrier.

A repeated live run exposed an action-free final answer from the orchestrator
while the reviewer's successful result was still pending review. The runner now
requires an explicit review action for such a result, and the wire-v2
coordinator suppresses an action-free task-result reply unless that exact result
was already accepted in a prior turn. Regression tests cover both the premature
reply and the legitimate summary-only reply after acceptance.

The wire-v2 container run on 2026-09-24 passed in 47.89 seconds using local
image `sha256:7bb4fcb99a2e99d4fa97d0653eb6dcc8c862267f06925febafcdaa92f0f18c1a`.
The coder job was `a3cfa099-e292-455d-ba68-ed7f500ee681`; the reviewer job was
`686ee696-ab0a-4a8b-83bd-0d160c4f4671`. The reviewer reported the exact
checkpoint `checkpoint-b5c161a9-a5cf-444f-b2b1-d8283e0ce8d9`; each job had
one attempt, both reviews were accepted, and the final reply was queued.

This proves a sequential handover for one prepared workspace. It does not yet
implement workspace creation, per-task lease allocation, crash reconciliation,
or automatic container lifecycle management in the product runtime. The test
driver performs those steps. Pi credentials are available inside each test
container so a coding worker with `bash` can potentially read them; this setup
must not be promoted as a general untrusted-code sandbox without separating
model credentials from the tool execution environment.
