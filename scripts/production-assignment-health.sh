#!/bin/sh
set -eu

operator_binary=${PRODUCTION_AGENT_LOOP_BINARY:?PRODUCTION_AGENT_LOOP_BINARY is required}
repositories_file=${PRODUCTION_REPOSITORIES_FILE:?PRODUCTION_REPOSITORIES_FILE is required}
rollback_drill_file=${ROLLBACK_DRILL_FILE:?ROLLBACK_DRILL_FILE is required}
artifact_dir=${HEALTH_ARTIFACT_DIR:?HEALTH_ARTIFACT_DIR is required}
release_tag=${RELEASE_TAG:?RELEASE_TAG is required}
release_commit=${RELEASE_COMMIT:?RELEASE_COMMIT is required}
stable_digest=${STABLE_BINARY_SHA256:?STABLE_BINARY_SHA256 is required}
health_soak_seconds=${HEALTH_SOAK_SECONDS:-300}

[ -x "$operator_binary" ]
[ -f "$repositories_file" ]
[ -f "$rollback_drill_file" ]
case "$health_soak_seconds" in
  ''|*[!0-9]*) echo "HEALTH_SOAK_SECONDS must be a non-negative integer" >&2; exit 2 ;;
esac
jq -e 'type == "array" and length >= 1 and all(.[]; (.repo_id | type == "string" and length > 0) and (.path | type == "string" and startswith("/")))' "$repositories_file" >/dev/null
jq -e --arg tag "$release_tag" '
  .schema_version == 1 and .typed_rollback == true and .same_artifact_reapplied == true and
  .before.version == $tag and .rollback.result == "succeeded" and .reapplied.version == $tag and
  .preserved.state == true and .preserved.issues == true and .preserved.leases == true and
  .preserved.worktrees == true and
  .other_repository_unchanged.assignment == true and .other_repository_unchanged.pid == true and
  .other_repository_unchanged.binary == true and .other_repository_unchanged.state_revision == true
' "$rollback_drill_file" >/dev/null

actual_operator_digest=$(shasum -a 256 "$operator_binary" | awk '{print $1}')
[ "$actual_operator_digest" = "$stable_digest" ]
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-assignment-health.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM
mkdir -p "$artifact_dir"

