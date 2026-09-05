#!/bin/sh
set -eu

production_repo=${PRODUCTION_REPOSITORY_PATH:?PRODUCTION_REPOSITORY_PATH is required}
production_binary=${PRODUCTION_AGENT_LOOP_BINARY:?PRODUCTION_AGENT_LOOP_BINARY is required}
artifact_dir=${HEALTH_ARTIFACT_DIR:?HEALTH_ARTIFACT_DIR is required}
release_tag=${RELEASE_TAG:?RELEASE_TAG is required}
release_commit=${RELEASE_COMMIT:?RELEASE_COMMIT is required}
stable_digest=${STABLE_BINARY_SHA256:?STABLE_BINARY_SHA256 is required}
health_soak_seconds=${HEALTH_SOAK_SECONDS:-300}

[ -x "$production_binary" ]
case "$health_soak_seconds" in
  ''|*[!0-9]*) echo "HEALTH_SOAK_SECONDS must be a non-negative integer" >&2; exit 2 ;;
esac
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-production-health.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM
mkdir -p "$artifact_dir"

capture_sample() {
  sample_name=$1
  sample_offset=$2
  sample_dir="$temporary_root/$sample_name"
  mkdir -p "$sample_dir"

  installed_digest=$(shasum -a 256 "$production_binary" | awk '{print $1}')
  [ "$installed_digest" = "$stable_digest" ]
  "$production_binary" delivery status --json >"$sample_dir/delivery.json"
  "$production_binary" doctor --repo "$production_repo" --json >"$sample_dir/doctor.json"
  "$production_binary" status --repo "$production_repo" --json >"$sample_dir/status.json"
  "$production_binary" version --json >"$sample_dir/version.json"

  jq -n \
    --arg sample "$sample_name" --argjson offset_seconds "$sample_offset" \
    --arg release_tag "$release_tag" --arg release_commit "$release_commit" --arg stable_digest "$installed_digest" \
    --slurpfile delivery "$sample_dir/delivery.json" --slurpfile doctor "$sample_dir/doctor.json" \
    --slurpfile status "$sample_dir/status.json" --slurpfile version "$sample_dir/version.json" '
  {
    sample:$sample,offset_seconds:$offset_seconds,stable_binary_sha256:$stable_digest,
    installed:{version:$version[0].version,commit:$version[0].commit},
    delivery:{phase:$delivery[0].phase,result:$delivery[0].result,current:$delivery[0].current},
    doctor:{schema_version:$doctor[0].schema_version,ok:$doctor[0].ok,
      failed_diagnostic_codes:[$doctor[0].diagnostics[] | select(.ok | not) | .code] | sort},
    status:{state_revision:$status[0].state.state_revision,supervisor_state:$status[0].state.supervisor.state,
      active_workers:$status[0].worker_pool.active,worker_limit:$status[0].worker_pool.limit,
      active_executions:(if $status[0].state.active_execution == null then 0 else 1 end),
      pending_requests:$status[0].pending_requests | length},
    rollback_occurred:($delivery[0].result == "rolled_back" or $delivery[0].result == "rollback_failed"),
    healthy:($version[0].version == $release_tag and $version[0].commit == $release_commit and
      $delivery[0].current.version == $release_tag and $delivery[0].current.commit == $release_commit and
      $delivery[0].phase == "succeeded" and $delivery[0].result == "succeeded" and
      $doctor[0].schema_version == 1 and $doctor[0].ok == true and
      ([$doctor[0].diagnostics[] | select(.ok | not)] | length) == 0 and
      $status[0].worker_pool.limit == 1 and $status[0].worker_pool.active <= 1)
  }
  ' >"$temporary_root/$sample_name.json"

  jq -e '.healthy == true and .rollback_occurred == false' "$temporary_root/$sample_name.json" >/dev/null
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
jq -n \
  --arg release_tag "$release_tag" --arg release_commit "$release_commit" --arg stable_digest "$stable_digest" \
  --argjson duration_seconds "$health_soak_seconds" --argjson middle_offset "$middle_offset" \
  --slurpfile samples "$temporary_root/samples.json" '
  ($samples[0][-1]) as $final |
  {
    schema_version:1, release_tag:$release_tag, release_commit:$release_commit,
    stable_binary_sha256:$stable_digest,
    installed:$final.installed,delivery:$final.delivery,doctor:$final.doctor,status:$final.status,
    rollback_occurred:any($samples[0][]; .rollback_occurred),
    soak:{duration_seconds:$duration_seconds,sample_offsets_seconds:[0,$middle_offset,$duration_seconds],samples:$samples[0]},
    healthy:all($samples[0][]; .healthy)
  }
' >"$artifact_dir/production-health-report.json"

jq -e --arg release_tag "$release_tag" --arg release_commit "$release_commit" '
  .installed.version == $release_tag and .installed.commit == $release_commit and
  .delivery.current.version == $release_tag and .delivery.current.commit == $release_commit and
  .delivery.phase == "succeeded" and .delivery.result == "succeeded" and
  .doctor.schema_version == 1 and .doctor.ok == true and (.doctor.failed_diagnostic_codes | length) == 0 and
  .status.worker_limit == 1 and .status.active_workers <= 1 and
  (.soak.samples | length) == 3 and all(.soak.samples[]; .healthy == true) and
  .rollback_occurred == false and .healthy == true
' "$artifact_dir/production-health-report.json" >/dev/null
