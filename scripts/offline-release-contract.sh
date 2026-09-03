#!/bin/sh

set -eu
if [ "${OFFLINE_CONTRACT_DEBUG:-false}" = true ]; then
  set -x
fi

binary=${CANDIDATE_BINARY:?CANDIDATE_BINARY is required}
artifact_dir=${CONTRACT_ARTIFACT_DIR:?CONTRACT_ARTIFACT_DIR is required}
workspace=${GITHUB_WORKSPACE:-$(git rev-parse --show-toplevel)}
go_toolchain=${GOTOOLCHAIN:-go1.25.13}
operator_home=${HOME:?HOME is required}
[ -x "$binary" ]
[ -z "${CANARY_GITHUB_TOKEN:-}" ]
[ -z "${OPENAI_API_KEY:-}" ]

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-offline-contract.XXXXXX")
supervisor_pid=
supervisor_starts=0
cleanup() {
  if [ -n "$supervisor_pid" ] && kill -0 "$supervisor_pid" 2>/dev/null; then
    kill -TERM "$supervisor_pid" 2>/dev/null || true
    wait "$supervisor_pid" 2>/dev/null || true
  fi
  if [ "${OFFLINE_CONTRACT_KEEP_TMP:-false}" = true ]; then
    printf '%s\n' "offline contract retained at $temporary_root" >&2
  else
    rm -rf "$temporary_root"
  fi
}
trap cleanup EXIT HUP INT TERM

repo_path="$temporary_root/repository"
remote_path="$temporary_root/origin.git"
state_root="$temporary_root/agent-loop-home"
stub_state="$temporary_root/stub-state"
stub_bin="$temporary_root/bin"
launch_root="$temporary_root/launchagents"
mkdir -p "$repo_path" "$state_root/bin" "$stub_state" "$stub_bin" "$launch_root" "$artifact_dir" "$temporary_root/home"

GOTOOLCHAIN=${GOTOOLCHAIN:-go1.25.13} go build -trimpath -o "$stub_bin/offline-contract-stub" ./cmd/offline-contract-stub
ln -s offline-contract-stub "$stub_bin/gh"
ln -s offline-contract-stub "$stub_bin/codex"

git init --bare -q "$remote_path"
git -C "$repo_path" init -q
git -C "$repo_path" config user.name offline-contract
git -C "$repo_path" config user.email offline-contract@example.invalid
git -C "$repo_path" config commit.gpgsign false
git -C "$repo_path" remote add origin "$remote_path"

sed -e 's#owner/repository#offline/repository#' "$workspace/.agent-loop.example.yaml" >"$repo_path/.agent-loop.yaml"
sed -i '' '/^  concurrency: 1$/a\
  poll_interval: 100ms
' "$repo_path/.agent-loop.yaml"
sed -i '' 's/name: application/name: contract/' "$repo_path/.agent-loop.yaml"
sed -i '' 's#paths: \[src/\*\*\]#paths: [offline-contract/**]#' "$repo_path/.agent-loop.yaml"
sed -i '' 's/auto_merge: false/auto_merge: true/' "$repo_path/.agent-loop.yaml"
printf '%s\n' '# Offline release contract repository' >"$repo_path/README.md"
git -C "$repo_path" add .agent-loop.yaml README.md
git -C "$repo_path" commit -q -m 'offline contract base'
git -C "$repo_path" branch -M main
git -C "$repo_path" push -q -u origin main
git --git-dir "$remote_path" symbolic-ref HEAD refs/heads/main

export OFFLINE_CONTRACT_STATE="$stub_state"
export OFFLINE_CONTRACT_REMOTE="$remote_path"
export HOME="$temporary_root/home"
export AGENT_LOOP_HOME="$state_root"
export AGENT_LOOP_LAUNCH_AGENTS_DIR="$launch_root"
export GH_CONFIG_DIR="$temporary_root/gh-config"
export CODEX_HOME="$temporary_root/codex-home"
export PATH="$stub_bin:$PATH"
export HTTP_PROXY=http://127.0.0.1:9
export HTTPS_PROXY=http://127.0.0.1:9
export ALL_PROXY=http://127.0.0.1:9
export NO_PROXY=127.0.0.1,localhost

