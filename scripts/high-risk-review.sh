#!/bin/sh
set -eu

base=${BASE_SHA:?BASE_SHA is required}
head=${HEAD_SHA:?HEAD_SHA is required}
output=${REVIEW_OUTPUT:?REVIEW_OUTPUT is required}
mkdir -p "$(dirname "$output")"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
jq -n --arg base "$base" --arg head "$head" '{schema_version:2,base:$base,head:$head,high_risk:false,checks:{},findings:[],finding_count:0}' >"$work/report"

check() {
  name=$1 status=$2 required=$3 evidence=$4
  if [ "$evidence" != "$work/evidence" ]; then printf '%s\n' "$evidence" >"$work/evidence"; fi
  jq --arg name "$name" --arg status "$status" --argjson required "$required" --slurpfile evidence "$work/evidence" '
    .checks[$name] = {status:$status,required:$required,evidence:$evidence[0]} |
    if $required and $status != "passed" then .findings += [$name + ":" + $status] else . end
  ' "$work/report" >"$work/next"
  mv "$work/next" "$work/report"
}

finish() {
  jq '.finding_count = (.findings | length)' "$work/report" >"$output"
  jq -e '.finding_count == 0' "$output" >/dev/null
}

for name in specification_mapping invariants migration fault_tests release_compatibility rollback secret_exposure; do
  check "$name" unverified false '{"reason":"Target revision has not been verified"}'
done
if ! git rev-parse --verify "$base^{commit}" >"$work/base" 2>/dev/null ||
   ! git rev-parse --verify "$head^{commit}" >"$work/head" 2>/dev/null ||
   [ "$(cat "$work/base")" != "$base" ] || [ "$(cat "$work/head")" != "$head" ] ||
   [ "$(git rev-parse HEAD)" != "$head" ] ||
   ! git diff --quiet HEAD || ! git diff --cached --quiet ||
   [ -n "$(git ls-files --others --exclude-standard)" ] ||
   ! git merge-base "$base" "$head" >"$work/merge-base"; then
  check target_revision unverified true '{}'
  finish
  exit 1
fi
merge_base=$(cat "$work/merge-base")
check target_revision passed true "$(jq -n --arg base "$base" --arg head "$head" --arg merge_base "$merge_base" '{base:$base,head:$head,merge_base:$merge_base}')"
git diff --name-only "$merge_base" "$head" >"$work/changed"
if ! grep -E '^(internal/adapter/state/|internal/application/supervisor/|internal/domain/issue/|internal/domain/statecontract/|internal/platform/schema/|internal/application/migration/|internal/application/delivery/|\.github/workflows/|scripts/(high-risk-review|check-release)\.sh$|\.agent-loop.*\.yaml$)' "$work/changed" >"$work/risk"; then
  for name in specification_mapping invariants migration fault_tests release_compatibility rollback secret_exposure; do
    check "$name" not_applicable false '{}'
  done
  finish
  exit 0
fi
jq --rawfile paths "$work/risk" '.high_risk = true | .changed_paths = ($paths | split("\n") | map(select(length > 0)))' "$work/report" >"$work/next"
mv "$work/next" "$work/report"
check specification_mapping unverified false '{"reason":"Requires independent review of requirements, base specification and diff"}'
check rollback unverified false '{"reason":"Requires human judgment of rollback feasibility"}'

suite() {
  suite_name=$1 pattern=$2
  shift 2
  result=0
  go test -json -count=1 -run "$pattern" "$@" >"$work/events" 2>"$work/stderr" || result=$?
  status=unverified
  if [ "$result" -ne 0 ] && [ "$result" -ne 126 ] && [ "$result" -ne 127 ]; then
    status=failed
  fi
  # A package-level pass without an executed test (including an empty suite) is insufficient.
  if [ "$result" -eq 0 ]; then
    packages=$(printf '%s\n' "$@" | sed 's|^\./|github.com/ishii1648/codex-issue-loop/|' | jq -R . | jq -s .)
    if jq -se --argjson packages "$packages" '
      . as $events | length > 0 and
      all(.[]; type == "object" and (.Action | type == "string") and .Action != "fail") and
      ([.[] | select(.Test == null and .Action == "pass") | .Package] | sort) == ($packages | sort) and
      all($packages[]; . as $pkg | any($events[]; .Package == $pkg and (.Test | type == "string" and length > 0) and .Action == "pass" and
        (.Test as $test | any($events[]; .Package == $pkg and .Test == $test and .Action == "run"))))
    ' "$work/events" >/dev/null 2>&1; then status=passed; else status=unverified; fi
  fi
  printf '[]\n' >"$work/executed"
  if jq -se 'all(.[]; type == "object")' "$work/events" >/dev/null 2>&1; then
    jq -s '[.[] | select(.Test != null and (.Action == "pass" or .Action == "fail" or .Action == "skip")) | {package:.Package,test:.Test,result:.Action}]' "$work/events" >"$work/executed"
  fi
  jq -n --slurpfile executed "$work/executed" --arg base "$base" --arg head "$head" --arg pattern "$pattern" --argjson exit_code "$result" --args '{base:$base,head:$head,command:(["go","test","-json","-count=1","-run",$pattern]+$ARGS.positional),exit_code:$exit_code,tests:$executed[0]}' -- "$@" >"$work/evidence"
  check "$suite_name" "$status" true "$work/evidence"
}

suite invariants '^Test' ./internal/adapter/state ./internal/domain/issue ./internal/domain/statecontract ./internal/application/conformance
suite migration '^Test' ./internal/application/migration
suite fault_tests '^TestFault' ./internal/adapter/state ./internal/application/supervisor ./internal/application/migration ./internal/application/delivery ./internal/application/conformance
suite release_compatibility '^Test' ./internal/application/delivery
if git diff "$merge_base" "$head" -- . | grep -E '^\+.*(gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9]{20,}|BEGIN (RSA|OPENSSH|EC) PRIVATE KEY)' >/dev/null; then
  check secret_exposure failed true '{"method":"added-line credential pattern scan"}'
else
  check secret_exposure passed true '{"method":"added-line credential pattern scan; not a complete secret audit"}'
fi
if [ "$(git rev-parse HEAD)" != "$head" ] || ! git diff --quiet HEAD || ! git diff --cached --quiet || [ -n "$(git ls-files --others --exclude-standard)" ]; then
  check target_revision unverified true '{}'
fi
finish
