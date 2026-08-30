#!/bin/sh
set -eu

command -v codex >/dev/null
codex login status >/dev/null
version=$(codex --version)
printf '%s' "$version" | grep -Eq '[0-9]+\.[0-9]+\.[0-9]+'
codex exec --help | grep -q -- --output-schema
codex exec --cd . resume --help | grep -q -- --output-last-message

repo_root=$(git rev-parse --show-toplevel)
go_toolchain=${GOTOOLCHAIN:-go1.25.13}
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-codex-contract.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM
mkdir -p "$temporary_root/repo"
git -C "$temporary_root/repo" init -q
git -C "$temporary_root/repo" -c user.name=contract -c user.email=contract@example.invalid -c commit.gpgsign=false commit --allow-empty -m initial -q

success_prompt='Return only a schema-conforming worker result with version 1, status completed, execution_profile standard, summary contract-success, question null, tests empty, git null, retry null. Do not modify files or run commands.'
printf '%s\n' "$success_prompt" | codex exec --json --cd "$temporary_root/repo" \
  --output-schema "$repo_root/internal/adapter/worker/worker-result.schema.json" \
  --output-last-message "$temporary_root/success.json" - >"$temporary_root/success.jsonl"
jq -e '.version == 1 and .status == "completed" and .execution_profile == "standard"' "$temporary_root/success.json" >/dev/null
session_id=$(jq -r 'select(.type == "thread.started") | .thread_id // empty' "$temporary_root/success.jsonl" | head -1)
[ -n "$session_id" ]

needs_input_prompt='Return only a schema-conforming worker result with version 1, status needs_input, execution_profile extended, summary contract-needs-input, one question with text Continue?, reason contract, recommended_option yes, one option labeled yes with description Continue, allow_free_text true, tests empty, git null, retry null. Do not modify files or run commands.'
printf '%s\n' "$needs_input_prompt" | codex exec --cd "$temporary_root/repo" resume --json \
  --output-schema "$repo_root/internal/adapter/worker/worker-result.schema.json" \
  --output-last-message "$temporary_root/needs-input.json" "$session_id" - >"$temporary_root/resume.jsonl"
jq -e '.version == 1 and .status == "needs_input" and .question.allow_free_text == true' "$temporary_root/needs-input.json" >/dev/null

GOTOOLCHAIN="$go_toolchain" go test ./internal/adapter/worker -run '^TestDecodeResultRevalidatesPublishedSchemaShape$' -count=1
GOTOOLCHAIN="$go_toolchain" go test ./internal/platform/compat -run '^(TestCapabilityProbes|TestCodexProbeRejectsResumeThatCannotAcceptPinnedWorkspace)$' -count=1

artifact_dir=${CONTRACT_ARTIFACT_DIR:-$temporary_root}
mkdir -p "$artifact_dir"
printf '{"schema_version":1,"runtime_version_present":true,"capability_probe":true,"structured_success":true,"needs_input":true,"session_resume":true,"malformed_json_rejected":true,"unsupported_capability_fail_closed":true}\n' >"$artifact_dir/codex-cli-contract.json"
cmp "$artifact_dir/codex-cli-contract.json" "$repo_root/internal/contract/testdata/codex-cli-contract.golden.json"
