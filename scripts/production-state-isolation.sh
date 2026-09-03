#!/bin/sh
set -eu
umask 077

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
host_gomodcache=$(go env GOMODCACHE)
host_gocache=$(go env GOCACHE)
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-production-isolation.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM
offline_home="$temporary_root/offline-home"
mkdir -p "$artifact_dir" "$offline_home"
[ -x "$offline_contract" ]

snapshot_production() {
  destination=$1
  doctor_path="$temporary_root/doctor.json"
  status_path="$temporary_root/status.json"
  if ! "$production_binary" doctor --repo "$production_repo" --assignment-health --json >"$doctor_path"; then
    : # A deliberately stopped supervisor is accepted below only when it is the sole diagnostic failure.
  fi
  "$production_binary" status --repo "$production_repo" --json >"$status_path"
  jq -n --slurpfile doctor "$doctor_path" --slurpfile status "$status_path" '
    {
      schema_version: 1,
      repo_id: $status[0].state.repo_id,
      state_revision: $status[0].state.state_revision,
      issue_count: ($status[0].state.issues | length),
      active_execution: ($status[0].state.active_execution //
        (if $status[0].worker_pool.active == 1 then $status[0].worker_pool.issues[0] else null end)),
      pending_request_count: ($status[0].pending_requests | length),
      active_workers: $status[0].worker_pool.active,
      worker_limit: $status[0].worker_pool.limit,
      supervisor_state: $status[0].state.supervisor.state,
      doctor_schema_version: $doctor[0].schema_version,
      doctor_ok: $doctor[0].ok,
      failed_diagnostic_codes: [$doctor[0].diagnostics[] | select(.ok | not) | .code] | sort
    }
    | .doctor_safe = (.doctor_ok or (.supervisor_state == "stopped" and .active_workers == 0 and
        .active_execution == null and .failed_diagnostic_codes == ["SUPERVISOR_STOPPED"]))
  ' >"$destination"
  jq -e '.doctor_schema_version == 1 and .doctor_safe == true and .worker_limit == 1 and .active_workers <= 1' "$destination" >/dev/null
}

snapshot_production "$temporary_root/production-before.json"
HOME="$offline_home" \
  GOMODCACHE="$host_gomodcache" \
  GOCACHE="$host_gocache" \
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
  .final.active_workers == 0 and .final.active_executions == 0 and .final.pending_requests == 0 and
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
  >"$artifact_dir/production-state-private-evidence.json"
chmod 0600 "$artifact_dir/production-state-private-evidence.json"

private_evidence_digest=$(shasum -a 256 "$artifact_dir/production-state-private-evidence.json" | awk '{print $1}')

jq -n \
  --arg release_tag "$release_tag" --arg release_commit "$release_commit" \
  --arg candidate_tag "$candidate_tag" --arg candidate_digest "$candidate_digest" \
  --arg private_evidence_digest "$private_evidence_digest" \
  --slurpfile production_before "$temporary_root/production-before.json" \
  --slurpfile production_after "$temporary_root/production-after.json" \
  --slurpfile offline_contract "$temporary_root/offline-contract/offline-contract-report.json" \
  '{schema_version:2,public_payload:"redacted-summary",release_tag:$release_tag,
    release_commit:$release_commit,candidate_tag:$candidate_tag,
    candidate_binary_sha256:$candidate_digest,production_state_accessed:true,
    production_state_changes:0,production_state_equal:($production_before[0] == $production_after[0]),
    production_health:{doctor_safe:$production_before[0].doctor_safe,
      worker_limit_enforced:($production_before[0].worker_limit == 1),
      active_workers_within_limit:($production_before[0].active_workers <= 1)},
    offline_contract:{mode:$offline_contract[0].mode,
      credentials_absent:(($offline_contract[0].credentials.canary_github_token | not) and
        ($offline_contract[0].credentials.openai_api_key | not)),
      external_network:$offline_contract[0].external_network,
      lifecycle_sequences_complete:(($offline_contract[0].sequences | length) == 2),
      final_resources_clean:($offline_contract[0].final.active_workers == 0 and
        $offline_contract[0].final.active_executions == 0 and
        $offline_contract[0].final.pending_requests == 0 and
        $offline_contract[0].final.orphan_pid_pgid == 0 and
        $offline_contract[0].final.duplicate_prs == 0 and
        $offline_contract[0].final.duplicate_comment_markers == 0)},
    private_evidence_sha256:$private_evidence_digest}' \
  >"$artifact_dir/production-state-report.json"

jq -e '
  def exact_keys($expected): keys == ($expected | sort);
  exact_keys(["schema_version","public_payload","release_tag","release_commit","candidate_tag",
    "candidate_binary_sha256","production_state_accessed","production_state_changes",
    "production_state_equal","production_health","offline_contract","private_evidence_sha256"]) and
  (.production_health | exact_keys(["doctor_safe","worker_limit_enforced","active_workers_within_limit"])) and
  (.offline_contract | exact_keys(["mode","credentials_absent","external_network",
    "lifecycle_sequences_complete","final_resources_clean"])) and
  .schema_version == 2 and .public_payload == "redacted-summary" and
  .production_state_accessed == true and .production_state_changes == 0 and
  .production_state_equal == true and .production_health.doctor_safe == true and
  .production_health.worker_limit_enforced == true and
  .production_health.active_workers_within_limit == true and
  .offline_contract.mode == "credentialless-offline" and
  .offline_contract.credentials_absent == true and .offline_contract.external_network == false and
  .offline_contract.lifecycle_sequences_complete == true and
  .offline_contract.final_resources_clean == true and
  (.private_evidence_sha256 | test("^[0-9a-f]{64}$"))
' "$artifact_dir/production-state-report.json" >/dev/null
