#!/bin/sh
set -eu

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-release-check.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM

commit=$(git rev-parse HEAD)
source_epoch=$(git show -s --format=%ct HEAD)
version=v0.0.0-test

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
printf '%s\n' "$version_json" | grep -Fq '"state_schema_current":4'
printf '%s\n' "$version_json" | grep -Fq '"state_schema_migration_from":3'
printf '%s\n' "$version_json" | grep -Fq '"semantic_contract_current":1'
grep -Fq "\"artifact_sha256\": \"$(shasum -a 256 "$temporary_root/first/agent-loop_Darwin_arm64" | awk '{print $1}')\"" "$temporary_root/first/release-manifest.json"
grep -Fq '"delivery_protocol": 1' "$temporary_root/first/release-manifest.json"
grep -Fq '"target": "darwin/arm64"' "$temporary_root/first/release-manifest.json"
grep -Fq '"state_schema_current": 4' "$temporary_root/first/release-manifest.json"
grep -Fq '"semantic_contract_minimum": 0' "$temporary_root/first/release-manifest.json"

# A new execution-required field without an explicit compatibility or
# migration decision must fail both normal CI and the release gate.
go test ./internal/domain/statecontract ./internal/adapter/state \
  -run '^Test(CurrentContractHasMigrationRulesForEveryExecutionRequirement|EveryExecutionRequiredFieldHasRuntimeValidator)$' \
  -count=1
go test ./internal/application/delivery -run '^Test(ProductionStateIsolationRunsCredentiallessContractBetweenSnapshots|ProductionReleaseHealthFailsClosed|ReleaseWorkflowPreservesRequiredGateChain|ContractWorkflowsRequireNoLongLivedSecrets|HighRiskReviewScopesIndependentApprovalToHighRiskChanges)$' -count=1

conformance_json="$temporary_root/conformance.jsonl"
go test ./internal/application/conformance -count=1 -json >"$conformance_json"
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
