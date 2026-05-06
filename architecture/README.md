# Architecture Pipeline Output

This directory contains the output of the extended `PROMPT-020` workflow executed against the current repository as a single-service system: `product-requirements-agent`.

## Scope

- Evidence source policy for this rerun: only live repository source/config/build files were used. Existing `docs/` and earlier `architecture/` outputs were intentionally excluded as evidence.
- `C0`: overall landscape around operators, Slack users, Slack Platform, ACPX/Codex CLI, local repositories, config, and SQLite.
- `C1`: system context of the local Spexus Agent runtime.
- `C2`: service/container topology for the current repository.
- `C3`: component decomposition for the current implementation.

## Stage Map

- `00-bootstrap`: input service and environment bootstrap
- `00-inventory`: repo scan and evidence inventory
- `10-classification`: normalized service/domain/auth/storage classification
- `15-components`: C3 component extraction
- `20-relations`: confirmed external relations and data flows
- `30-scenarios`: service-level scenarios
- `35-component-scenarios`: component flow paths
- `40-deployment`: local deployment/runtime variants
- `50-likec4`: validated LikeC4 inputs, models, and views
- `90-validation`: completeness, traceability, anomalies, and verdict

## Current Summary

- Input services: 1
- Confirmed services: 1
- Confirmed C3 components: 12
- Confirmed external relations: 4
- Cross-repo relations: 0
- Pipeline verdict: `SUCCESS`

## Incremental Update Guidance

If only one area changes:

- `internal/config` or `internal/cli/config_handler.go`: update `10-classification/auth-surface-map.json`, `15-components/component-catalog.json`, and component views.
- `internal/cli/project_handler.go` or `internal/registry`: update project-import scenarios, component graph, and relations to Slack/channel provisioning and `git` subprocess usage.
- `internal/cli/runtime_handler.go` or `internal/runtime/*`: update component catalog, dependency matrix, dynamic views, and runtime scenarios first.
- `internal/slack/*`: update relations, auth surface, and transport-related components.
- `internal/acpxadapter/*`: update external dependency notes and runtime data flows.
- `internal/storage/*`: update ownership, database schema, and deployment/runtime persistence notes.

## Validation Notes

JSON artifacts were generated in a reproducible shell run.

Validated in this run:

- `codesearch` indexed the repository successfully using workspace-local db/config state plus a repo-local Hugging Face cache.
- LikeC4 CLI validated all generated model and view files successfully.
