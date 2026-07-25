#!/usr/bin/env bash
# Verifies the Spexus Agent compatibility boundary for SP-EP-016.
#
# The agent authenticates only to Slack and invokes ACPX as a local subprocess.
# It must not acquire a direct dependency on legacy Spexus account roles or on
# removed /auth/users mutations. This command is deliberately offline: focused
# tests use their existing fake Slack/ACPX transports and never contact a shared
# backend, production system, or production backup.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "${script_dir}/.." && pwd)"
cd "${repo_dir}"

legacy_authority_pattern='User\.Role|RequireAdministrator|RequireUser|RequireCommenter|RequireRole|/auth/users|auth/users|/api/v1/users'
if rg -n -e "${legacy_authority_pattern}" cmd internal --glob '*.go'; then
  echo 'SP-EP-016 compatibility failure: agent source depends on a removed legacy account-role or /auth/users contract.' >&2
  exit 1
fi

# Direct HTTP is an intentional Slack transport concern only. A result outside
# internal/slack would require a fresh compatibility assessment before cutover.
unexpected_http_files="$(rg -l -e 'net/http|http\.NewRequest|http\.Client' cmd internal --glob '*.go' | rg -v '^internal/slack/' || true)"
if [[ -n "${unexpected_http_files}" ]]; then
  printf '%s\n' "${unexpected_http_files}" >&2
  echo 'SP-EP-016 compatibility failure: a non-Slack direct HTTP client was introduced.' >&2
  exit 1
fi

compat_go_cache="${SPEXUS_AGENT_EP016_GOCACHE:-/private/tmp/spexus-agent-sp-ep-016-go-build}"
mkdir -p "${compat_go_cache}"
GOCACHE="${compat_go_cache}" go test ./internal/slack ./internal/acpxadapter ./internal/runtime ./internal/cli -count=1
git diff --check

echo 'SP-EP-016 agent compatibility: PASS (Slack-only auth transport; no direct Spexus RBAC or legacy /auth/users dependency).'
