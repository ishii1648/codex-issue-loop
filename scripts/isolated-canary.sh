#!/bin/sh
set -eu

repository=${CANARY_REPOSITORY:?CANARY_REPOSITORY is required}
binary=${CANDIDATE_BINARY:?CANDIDATE_BINARY is required}
artifact_dir=${CANARY_ARTIFACT_DIR:?CANARY_ARTIFACT_DIR is required}
go_toolchain=${GOTOOLCHAIN:-go1.25.13}
case "$repository" in
  */codex-issue-loop-canary) ;;
  *) printf '%s\n' "refusing non-canary repository: $repository" >&2; exit 1 ;;
esac
[ -x "$binary" ]
command -v codex >/dev/null
codex login status >/dev/null

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-canary.XXXXXX")
state_root="$temporary_root/state"
launch_root="$temporary_root/launchagents"
repo_path="$temporary_root/repository"
workspace=${GITHUB_WORKSPACE:-$(git rev-parse --show-toplevel)}
canary_run_id=${GITHUB_RUN_ID:-local-$(date +%s)}
case "$canary_run_id" in
  *[!0-9]*) webhook_port_seed=$$ ;;
  *) webhook_port_seed=$canary_run_id ;;
esac
supervisor_pid=
broker_pid=
cleanup() {
  if [ -n "$supervisor_pid" ] && kill -0 "$supervisor_pid" 2>/dev/null; then
    status=$($binary status --repo "$repo_path" --json 2>/dev/null || printf '{}')
    if [ "$(printf '%s' "$status" | jq -r '.worker_pool.active // 1')" = 0 ]; then
      kill -TERM "$supervisor_pid" 2>/dev/null || true
      wait "$supervisor_pid" 2>/dev/null || true
    fi
  fi
  if [ -n "$broker_pid" ] && kill -0 "$broker_pid" 2>/dev/null; then
    kill -TERM "$broker_pid" 2>/dev/null || true
    wait "$broker_pid" 2>/dev/null || true
  fi
  rm -rf "$temporary_root"
}
trap cleanup EXIT HUP INT TERM

production_before=$(gh api repos/ishii1648/codex-issue-loop --jq '[.id,.open_issues_count,.default_branch] | @tsv')
production_prs_before=$(gh pr list --repo ishii1648/codex-issue-loop --state all --limit 1000 --json number --jq length)
production_branches_before=$(gh api --paginate repos/ishii1648/codex-issue-loop/branches --jq '.[].name' | wc -l | tr -d ' ')

gh repo clone "$repository" "$repo_path" -- --quiet
repo_id=$(gh repo view "$repository" --json databaseId --jq .databaseId)
repository_escaped=$(printf '%s' "$repository" | sed 's/[&/]/\\&/g')
repo_id_escaped=$(printf '%s' "$repo_id" | sed 's/[&/]/\\&/g')
webhook_secret="$temporary_root/canary-webhook-secret"
printf '%s\n' "canary-$canary_run_id" >"$webhook_secret"
chmod 0600 "$webhook_secret"
webhook_port=$((18000 + (webhook_port_seed % 1000)))
sed -e "s#owner/repository#$repository_escaped#" -e "s/# repository_id: 123456789/repository_id: $repo_id_escaped/" \
  "$workspace/.agent-loop.example.yaml" >"$repo_path/.agent-loop.yaml"
sed -i '' 's/auto_merge: false/auto_merge: true/' "$repo_path/.agent-loop.yaml"
sed -i '' 's/^  mode: polling$/  mode: webhook/' "$repo_path/.agent-loop.yaml"
printf '  listener_address: 127.0.0.1:%s\n  public_url_identifier: canary.invalid/github/webhook\n  secret_source:\n    file: %s\n  installation_ids: []\n  allow_repository_webhook: true\n  safety_sweep_interval: 2s\n  safety_sweep_jitter: 0\n' \
  "$webhook_port" "$webhook_secret" >>"$repo_path/.agent-loop.yaml"