"$stub_bin/offline-contract-stub" seed
cp "$binary" "$state_root/bin/agent-loop"
chmod 0755 "$state_root/bin/agent-loop"
"$binary" register --repo "$repo_path" --json >"$temporary_root/register.json"

start_supervisor() {
  "$binary" run --repo "$repo_path" >"$temporary_root/supervisor.log" 2>"$temporary_root/supervisor.err" &
  supervisor_pid=$!
  sleep 1
  if ! kill -0 "$supervisor_pid" 2>/dev/null; then
    tail -100 "$temporary_root/supervisor.err" >&2
    exit 1
  fi
  supervisor_starts=$((supervisor_starts + 1))
}

wait_issue_status() {
  issue_number=$1
  wanted=$2
  deadline=$(( $(date +%s) + 180 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    "$binary" status --repo "$repo_path" --json >"$temporary_root/status.json"
    current=$(jq -r --arg key "$issue_number" '.state.issues[$key].status // ""' "$temporary_root/status.json")
    if [ "$current" = "$wanted" ]; then
      return
    fi
    if ! kill -0 "$supervisor_pid" 2>/dev/null; then
      tail -100 "$temporary_root/supervisor.err" >&2
      exit 1
    fi
    sleep 1
  done
  printf '%s\n' "offline Issue #$issue_number did not reach $wanted" >&2
  tail -100 "$temporary_root/supervisor.err" >&2
  exit 1
}

stop_idle_supervisor() {
  deadline=$(( $(date +%s) + 60 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    "$binary" status --repo "$repo_path" --json >"$temporary_root/status.json"
    if [ "$(jq -r '.worker_pool.active' "$temporary_root/status.json")" = 0 ]; then
      kill -TERM "$supervisor_pid"
      wait "$supervisor_pid"
      supervisor_pid=
      return
    fi
    sleep 1
  done
  printf '%s\n' 'offline supervisor did not reach an idle durable checkpoint' >&2
  exit 1
}

start_supervisor
wait_issue_status 1 completed
wait_issue_status 2 needs_input
stop_idle_supervisor

request_id=$(jq -r '.pending_requests[] | select(.issue_number == 2 and .status == "pending") | .id' "$temporary_root/status.json" | head -1)
[ -n "$request_id" ]
printf '%s\n' yes | "$binary" answer --repo "$repo_path" --request-id "$request_id" --message-file - --json >/dev/null

start_supervisor
wait_issue_status 2 completed
stop_idle_supervisor

"$binary" status --repo "$repo_path" --json >"$temporary_root/final-status.json"
"$stub_bin/offline-contract-stub" summary >"$temporary_root/stub-summary.json"
git clone -q "$remote_path" "$temporary_root/verified"

test "$(cat "$temporary_root/verified/offline-contract/sequence-1.txt")" = completed
test "$(cat "$temporary_root/verified/offline-contract/sequence-2.txt")" = resumed
jq -e '
  .worker_pool.limit == 1 and .worker_pool.active == 0 and
  ([.state.issues[] | select(.status != "completed")] | length) == 0 and
  ([.state.issues[] | select(.lease != null)] | length) == 0 and
  ([.state.issues[] | select((.worker_pid // 0) != 0 or (.worker_pgid // 0) != 0)] | length) == 0 and
  ([.pending_requests[] | select(.status == "pending")] | length) == 0
' "$temporary_root/final-status.json" >/dev/null
jq -e '
  ([.issues[] | select(.state != "CLOSED")] | length) == 0 and
  (.pull_requests | length) == 2 and
  ([.pull_requests[] | select(.state != "MERGED")] | length) == 0 and
  ([.pull_requests[].headRefName] | unique | length) == 2 and
  ([.issues[] | ([.comments[] | scan("<!-- codex-issue-loop:[^>]+ -->")] | group_by(.) | map(select(length > 1)) | length)] | add) == 0 and
  ([.calls[] | select(. == "codex issue=1 resumed=false")] | length) == 1 and
  ([.calls[] | select(. == "codex issue=2 resumed=false")] | length) == 1 and
  ([.calls[] | select(. == "codex issue=2 resumed=true")] | length) == 1
' "$temporary_root/stub-summary.json" >/dev/null

transaction_crash_recovery=0
HOME="$operator_home" GOTOOLCHAIN="$go_toolchain" go test ./internal/application/conformance \
  -run '^TestFaultDurableTransactionFiveCrashBoundaries$' -count=1 >"$temporary_root/transaction-replay.log"
transaction_crash_recovery=1
webhook_fixture_replay=0
HOME="$operator_home" GOTOOLCHAIN="$go_toolchain" go test ./internal/adapter/webhook ./internal/application/supervisor \
  -run '^(TestSharedBrokerSafetySweepPaginatesAndWarmsWith304|TestWebhookMailboxClaimsReadyIssueWithoutQueuePolling|TestSweepCollectionExitUsesTargetedAuthorityAndBlocksManualExclusion)$' \
  -count=1 >"$temporary_root/webhook-replay.log"
webhook_fixture_replay=1

candidate_digest=$(shasum -a 256 "$binary" | awk '{print $1}')
jq -n \
  --arg candidate_sha256 "$candidate_digest" \
  --argjson supervisor_starts "$supervisor_starts" \
  --argjson transaction_crash_recovery "$transaction_crash_recovery" \
  --argjson webhook_fixture_replay "$webhook_fixture_replay" \
  --slurpfile status "$temporary_root/final-status.json" \
  --slurpfile stub "$temporary_root/stub-summary.json" '
  {
    schema_version: 1,
    mode: "credentialless-offline",
    candidate_sha256: $candidate_sha256,
    credentials: {canary_github_token: false, openai_api_key: false},
    external_network: false,
    sequences: [
      {id: "claim-worker-publication-checks-terminal", issue: 1, status: "completed"},
      {id: "needs-input-answer-resume-publication-terminal", issue: 2, status: "completed"}
    ],
    supervisor_starts: $supervisor_starts,
    transaction_crash_recovery: $transaction_crash_recovery,
    webhook_fixture_replay: $webhook_fixture_replay,
    pull_requests: ($stub[0].pull_requests | length),
    command_count: ($stub[0].calls | length),
    final: {
      worker_limit: $status[0].worker_pool.limit,
      active_workers: $status[0].worker_pool.active,
      active_leases: ([$status[0].state.issues[] | select(.lease != null)] | length),
      pending_requests: ([$status[0].pending_requests[] | select(.status == "pending")] | length),
      orphan_pid_pgid: ([$status[0].state.issues[] | select((.worker_pid // 0) != 0 or (.worker_pgid // 0) != 0)] | length),
      duplicate_prs: (($stub[0].pull_requests | to_entries | map(.value.headRefName) | length) - ($stub[0].pull_requests | to_entries | map(.value.headRefName) | unique | length)),
      duplicate_comment_markers: ([$stub[0].issues[] | ([.comments[] | scan("<!-- codex-issue-loop:[^>]+ -->")] | group_by(.) | map(select(length > 1)) | length)] | add)
    }
  }
' >"$artifact_dir/offline-contract-report.json"

jq 'del(.candidate_sha256, .command_count)' "$artifact_dir/offline-contract-report.json" >"$temporary_root/offline-contract.normalized.json"
cmp "$temporary_root/offline-contract.normalized.json" "$workspace/internal/contract/testdata/offline-release-contract.golden.json"
