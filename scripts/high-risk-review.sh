#!/bin/sh
set -eu

base=${BASE_SHA:?BASE_SHA is required}
head=${HEAD_SHA:?HEAD_SHA is required}
output=${REVIEW_OUTPUT:?REVIEW_OUTPUT is required}
merge_base=$(git merge-base "$base" "$head")
changed=$(git diff --name-only "$merge_base" "$head")
high_risk=$(printf '%s\n' "$changed" | grep -E '^(internal/adapter/state/|internal/application/supervisor/|internal/domain/issue/|internal/domain/statecontract/|internal/application/migration/|internal/application/delivery/|\.github/workflows/|scripts/check-release\.sh$|\.agent-loop.*\.yaml$)' || true)
findings=

add_finding() {
  if [ -z "$findings" ]; then findings=$1; else findings="$findings,$1"; fi
}

if [ -n "$high_risk" ]; then
  production_go=$(printf '%s\n' "$high_risk" | grep -E '\.go$' | grep -Ev '_test\.go$' || true)
  tests=$(printf '%s\n' "$changed" | grep -E '(_test\.go$|internal/application/conformance/)' || true)
  [ -z "$production_go" ] || [ -n "$tests" ] || add_finding missing_fault_or_regression_test

  state_changes=$(printf '%s\n' "$high_risk" | grep -E '^(internal/adapter/state/|internal/domain/statecontract/)' || true)
  invariant_tests=$(printf '%s\n' "$changed" | grep -E 'internal/adapter/state/.*_test\.go$' || true)
  [ -z "$state_changes" ] || [ -n "$invariant_tests" ] || add_finding missing_invariant_test

  schema_changes=$(git diff "$merge_base" "$head" -- internal/platform/schema internal/domain/statecontract | grep -E '^\+.*(Current|Version|version)' || true)
  migration_changes=$(printf '%s\n' "$changed" | grep -E '^internal/application/migration/' || true)
  [ -z "$schema_changes" ] || [ -n "$migration_changes" ] || add_finding missing_migration_change

  release_changes=$(printf '%s\n' "$high_risk" | grep -E '^(\.github/workflows/|scripts/check-release\.sh$|\.agent-loop)' || true)
  rollback_evidence=$(printf '%s\n' "$changed" | grep -E '^(docs/|README\.md$|scripts/check-release\.sh$)' || true)
  [ -z "$release_changes" ] || [ -n "$rollback_evidence" ] || add_finding missing_release_rollback_evidence

  if git diff "$merge_base" "$head" -- . ':!**/*_test.go' | grep -E '^\+.*(gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9]{20,}|BEGIN (RSA|OPENSSH|EC) PRIVATE KEY)' >/dev/null; then
    add_finding possible_secret_exposure
  fi
fi

mkdir -p "$(dirname "$output")"
if [ -n "$findings" ]; then
  json_findings=$(printf '%s' "$findings" | awk -F, '{printf "["; for(i=1;i<=NF;i++){if(i>1)printf ","; printf "\"%s\"",$i}; printf "]"}')
else
  json_findings='[]'
fi
jq -n --arg base "$base" --arg head "$head" --argjson high_risk "$([ -n "$high_risk" ] && printf true || printf false)" \
  --argjson findings "$json_findings" \
  '{schema_version:1,base:$base,head:$head,high_risk:$high_risk,checks:{specification_mapping:true,invariants:true,migration:true,fault_tests:true,release_compatibility:true,rollback:true,secret_exposure:true},findings:$findings,finding_count:($findings|length)}' >"$output"
[ -z "$findings" ]
