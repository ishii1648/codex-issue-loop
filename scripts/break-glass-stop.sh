#!/bin/sh
set -eu

repo_id=""
managed_root="${AGENT_LOOP_HOME:-$HOME/Library/Application Support/codex-issue-loop}"
launch_agents_dir="${AGENT_LOOP_LAUNCH_AGENTS_DIR:-$HOME/Library/LaunchAgents}"
launchctl_bin="${AGENT_LOOP_LAUNCHCTL:-launchctl}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --repo-id) repo_id=${2:?}; shift 2 ;;
    --managed-root) managed_root=${2:?}; shift 2 ;;
    --launch-agents-dir) launch_agents_dir=${2:?}; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

case "$repo_id" in
  ""|*[!a-zA-Z0-9._-]*) echo "--repo-id must be an exact registered repository ID" >&2; exit 2 ;;
esac
case "$managed_root:$launch_agents_dir" in
  /*:/*) ;;
  *) echo "managed root and LaunchAgents directory must be absolute" >&2; exit 2 ;;
esac

registry="$managed_root/registry.json"
jq -e --arg id "$repo_id" '.repos[$id].repo_id == $id' "$registry" >/dev/null

uid=$(id -u)
stopped='[]'
for label in "com.codex-issue-loop.delivery" "com.codex-issue-loop.$repo_id"; do
  plist="$launch_agents_dir/$label.plist"
  [ -f "$plist" ] && [ ! -L "$plist" ]
  grep -F "<key>Label</key><string>$label</string>" "$plist" >/dev/null
  target="gui/$uid/$label"
  if "$launchctl_bin" print "$target" >/dev/null 2>&1; then
    "$launchctl_bin" bootout "$target"
    stopped=$(jq -cn --argjson values "$stopped" --arg label "$label" '$values + [$label]')
  fi
done

jq -cn --arg repo_id "$repo_id" --argjson stopped "$stopped" '{version:1,repo_id:$repo_id,stopped:$stopped,state_modified:false}'
