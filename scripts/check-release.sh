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
