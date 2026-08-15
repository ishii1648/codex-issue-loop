---
name: agent-loop
description: Operate and monitor the codex-issue-loop supervisor from Codex. Use for starting, stopping, inspecting, watching, or answering a loop-managed GitHub Issue queue.
---

# agent-loop

Use the `agent-loop` CLI as the only control interface. The Skill does not own the Issue loop or its durable state.

## Safe workflow

1. Run `agent-loop doctor --repo <path> --json` before the first start or after configuration changes.
   - Require `schema_version: 1`. If the version is unknown, stop and report that the installed CLI and Skill are incompatible.
   - Require `INSTALL_VERSION_CONSISTENT` when an installed distribution is present. If binary, Skill, or manifest versions differ, do not start the loop.
   - Require `SCHEMA_VERSION_SUPPORTED`. For `SCHEMA_MIGRATION_REQUIRED`, show `agent-loop migrate --json` as a read-only preview and do not start the loop. Apply or roll back a migration only when the user explicitly authorized the version change and every registered loop is stopped.
   - Branch on failed `diagnostics[].code`, not localized `summary` or `detail` text.
   - Present each failed diagnostic with its concrete `remediations`. Do not execute a remediation merely because doctor returned it; every remediation is advisory and has explicit `automatic` and `destructive` fields.
   - Never delete, reset, overwrite state, or change macOS/GitHub/Codex settings as an automatic repair. For `GITHUB_LABELS_MISSING`, preview `bootstrap-labels` first and apply only within the user's authorized repository scope.
2. Run `agent-loop status --repo <path> --json` before mutating loop state.
3. Use `start`, `stop`, or `restart` only for the repository the user named.
4. If status contains a pending request, present that request before starting a new watch.
5. Otherwise call `agent-loop watch --repo <path> --until-attention --json` once.
6. Do not implement a Codex-side polling loop. The Go watch process combines OS events with reconciliation polling internally.
7. When watch returns `needs_input`, preserve the request ID, ask the question with its recommendation and options, then record the answer with `agent-loop answer --request-id <id> --message-file -`.
8. Return to one blocking watch call after the answer is recorded.

Confirm the exact repository and impact before `stop`, `restart`, `unregister`, `update`, `migrate --apply`, either rollback, or `uninstall`. Use only a checksum- and attestation-verified release artifact for update. Use only the exact managed backup paths returned by update and migrate for rollback. When rolling back across schema versions, restore the schema backup before the installation backup. None of these commands should delete state, worktrees, or uncommitted changes.
