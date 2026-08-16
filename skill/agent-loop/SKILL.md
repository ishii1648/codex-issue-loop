---
name: agent-loop
description: Operate and monitor the codex-issue-loop supervisor from Codex. Use for starting, stopping, inspecting, watching, answering, or safely cleaning up a loop-managed GitHub Issue queue.
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
   - For `NOTIFICATION_CREDENTIAL_MISSING` or `NOTIFICATION_CREDENTIAL_UNSAFE`, explain that external push is opt-in. Configure it only when the user requests it, and read the token only with `agent-loop notification-token --repo <path> --token-file -`. Never put the token in chat, command arguments, repository files, config, or plist.
2. Run `agent-loop status --repo <path> --json` before mutating loop state.
3. Use `start`, `stop`, or `restart` only for the repository the user named.
4. If status contains a pending request, present that request before starting a new watch. Preserve its request ID, Issue number, question, reason, recommended option, every option ID and label, and whether free text is allowed. In Codex Desktop, ask it as a question that waits for the user's response so question notifications and Activity can surface it; do not finish with a progress-only summary.
5. Otherwise call `agent-loop watch --repo <path> --until-attention --json` once.
6. Do not implement a Codex-side polling loop. The Go watch process combines OS events with reconciliation polling internally.
7. When watch returns `needs_input`, preserve the request ID and all question fields described above, then ask the user. Never merge multiple requests or infer an answer for one request from another.
8. Record the answer exactly once with `agent-loop answer --repo <path> --request-id <id> --message-file -`, passing the answer through standard input rather than interpolating it into a shell command. Confirm with one `status --json` call that the named request is answered.
9. Return to one blocking watch call in the same monitoring task after the answer is recorded. After a task disconnect, Desktop restart, or Mac restart, begin again with `status --json`; re-present any durable pending request before reconnecting watch.

For normal Desktop operation, dedicate and pin one monitoring task per repository. Keep the repository path and every `--repo` argument fixed within that task. Do not multiplex repositories in one blocking monitor. OS notifications and Activity are discovery aids; the snapshot and GitHub Issue remain authoritative, and no new Activity item is guaranteed while the Desktop task is disconnected.

When an Issue is finally `blocked` after Pull Request conflict recovery, inspect `state.issues[<number>].conflict_recovery` and explain its attempts, base SHA history, conflict files, and last reason. Use `agent-loop retry --repo <path> --issue <number> --json` only when the user explicitly asks to retry that Issue and status confirms the blocked cause is a Pull Request conflict. Never emulate retry by editing labels or state files, and never create a replacement branch or Pull Request.

For worktree retention, run `agent-loop cleanup --repo <path> --json` first and present every candidate, reason, safety flag, recovery source, and purge confirmation token. `cleanup --apply` requires the named repository loop to be stopped and explicit user authorization. Never use `cleanup --apply` for an entry marked dirty, unpushed, open-PR, or unanswered-request; the CLI must also reject it. Use `purge` only for the single Issue the user explicitly authorized, copy the exact confirmation token from the current cleanup preview, and explain that dirty changes are not recoverable. Never infer or synthesize approval for purge.

Confirm the exact repository and impact before `stop`, `restart`, `unregister`, `update`, `migrate --apply`, either rollback, or `uninstall`. Use only a checksum- and attestation-verified release artifact for update. Use only the exact managed backup paths returned by update and migrate for rollback. When rolling back across schema versions, restore the schema backup before the installation backup. None of these commands should delete state, worktrees, or uncommitted changes.

External push is an attention hint, not durable state. After a notification, always read `status` and present the current pending request. Clearing a managed notification token requires explicit user authorization and notifications must be disabled or the loop stopped first.
