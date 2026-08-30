#!/bin/sh
set -eu

production_repo=${PRODUCTION_REPOSITORY_PATH:?PRODUCTION_REPOSITORY_PATH is required}
production_binary=${PRODUCTION_AGENT_LOOP_BINARY:?PRODUCTION_AGENT_LOOP_BINARY is required}
artifact_dir=${HEALTH_ARTIFACT_DIR:?HEALTH_ARTIFACT_DIR is required}
release_tag=${RELEASE_TAG:?RELEASE_TAG is required}
release_commit=${RELEASE_COMMIT:?RELEASE_COMMIT is required}
stable_digest=${STABLE_BINARY_SHA256:?STABLE_BINARY_SHA256 is required}

[ -x "$production_binary" ]
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-production-health.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM
mkdir -p "$artifact_dir"

installed_digest=$(shasum -a 256 "$production_binary" | awk '{print $1}')
[ "$installed_digest" = "$stable_digest" ]

"$production_binary" delivery status --json >"$temporary_root/delivery.json"
"$production_binary" doctor --repo "$production_repo" --json >"$temporary_root/doctor.json"
"$production_binary" status --repo "$production_repo" --json >"$temporary_root/status.json"
"$production_binary" version --json >"$temporary_root/version.json"

jq -n \
  --arg release_tag "$release_tag" --arg release_commit "$release_commit" --arg stable_digest "$installed_digest" \
  --slurpfile delivery "$temporary_root/delivery.json" --slurpfile doctor "$temporary_root/doctor.json" \
  --slurpfile status "$temporary_root/status.json" --slurpfile version "$temporary_root/version.json" '
  {
    schema_version:1, release_tag:$release_tag, release_commit:$release_commit,
    stable_binary_sha256:$stable_digest,
    installed:{version:$version[0].version,commit:$version[0].commit},
    delivery:{phase:$delivery[0].phase,result:$delivery[0].result,current:$delivery[0].current},
    doctor:{schema_version:$doctor[0].schema_version,ok:$doctor[0].ok,
      failed_diagnostic_codes:[$doctor[0].diagnostics[] | select(.ok | not) | .code] | sort},
    status:{state_revision:$status[0].state.state_revision,supervisor_state:$status[0].state.supervisor.state,
      active_workers:$status[0].worker_pool.active,worker_limit:$status[0].worker_pool.limit,
      active_leases:[$status[0].state.issues[] | select(.lease != null)] | length,
      pending_requests:$status[0].pending_requests | length},
    rollback_occurred:($delivery[0].result == "rolled_back" or $delivery[0].result == "rollback_failed"),
    healthy:($version[0].version == $release_tag and $version[0].commit == $release_commit and
      $delivery[0].current.version == $release_tag and $delivery[0].current.commit == $release_commit and
      $delivery[0].phase == "succeeded" and $delivery[0].result == "succeeded" and
      $doctor[0].schema_version == 1 and $doctor[0].ok == true and
      ([$doctor[0].diagnostics[] | select(.ok | not)] | length) == 0 and
      $status[0].worker_pool.limit == 1 and $status[0].worker_pool.active <= 1)
  }
' >"$artifact_dir/production-health-report.json"

jq -e --arg release_tag "$release_tag" --arg release_commit "$release_commit" '
  .installed.version == $release_tag and .installed.commit == $release_commit and
  .delivery.current.version == $release_tag and .delivery.current.commit == $release_commit and
  .delivery.phase == "succeeded" and .delivery.result == "succeeded" and
  .doctor.schema_version == 1 and .doctor.ok == true and (.doctor.failed_diagnostic_codes | length) == 0 and
  .status.worker_limit == 1 and .status.active_workers <= 1 and
  .rollback_occurred == false and .healthy == true
' "$artifact_dir/production-health-report.json" >/dev/null
