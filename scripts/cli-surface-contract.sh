#!/bin/sh

set -eu

artifact_dir=${CONTRACT_ARTIFACT_DIR:?CONTRACT_ARTIFACT_DIR is required}
workspace=${GITHUB_WORKSPACE:-$(git rev-parse --show-toplevel)}
[ -z "${CANARY_GITHUB_TOKEN:-}" ]
[ -z "${OPENAI_API_KEY:-}" ]
command -v gh >/dev/null
command -v codex >/dev/null
mkdir -p "$artifact_dir"

require_options() {
  output=$("$@" --help)
  for option in $REQUIRED_OPTIONS; do
    printf '%s\n' "$output" | grep -Fq -- "$option"
  done
}

gh_version=$(gh --version | head -1)
codex_version=$(codex --version | head -1)
printf '%s\n' "$gh_version" | grep -Eq 'gh version [0-9]+\.[0-9]+\.[0-9]+'
printf '%s\n' "$codex_version" | grep -Eq '[0-9]+\.[0-9]+\.[0-9]+'

REQUIRED_OPTIONS='--json --limit --label --assignee --milestone' require_options gh issue list
REQUIRED_OPTIONS='--json --jq' require_options gh issue view
REQUIRED_OPTIONS='--add-label --remove-label' require_options gh issue edit
REQUIRED_OPTIONS='--body' require_options gh issue comment
REQUIRED_OPTIONS='--repo' require_options gh issue close
REQUIRED_OPTIONS='--json --head --limit --state' require_options gh pr list
REQUIRED_OPTIONS='--base --head --title --body --draft' require_options gh pr create
REQUIRED_OPTIONS='--json --jq' require_options gh pr view
REQUIRED_OPTIONS='--repo' require_options gh pr ready
REQUIRED_OPTIONS='--repo' require_options gh pr update-branch
REQUIRED_OPTIONS='--squash --repo' require_options gh pr merge
REQUIRED_OPTIONS='--json --limit' require_options gh label list
REQUIRED_OPTIONS='--color --description --repo' require_options gh label create
REQUIRED_OPTIONS='--json --jq' require_options gh release view
REQUIRED_OPTIONS='--prerelease --target --title --notes' require_options gh release create
REQUIRED_OPTIONS='--pattern --dir --clobber' require_options gh release download
REQUIRED_OPTIONS='--repo' require_options gh attestation verify

REQUIRED_OPTIONS='--json --output-schema --output-last-message --sandbox --cd' require_options codex exec
resume_help=$(codex exec --cd . resume --help)
for option in --json --output-schema --output-last-message; do
  printf '%s\n' "$resume_help" | grep -Fq -- "$option"
done
features=$(codex features list)
for feature in network_proxy apps browser_use computer_use plugins remote_plugin skill_search tool_suggest; do
  printf '%s\n' "$features" | grep -Fq -- "$feature"
done

jq -n --arg gh "$gh_version" --arg codex "$codex_version" '
  {
    schema_version: 1,
    mode: "no-auth-surface",
    credentials: {canary_github_token: false, openai_api_key: false},
    inference_requests: 0,
    gh: {version: $gh, command_groups: 16},
    codex: {version: $codex, exec: true, resume: true, guarded_features: 8}
  }
' >"$artifact_dir/cli-surface-report.json"

normalized=$(mktemp "${TMPDIR:-/tmp}/agent-loop-cli-surface.XXXXXX")
trap 'rm -f "$normalized"' EXIT HUP INT TERM
jq 'del(.gh.version, .codex.version)' "$artifact_dir/cli-surface-report.json" >"$normalized"
cmp "$normalized" "$workspace/internal/contract/testdata/cli-surface-contract.golden.json"
