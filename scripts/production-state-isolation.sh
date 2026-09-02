#!/bin/sh
set -eu

production_repo=${PRODUCTION_REPOSITORY_PATH:?PRODUCTION_REPOSITORY_PATH is required}
production_binary=${PRODUCTION_AGENT_LOOP_BINARY:?PRODUCTION_AGENT_LOOP_BINARY is required}
candidate_binary=${CANDIDATE_BINARY:?CANDIDATE_BINARY is required}
artifact_dir=${CONTRACT_ARTIFACT_DIR:?CONTRACT_ARTIFACT_DIR is required}
release_tag=${RELEASE_TAG:?RELEASE_TAG is required}
release_commit=${RELEASE_COMMIT:?RELEASE_COMMIT is required}
candidate_tag=${CANDIDATE_TAG:?CANDIDATE_TAG is required}

case "$release_commit" in
  *[!0-9a-f]*|'') printf '%s\n' "RELEASE_COMMIT must be lowercase hexadecimal" >&2; exit 1 ;;
esac
[ "${#release_commit}" -eq 40 ]
[ -d "$production_repo/.git" ] || [ -f "$production_repo/.git" ]
[ -x "$production_binary" ]
[ -x "$candidate_binary" ]

repo_root=$(git rev-parse --show-toplevel)
offline_contract=${OFFLINE_CONTRACT_SCRIPT:-$repo_root/scripts/offline-release-contract.sh}
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-production-isolation.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM
offline_home="$temporary_root/offline-home"
mkdir -p "$artifact_dir" "$offline_home"
[ -x "$offline_contract" ]

snapshot_production() {
  destination=$1
  doctor_path="$temporary_root/doctor.json"
  status_path="$temporary_root/status.json"
  if ! "$production_binary" doctor --repo "$production_repo" --json >"$doctor_path"; then
    : # A deliberately stopped supervisor is accepted below only when it is the sole diagnostic failure.
  fi
  "$production_binary" status --repo "$production_repo" --json >"$status_path"
  jq -n --slurpfile doctor "$doctor_path" --slurpfile status "$status_path" '
    {
      schema_version: 1,
      repo_id: $status[0].state.repo_id,
      state_revision: $status[0].state.state_revision,
      issue_count: ($status[0].state.issues | length),
      leases: [
        $status[0].state.issues | to_entries[] |
        select(.value.lease != null) |
        {issue_number:(.key | tonumber), owner:.value.lease.owner, slot:.value.lease.slot,
         resources:.value.lease.resolved_resources, base_sha:.value.lease.base_sha}
      ] | sort_by(.issue_number),
      pending_request_count: ($status[0].pending_requests | length),
      active_workers: $status[0].worker_pool.active,
      worker_limit: $status[0].worker_pool.limit,
      supervisor_state: $status[0].state.supervisor.state,
      doctor_schema_version: $doctor[0].schema_version,
      doctor_ok: $doctor[0].ok,
      failed_diagnostic_codes: [$doctor[0].diagnostics[] | select(.ok | not) | .code] | sort
    }
    | .doctor_safe = (.doctor_ok or (.supervisor_state == "stopped" and .active_workers == 0 and .failed_diagnostic_codes == ["SUPERVISOR_STOPPED"]))
  ' >"$destination"
  jq -e '.doctor_schema_version == 1 and .doctor_safe == true and .worker_limit == 1 and .active_workers <= 1' "$destination" >/dev/null
}

snapshot_production "$temporary_root/production-before.json"
HOME="$offline_home" \
  CANDIDATE_BINARY="$candidate_binary" \
  CONTRACT_ARTIFACT_DIR="$temporary_root/offline-contract" \
  "$offline_contract"
snapshot_production "$temporary_root/production-after.json"
cmp "$temporary_root/production-before.json" "$temporary_root/production-after.json"

candidate_digest=$(shasum -a 256 "$candidate_binary" | awk '{print $1}')
jq -e '
  .mode == "credentialless-offline" and
  .credentials.canary_github_token == false and .credentials.openai_api_key == false and
  .external_network == false and
  .final.active_workers == 0 and .final.active_leases == 0 and .final.pending_requests == 0 and
  .final.orphan_pid_pgid == 0 and .final.duplicate_prs == 0 and .final.duplicate_comment_markers == 0 and
  (.sequences | length) == 2 and .supervisor_starts >= 2 and
  .webhook_fixture_replay == 1 and .transaction_crash_recovery == 1
' "$temporary_root/offline-contract/offline-contract-report.json" >/dev/null

jq -n \
  --arg release_tag "$release_tag" --arg release_commit "$release_commit" \
  --arg candidate_tag "$candidate_tag" --arg candidate_digest "$candidate_digest" \
  --slurpfile production_before "$temporary_root/production-before.json" \
  --slurpfile production_after "$temporary_root/production-after.json" \
  --slurpfile offline_contract "$temporary_root/offline-contract/offline-contract-report.json" \
  '{schema_version:1,release_tag:$release_tag,release_commit:$release_commit,candidate_tag:$candidate_tag,
    candidate_binary_sha256:$candidate_digest,production_state_accessed:true,
    production_before:$production_before[0],production_after:$production_after[0],
    production_state_changes:0,offline_contract:$offline_contract[0]}' \
  >"$artifact_dir/production-state-report.json"

jq -e '
  .production_state_accessed == true and .production_state_changes == 0 and
  .production_before == .production_after and .production_before.worker_limit == 1 and
  .production_before.active_workers <= 1 and .production_before.doctor_safe == true
' "$artifact_dir/production-state-report.json" >/dev/null
