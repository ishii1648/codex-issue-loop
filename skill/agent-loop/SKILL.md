---
name: agent-loop
description: Operate and monitor the codex-issue-loop supervisor from Codex. Use for starting, stopping, inspecting, watching, or answering a loop-managed GitHub Issue queue.
---

# agent-loop

Use the `agent-loop` CLI as the only control interface. The Skill does not own the Issue loop or its durable state.

## Safe workflow

1. Run `agent-loop doctor --repo <path> --json` before the first start or after configuration changes.
2. Run `agent-loop status --repo <path> --json` before mutating loop state.
3. Use `start`, `stop`, or `restart` only for the repository the user named.
4. If status contains a pending request, present that request before starting a new watch.
5. Otherwise call `agent-loop watch --repo <path> --until-attention --json` once.
6. Do not implement a Codex-side polling loop. The Go watch process combines OS events with reconciliation polling internally.
7. When watch returns `needs_input`, preserve the request ID, ask the question with its recommendation and options, then record the answer with `agent-loop answer --request-id <id> --message-file -`.
8. Return to one blocking watch call after the answer is recorded.

Confirm the exact repository and impact before `stop`, `restart`, `unregister`, or `uninstall`. None of these commands should delete worktrees or uncommitted changes.
