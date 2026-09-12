#!/bin/bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?}"
: "${RELEASE_COMMIT:?}"
[[ "$RELEASE_COMMIT" =~ ^[0-9a-f]{40}$ ]]
api="repos/$GITHUB_REPOSITORY/actions"
workflow=$(gh api "$api/workflows/ci.yml")
workflow_id=$(jq -er 'select(.path == ".github/workflows/ci.yml" and .state == "active") | .id | numbers | select(. > 0)' <<<"$workflow")

# Refuse truncated results instead of selecting an older successful run.
runs=$(gh api "$api/workflows/$workflow_id/runs?branch=main&event=push&head_sha=$RELEASE_COMMIT&per_page=100")
run=$(jq -ec --arg repo "$GITHUB_REPOSITORY" --arg sha "$RELEASE_COMMIT" --argjson workflow "$workflow_id" '
  select(.total_count > 0 and .total_count <= 100 and .total_count == (.workflow_runs | length)) |
  .workflow_runs |
  select(all(.[]; .repository.full_name == $repo and .head_repository.full_name == $repo and
    .head_sha == $sha and .head_branch == "main" and .event == "push" and
    .workflow_id == $workflow and .path == ".github/workflows/ci.yml" and (.id | type) == "number")) |
  max_by(.id) |
  select(.status == "completed" and .conclusion == "success" and .run_attempt > 0)
' <<<"$runs")
run_id=$(jq -r '.id' <<<"$run")
attempt=$(jq -r '.run_attempt' <<<"$run")
printf 'Checking main CI run %s attempt %s for %s\n' "$run_id" "$attempt" "$RELEASE_COMMIT"

jobs=$(gh api "$api/runs/$run_id/attempts/$attempt/jobs?per_page=100")
jq -e --arg sha "$RELEASE_COMMIT" --argjson run "$run_id" --argjson attempt "$attempt" '
  select(.total_count > 0 and .total_count <= 100 and .total_count == (.jobs | length)) |
  .jobs |
  select(all(.[]; .run_id == $run and .run_attempt == $attempt and .head_sha == $sha and
    .status == "completed" and .conclusion == "success")) |
  map(select(.name == "Quality gates")) | length == 1
' <<<"$jobs" >/dev/null

# A rerun started during verification invalidates the observed attempt.
current=$(gh api "$api/runs/$run_id")
jq -e --argjson expected "$run" '
  .id == $expected.id and .run_attempt == $expected.run_attempt and
  .workflow_id == $expected.workflow_id and .path == $expected.path and
  .repository.full_name == $expected.repository.full_name and
  .head_repository.full_name == $expected.head_repository.full_name and
  .head_sha == $expected.head_sha and .head_branch == "main" and .event == "push" and
  .status == "completed" and .conclusion == "success"
' <<<"$current" >/dev/null
printf 'Verified https://github.com/%s/actions/runs/%s/attempts/%s commit %s\n' \
  "$GITHUB_REPOSITORY" "$run_id" "$attempt" "$RELEASE_COMMIT" | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"
