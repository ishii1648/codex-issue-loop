#!/bin/sh
set -eu

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-release-check.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM

commit=$(git rev-parse HEAD)
source_epoch=$(git show -s --format=%ct HEAD)
version=v0.0.0-test

scripts/build-release.sh "$version" "$commit" "$source_epoch" "$temporary_root/first"
scripts/build-release.sh "$version" "$commit" "$source_epoch" "$temporary_root/second"

for name in agent-loop_Darwin_arm64 agent-loop_Darwin_arm64.spdx.json checksums.txt; do
  cmp "$temporary_root/first/$name" "$temporary_root/second/$name"
done

version_json=$($temporary_root/first/agent-loop_Darwin_arm64 version --json)
printf '%s\n' "$version_json" | grep -Fq '"version":"v0.0.0-test"'
printf '%s\n' "$version_json" | grep -Fq "\"commit\":\"$commit\""
printf '%s\n' "$version_json" | grep -Fq '"state_schema_current":4'
printf '%s\n' "$version_json" | grep -Fq '"state_schema_migration_from":3'
printf '%s\n' "$version_json" | grep -Fq '"semantic_contract_current":1'

# A new execution-required field without an explicit compatibility or
# migration decision must fail both normal CI and the release gate.
go test ./internal/statecontract ./internal/state \
  -run '^Test(CurrentContractHasMigrationRulesForEveryExecutionRequirement|EveryExecutionRequiredFieldHasRuntimeValidator)$' \
  -count=1

# Recovery fixtures are production-derived release evidence. Refuse a release
# if a reviewed byte changes, or if its internal completeness/hash manifest no
# longer verifies. Updating both files requires an explicit fixture review.
while read -r expected fixture; do
  [ -n "$expected" ] || continue
  fixture_path="internal/recoveryfixture/testdata/$fixture"
  actual=$(shasum -a 256 "$fixture_path" | awk '{print $1}')
  [ "$actual" = "$expected" ]
  "$temporary_root/first/agent-loop_Darwin_arm64" verify-recovery-fixture --fixture "$fixture_path" --json >/dev/null
done < internal/recoveryfixture/testdata/blessed-fixtures.sha256
