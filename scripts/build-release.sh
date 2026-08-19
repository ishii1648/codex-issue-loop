#!/bin/sh
set -eu

if test "$#" -ne 4; then
  echo "usage: build-release.sh <version> <commit> <source-date-epoch> <output-dir>" >&2
  exit 2
fi

version=$1
commit=$2
source_epoch=$3
output_dir=$4

if ! printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$'; then
  echo "version must be a v-prefixed semantic version" >&2
  exit 2
fi
if ! printf '%s\n' "$commit" | grep -Eq '^[0-9a-f]{40}$'; then
  echo "commit must be a 40-character lowercase hexadecimal Git commit" >&2
  exit 2
fi
case "$source_epoch" in
  *[!0-9]*|'') echo "source-date-epoch must be an integer" >&2; exit 2 ;;
esac

mkdir -p "$output_dir"
artifact="$output_dir/agent-loop_Darwin_arm64"
sbom="$output_dir/agent-loop_Darwin_arm64.spdx.json"
checksums="$output_dir/checksums.txt"
manifest="$output_dir/release-manifest.json"

CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build \
  -trimpath \
  -buildvcs=false \
  -ldflags "-s -w -X github.com/ishii1648/codex-issue-loop/internal/app.Version=$version -X github.com/ishii1648/codex-issue-loop/internal/app.Commit=$commit" \
  -o "$artifact" \
  ./cmd/agent-loop

SOURCE_DATE_EPOCH=$source_epoch go run ./cmd/sbom \
  --artifact "$artifact" \
  --version "$version" \
  --output "$sbom"

go run ./cmd/releasemanifest \
  --artifact "$artifact" \
  --version "$version" \
  --commit "$commit" \
  --output "$manifest"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$output_dir" && sha256sum agent-loop_Darwin_arm64 agent-loop_Darwin_arm64.spdx.json release-manifest.json > checksums.txt)
else
  (cd "$output_dir" && shasum -a 256 agent-loop_Darwin_arm64 agent-loop_Darwin_arm64.spdx.json release-manifest.json > checksums.txt)
fi

chmod 0755 "$artifact"
chmod 0644 "$sbom" "$manifest" "$checksums"
