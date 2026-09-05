#!/bin/sh
set -eu

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-release-check.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM

commit=$(git rev-parse HEAD)
source_epoch=$(git show -s --format=%ct HEAD)
version=v0.0.0-test

run_host_go_test() {
  if [ "$(go env GOHOSTOS)" = darwin ]; then
    CGO_ENABLED=1 go test -ldflags=-linkmode=external "$@"
  else
    go test "$@"
  fi
}

scripts/build-release.sh "$version" "$commit" "$source_epoch" "$temporary_root/first"
scripts/build-release.sh "$version" "$commit" "$source_epoch" "$temporary_root/second"

for name in agent-loop_Darwin_arm64 agent-loop_Darwin_arm64.spdx.json release-manifest.json checksums.txt; do
  cmp "$temporary_root/first/$name" "$temporary_root/second/$name"
done

version_json=$($temporary_root/first/agent-loop_Darwin_arm64 version --json)
printf '%s\n' "$version_json" | grep -Fq '"version":"v0.0.0-test"'
printf '%s\n' "$version_json" | grep -Fq "\"commit\":\"$commit\""
printf '%s\n' "$version_json" | grep -Fq '"target":"darwin/arm64"'
printf '%s\n' "$version_json" | grep -Fq '"delivery_protocol":1'
printf '%s\n' "$version_json" | grep -Fq '"assignment_protocol":1'
printf '%s\n' "$version_json" | grep -Fq '"state_schema_current":5'
printf '%s\n' "$version_json" | grep -Fq '"state_schema_migration_from":4'
printf '%s\n' "$version_json" | grep -Fq '"semantic_contract_current":4'
printf '%s\n' "$version_json" | grep -Fq '"issue_lifecycle_api_current":"2.1"'
printf '%s\n' "$version_json" | grep -Fq '"issue_lifecycle_api_minimum":"1.0"'
grep -Fq "\"artifact_sha256\": \"$(shasum -a 256 "$temporary_root/first/agent-loop_Darwin_arm64" | awk '{print $1}')\"" "$temporary_root/first/release-manifest.json"
grep -Fq '"delivery_protocol": 1' "$temporary_root/first/release-manifest.json"
grep -Fq '"assignment_protocol": 1' "$temporary_root/first/release-manifest.json"
grep -Fq '"target": "darwin/arm64"' "$temporary_root/first/release-manifest.json"
grep -Fq '"state_schema_current": 5' "$temporary_root/first/release-manifest.json"
grep -Fq '"state_schema_migration_from": 4' "$temporary_root/first/release-manifest.json"
grep -Fq '"semantic_contract_current": 4' "$temporary_root/first/release-manifest.json"
grep -Fq '"issue_lifecycle_api_current": "2.1"' "$temporary_root/first/release-manifest.json"
grep -Fq '"issue_lifecycle_api_minimum": "1.0"' "$temporary_root/first/release-manifest.json"
grep -Fq '"semantic_contract_minimum": 1' "$temporary_root/first/release-manifest.json"

# A new execution-required field without an explicit compatibility or
# migration decision must fail both normal CI and the release gate.
run_host_go_test ./internal/domain/statecontract ./internal/adapter/state \
  -run '^Test(CurrentContractHasMigrationRulesForEveryExecutionRequirement|EveryExecutionRequiredFieldHasRuntimeValidator)$' \
  -count=1
run_host_go_test ./internal/application/migration \
  -run '^Test(ProductionDerivedV4RecoveryMatrixMigratesElevenIssuesAndFourteenSubstatesWithoutLoss|V4PreparedTransactionMigratesItsSnapshotThroughTheSameV5Boundary)$' \
  -count=1
run_host_go_test ./internal/application/delivery -run '^Test(ProductionStateIsolationRunsCredentiallessContractBetweenSnapshots|ProductionReleaseHealthFailsClosed|ProductionAssignmentHealthRequiresExactStableAssignmentsAndRollbackDrill|ReleaseWorkflowPreservesRequiredGateChain|ContractWorkflowsRequireNoLongLivedSecrets|HighRiskReviewUsesMachineVerifiableEvidence)$' -count=1

help_output=$($temporary_root/first/agent-loop_Darwin_arm64 help)
printf '%s\n' "$help_output" | grep -Fq 'issue         Plan or resolve one typed Issue suspension'
if printf '%s\n' "$help_output" | grep -Eq '^[[:space:]]+retry([[:space:]]|$)'; then
  printf '%s\n' "legacy recovery command remains in help: retry" >&2
  exit 1
fi
for legacy_command in resume-blocked recover-publication recover-checks recover-answered-workspace recover-workspace adopt-merged-pr explain-recovery; do
  if printf '%s\n' "$help_output" | grep -Fq "$legacy_command"; then
    printf '%s\n' "legacy recovery command remains in help: $legacy_command" >&2
    exit 1
  fi
done

conformance_json="$temporary_root/conformance.jsonl"
run_host_go_test ./internal/application/conformance -count=1 -json >"$conformance_json"
if grep -Eq '"Action":"(skip|fail)"' "$conformance_json"; then
  cat "$conformance_json"
  exit 1
fi

# Recovery fixtures are production-derived release evidence. Refuse a release
# if a reviewed byte changes, or if its internal completeness/hash manifest no
# longer verifies. Updating both files requires an explicit fixture review.
while read -r expected fixture; do
  [ -n "$expected" ] || continue
  fixture_path="internal/application/recoveryfixture/testdata/$fixture"
  actual=$(shasum -a 256 "$fixture_path" | awk '{print $1}')
  [ "$actual" = "$expected" ]
  "$temporary_root/first/agent-loop_Darwin_arm64" verify-recovery-fixture --fixture "$fixture_path" --json >/dev/null
done < internal/application/recoveryfixture/testdata/blessed-fixtures.sha256

while read -r expected fixture; do
  [ -n "$expected" ] || continue
  fixture_path="internal/contract/testdata/$fixture"
  actual=$(shasum -a 256 "$fixture_path" | awk '{print $1}')
  [ "$actual" = "$expected" ]
done < internal/contract/testdata/contract-fixtures.sha256
