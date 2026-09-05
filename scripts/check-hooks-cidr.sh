#!/bin/sh
set -eu

# Compares the recorded GitHub webhook source CIDRs with the current values
# published by the GitHub meta API. Webhook mode allowlists these ranges on the
# reverse proxy, so a silent upstream change would start rejecting deliveries.
# Prints a diff and exits 1 when they differ. Requires an authenticated gh.

baseline=.github/github-hooks-cidr.txt

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-hooks-cidr.XXXXXX")
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM

current="$temporary_root/current.txt"
gh api meta --jq '.hooks[]' | LC_ALL=C sort >"$current"

if [ ! -s "$current" ]; then
  echo "github meta returned no webhook CIDRs" >&2
  exit 2
fi

if diff -u "$baseline" "$current"; then
  echo "GitHub webhook source CIDRs match $baseline."
  exit 0
fi

echo "GitHub webhook source CIDRs changed. Update the reverse proxy allowlist and $baseline." >&2
exit 1