export AGENT_LOOP_HOME="$state_root"
export AGENT_LOOP_LAUNCH_AGENTS_DIR="$launch_root"
mkdir -p "$state_root/bin" "$launch_root" "$artifact_dir"
cp "$binary" "$state_root/bin/agent-loop"
chmod 0755 "$state_root/bin/agent-loop"
$binary bootstrap-labels --repo "$repo_path" --apply --json >/dev/null
registration=$($binary register --repo "$repo_path" --json)
runtime_repo_id=$(printf '%s' "$registration" | jq -r '.entry.repo_id')
[ -n "$runtime_repo_id" ]

start_broker() {
  $binary broker >"$temporary_root/broker.log" 2>"$temporary_root/broker.err" &
  broker_pid=$!
  sleep 2
  if ! kill -0 "$broker_pid" 2>/dev/null; then
    tail -100 "$temporary_root/broker.err" >&2
    exit 1
  fi
}

start_supervisor() {
  $binary run --repo "$repo_path" >"$temporary_root/supervisor.log" 2>"$temporary_root/supervisor.err" &
  supervisor_pid=$!
  sleep 2
  kill -0 "$supervisor_pid"
}

stop_idle_supervisor() {
  deadline=$(( $(date +%s) + 300 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    status=$($binary status --repo "$repo_path" --json)
    if [ "$(printf '%s' "$status" | jq -r '.worker_pool.active')" = 0 ]; then
      kill -TERM "$supervisor_pid"
      wait "$supervisor_pid"
      supervisor_pid=
      return
    fi
    sleep 5
  done
  printf '%s\n' "supervisor did not reach an idle durable checkpoint" >&2
  exit 1
}

wait_issue_status() {
  issue_number=$1
  wanted=$2
  deadline=$(( $(date +%s) + 1200 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    status=$($binary status --repo "$repo_path" --json)
    current=$(printf '%s' "$status" | jq -r --arg key "$issue_number" '.state.issues[$key].status // ""')
    if [ "$current" = "$wanted" ]; then return; fi
    if ! kill -0 "$supervisor_pid" 2>/dev/null; then
      tail -100 "$temporary_root/supervisor.err" >&2
      exit 1
    fi
    sleep 10
  done
  printf '%s\n' "Issue #$issue_number did not reach $wanted" >&2
  exit 1
}

sequence_one_url=$(gh issue create --repo "$repository" --title "canary completion $canary_run_id" --body "Create canary-sequence-1.txt containing completion, run git status, and return completed." --label codex-loop:ready)
sequence_one=${sequence_one_url##*/}
start_broker
start_supervisor
wait_issue_status "$sequence_one" completed
stop_idle_supervisor

sequence_two_url=$(gh issue create --repo "$repository" --title "canary needs input $canary_run_id" --body "Before editing, return needs_input asking whether to create canary-sequence-2.txt. After the answer, create it containing resumed and return completed." --label codex-loop:ready)
sequence_two=${sequence_two_url##*/}
start_supervisor
wait_issue_status "$sequence_two" needs_input
stop_idle_supervisor
start_supervisor
request_id=$($binary status --repo "$repo_path" --json | jq -r --argjson issue "$sequence_two" '.pending_requests[] | select(.issue_number == $issue) | .id' | head -1)
[ -n "$request_id" ]
printf '%s\n' yes | $binary answer --repo "$repo_path" --request-id "$request_id" --message-file - --json >/dev/null
wait_issue_status "$sequence_two" completed
stop_idle_supervisor

GOTOOLCHAIN="$go_toolchain" GOCACHE="$temporary_root/go-cache" go test ./internal/application/conformance -run '^TestFaultDurableTransactionFiveCrashBoundaries$' -count=1
transaction_crash_recovery=1
final_status=$($binary status --repo "$repo_path" --json)
active=$(printf '%s' "$final_status" | jq -r '.worker_pool.active')
active_leases=$(printf '%s' "$final_status" | jq '[.state.issues[] | select(.lease != null)] | length')
pending=$(printf '%s' "$final_status" | jq '[.pending_requests[] | select(.status == "pending")] | length')
orphan_processes=$(printf '%s' "$final_status" | jq '[.state.issues[] | select((.worker_pid // 0) != 0 or (.worker_pgid // 0) != 0)] | length')
duplicate_prs=$(gh pr list --repo "$repository" --state all --limit 1000 --json headRefName --jq 'group_by(.headRefName) | map(select(length > 1)) | length')
duplicate_markers=$(gh issue view "$sequence_two" --repo "$repository" --json comments --jq '[.comments[].body | scan("<!-- codex-issue-loop:[^>]+ -->")] | group_by(.) | map(select(length > 1)) | length')
[ "$active" = 0 ] && [ "$active_leases" = 0 ] && [ "$pending" = 0 ] && [ "$orphan_processes" = 0 ] && [ "$duplicate_prs" = 0 ] && [ "$duplicate_markers" = 0 ]
sweep_path="$state_root/repos/$runtime_repo_id/webhook-sweep.json"
broker_status_path="$state_root/broker/status.json"
sweep_rest_200=$(jq -r '.rest_200 // 0' "$sweep_path")
sweep_last_successful=$(jq -r '.last_successful // ""' "$sweep_path")
accepted_webhooks=$(jq -r '.accepted // 0' "$broker_status_path")
[ "$sweep_rest_200" -ge 1 ] && [ -n "$sweep_last_successful" ] && [ "$accepted_webhooks" = 0 ]
stopped_broker_pid=$broker_pid
kill -TERM "$broker_pid"
wait "$broker_pid" || true
broker_pid=
if kill -0 "$stopped_broker_pid" 2>/dev/null; then
  printf '%s\n' "canary webhook broker survived shutdown" >&2
  exit 1
fi

production_after=$(gh api repos/ishii1648/codex-issue-loop --jq '[.id,.open_issues_count,.default_branch] | @tsv')
production_prs_after=$(gh pr list --repo ishii1648/codex-issue-loop --state all --limit 1000 --json number --jq length)
production_branches_after=$(gh api --paginate repos/ishii1648/codex-issue-loop/branches --jq '.[].name' | wc -l | tr -d ' ')
[ "$production_before" = "$production_after" ]
[ "$production_prs_before" = "$production_prs_after" ]
[ "$production_branches_before" = "$production_branches_after" ]

jq -n \
  --arg repository "$repository" --argjson repository_id "$repo_id" \
  --argjson sequence_one "$sequence_one" --argjson sequence_two "$sequence_two" \
  --argjson active "$active" --argjson active_leases "$active_leases" --argjson pending "$pending" \
  --argjson orphan_processes "$orphan_processes" --argjson duplicate_prs "$duplicate_prs" --argjson duplicate_markers "$duplicate_markers" \
  --argjson sweep_rest_200 "$sweep_rest_200" --arg sweep_last_successful "$sweep_last_successful" \
  --argjson accepted_webhooks "$accepted_webhooks" --argjson transaction_crash_recovery "$transaction_crash_recovery" \
  '{schema_version:1, repository:$repository, repository_id:$repository_id, sequences:[{id:"claim-worker-publication-checks-terminal",issue:$sequence_one,status:"completed"},{id:"needs-input-answer-resume-terminal",issue:$sequence_two,status:"completed"}],supervisor_restarts:2,webhook_miss_polling_fallback:1,webhook_evidence:{accepted_deliveries:$accepted_webhooks,safety_sweep_rest_200:$sweep_rest_200,last_successful_safety_sweep:$sweep_last_successful},transaction_crash_recovery:$transaction_crash_recovery,production_remote_changes:0,production_state_accessed:false,final:{active_workers:$active,active_leases:$active_leases,pending_requests:$pending,orphan_pid_pgid:$orphan_processes,duplicate_prs:$duplicate_prs,duplicate_comment_markers:$duplicate_markers,broker_processes:0}}' \
  >"$artifact_dir/canary-report.json"
