#!/bin/sh

set -u

agent_loop_bin=${AGENT_LOOP_BIN:-agent-loop}
repo_path=
timeout_seconds=0
child_pid=
watchdog_pid=
timeout_marker=
cleaned=0

usage() {
  echo "usage: $0 --repo PATH [--timeout SECONDS] -- COMMAND [ARG ...]" >&2
  exit 2
}

cleanup() {
  if [ "$cleaned" -eq 1 ]; then
    return
  fi
  cleaned=1
  if [ -n "$watchdog_pid" ]; then
    kill "$watchdog_pid" 2>/dev/null || true
    wait "$watchdog_pid" 2>/dev/null || true
  fi
  if [ -n "$child_pid" ]; then
    kill -TERM "$child_pid" 2>/dev/null || true
    wait "$child_pid" 2>/dev/null || true
  fi
  "$agent_loop_bin" stop --repo "$repo_path" --json >/dev/null 2>&1 || true
  "$agent_loop_bin" unregister --repo "$repo_path" --json >/dev/null 2>&1 || true
  if [ -n "$timeout_marker" ]; then
    rm -f "$timeout_marker"
  fi
}

on_exit() {
  exit_status=$?
  trap - EXIT HUP INT TERM
  cleanup
  exit "$exit_status"
}

on_signal() {
  exit_status=$1
  if [ -n "$child_pid" ]; then
    kill -TERM "$child_pid" 2>/dev/null || true
  fi
  exit "$exit_status"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --repo)
      [ "$#" -ge 2 ] || usage
      repo_path=$2
      shift 2
      ;;
    --timeout)
      [ "$#" -ge 2 ] || usage
      timeout_seconds=$2
      shift 2
      ;;
    --)
      shift
      break
      ;;
    *)
      usage
      ;;
  esac
done

[ -n "$repo_path" ] || usage
[ "$#" -gt 0 ] || usage
case "$timeout_seconds" in
  *[!0-9]*|'') usage ;;
esac

trap on_exit EXIT
trap 'on_signal 129' HUP
trap 'on_signal 130' INT
trap 'on_signal 143' TERM

"$agent_loop_bin" register --repo "$repo_path" --json >/dev/null || exit $?
"$agent_loop_bin" start --repo "$repo_path" --json >/dev/null || exit $?

"$@" &
child_pid=$!
if [ "$timeout_seconds" -gt 0 ]; then
  timeout_marker=$(mktemp "${TMPDIR:-/tmp}/agent-loop-e2e-timeout.XXXXXX") || exit 1
  (
    sleep "$timeout_seconds"
    printf 'timeout\n' >"$timeout_marker"
    kill -TERM "$child_pid" 2>/dev/null || true
  ) &
  watchdog_pid=$!
fi

wait "$child_pid"
command_status=$?
child_pid=
if [ -n "$watchdog_pid" ]; then
  kill "$watchdog_pid" 2>/dev/null || true
  wait "$watchdog_pid" 2>/dev/null || true
  watchdog_pid=
fi
if [ -n "$timeout_marker" ] && [ -s "$timeout_marker" ]; then
  exit 124
fi
exit "$command_status"