capture_sample() {
  sample_name=$1
  sample_offset=$2
  sample_dir="$temporary_root/$sample_name"
  mkdir -p "$sample_dir/repos"
  : >"$sample_dir/repositories.jsonl"

  jq -r '.[] | [.repo_id,.path] | @tsv' "$repositories_file" |
  while IFS="	" read -r repo_id repo_path; do
    safe_id=$(printf '%s' "$repo_id" | tr -c 'A-Za-z0-9._-' '_')
    "$operator_binary" delivery assignment status --repo "$repo_path" --json >"$sample_dir/repos/$safe_id-assignment.json"
    "$operator_binary" delivery assignment verify --repo "$repo_path" --json >"$sample_dir/repos/$safe_id-verify.json"
    "$operator_binary" doctor --repo "$repo_path" --assignment-health --json >"$sample_dir/repos/$safe_id-doctor.json"
    "$operator_binary" status --repo "$repo_path" --json >"$sample_dir/repos/$safe_id-status.json"
    jq -n --arg repo_id "$repo_id" --arg tag "$release_tag" --arg commit "$release_commit" --arg digest "$stable_digest" \
      --slurpfile assignment "$sample_dir/repos/$safe_id-assignment.json" \
      --slurpfile verify "$sample_dir/repos/$safe_id-verify.json" \
      --slurpfile doctor "$sample_dir/repos/$safe_id-doctor.json" \
      --slurpfile status "$sample_dir/repos/$safe_id-status.json" '
      ($assignment[0].assignments[0]) as $a |
      {
        repository_id:$repo_id,
        assignment:{version:$a.assignment.version,commit:$a.assignment.commit,
          artifact_sha256:$a.assignment.artifact_sha256,generation:$a.assignment.generation,
          previous_version:($a.assignment.previous.version // null)},
        runtime:{digest:$a.runtime.digest,matches:$a.runtime.matches,loaded:$a.runtime.launchd.loaded,
          running:$a.runtime.launchd.running,pid:($a.runtime.launchd.pid // 0)},
        transaction_phase:($a.transaction.phase // null),fence_active:$a.fence_active,
        doctor:{schema_version:$doctor[0].schema_version,ok:$doctor[0].ok,
          failed_diagnostic_codes:[$doctor[0].diagnostics[] | select(.ok | not) | .code] | sort},
        status:{state_revision:$status[0].state.state_revision,supervisor_state:$status[0].state.supervisor.state,
          issue_count:($status[0].state.issues | length),active_workers:$status[0].worker_pool.active,
          worker_limit:$status[0].worker_pool.limit,active_leases:([$status[0].state.issues[] | select(.execution_lease != null)] | length),
          pending_requests:($status[0].pending_requests | length)},
        verified:$verify[0].verified,
        healthy:($a.assignment.version == $tag and $a.assignment.commit == $commit and
          $a.assignment.artifact_sha256 == $digest and $a.runtime.digest == $digest and
          $a.runtime.matches == true and $a.runtime.launchd.loaded == true and $a.runtime.launchd.running == true and
          $a.fence_active == false and (($a.transaction.phase // "succeeded") == "succeeded") and
          $verify[0].verified == true and $doctor[0].schema_version == 1 and $doctor[0].ok == true and
          ([$doctor[0].diagnostics[] | select(.ok | not)] | length) == 0 and
          $status[0].worker_pool.limit == 1 and $status[0].worker_pool.active <= 1)
      }
    ' >>"$sample_dir/repositories.jsonl"
  done

  jq -s --arg sample "$sample_name" --argjson offset "$sample_offset" \
    '{sample:$sample,offset_seconds:$offset,repositories:.,healthy:all(.[];.healthy == true)}' \
    "$sample_dir/repositories.jsonl" >"$temporary_root/$sample_name.json"
  jq -e '.healthy == true' "$temporary_root/$sample_name.json" >/dev/null
}

middle_offset=60
if [ "$health_soak_seconds" -lt "$middle_offset" ]; then
  middle_offset=$((health_soak_seconds / 2))
fi
final_delay=$((health_soak_seconds - middle_offset))

capture_sample start 0
if [ "$middle_offset" -gt 0 ]; then sleep "$middle_offset"; fi
capture_sample intermediate "$middle_offset"
if [ "$final_delay" -gt 0 ]; then sleep "$final_delay"; fi
capture_sample final "$health_soak_seconds"

jq -s '.' "$temporary_root/start.json" "$temporary_root/intermediate.json" "$temporary_root/final.json" >"$temporary_root/samples.json"
jq -n --arg tag "$release_tag" --arg commit "$release_commit" --arg digest "$stable_digest" \
  --argjson duration "$health_soak_seconds" --argjson middle "$middle_offset" \
  --slurpfile samples "$temporary_root/samples.json" --slurpfile rollback "$rollback_drill_file" '
  {
    schema_version:2,release_tag:$tag,release_commit:$commit,stable_binary_sha256:$digest,
    rollout_mode:"per-repository-stable-assignment",rollback_drill:$rollback[0],
    soak:{duration_seconds:$duration,sample_offsets_seconds:[0,$middle,$duration],samples:$samples[0]},
    repositories:$samples[0][-1].repositories,
    healthy:all($samples[0][];.healthy == true)
  }
' >"$artifact_dir/production-health-report.json"

jq -e --arg tag "$release_tag" --arg commit "$release_commit" --arg digest "$stable_digest" '
  .schema_version == 2 and .release_tag == $tag and .release_commit == $commit and
  .stable_binary_sha256 == $digest and .rollout_mode == "per-repository-stable-assignment" and
  .rollback_drill.typed_rollback == true and .rollback_drill.same_artifact_reapplied == true and
  (.soak.samples | length) == 3 and all(.soak.samples[];.healthy == true) and
  (.repositories | length) >= 1 and all(.repositories[];
    .assignment.version == $tag and .assignment.commit == $commit and .assignment.artifact_sha256 == $digest and
    .runtime.digest == $digest and .runtime.matches == true and .fence_active == false and
    .transaction_phase == "succeeded" and .doctor.ok == true and .status.worker_limit == 1 and .healthy == true) and
  .healthy == true
' "$artifact_dir/production-health-report.json" >/dev/null
