---
name: self-repair
description: Directly repair this codex-issue-loop repository from a normal Codex session when its own agent-loop runtime cannot safely process the repair. Use only when the user explicitly invokes $self-repair and directs Codex not to delegate the change to agent-loop. Never invoke this skill implicitly from a suspected or observed failure.
---

# Self Repair

Use this repository-scoped break-glass workflow only after the user explicitly invokes
`$self-repair` and requests direct implementation without agent-loop. Treat that invocation as
an explicit request to create the Codex goal required by this workflow. Do not create an
implementation Issue for the same request.

Establish the repair goal immediately after reading this skill. Do not inspect the repository,
run a command, or take any other workflow step first.

## Establish the goal

1. Call `get_goal` as the first action.
2. If no unfinished goal exists, call `create_goal` with an objective that names the defect, the
   authorized delivery scope, the required verification, and the intended restored state. Omit
   `token_budget` unless the user explicitly supplied one.
3. If the active goal describes this repair, continue it instead of creating another goal. If an
   unrelated unfinished goal exists, do not replace it; stop and ask the user how to proceed.
4. Do not mutate repository or external state when goal tooling is unavailable or the repair goal
   cannot be established.

Do not continue to break-glass eligibility checks until this gate succeeds.

On every resumed or automatically continued turn, call `get_goal` and re-read live Git, process,
queue, and delivery state before acting. The goal is a continuation anchor, not operational
evidence.

## Confirm break-glass eligibility

1. Resolve the repository root with Git and keep every path and command scoped to that root.
2. Read the repository instructions, `.agent-loop.yaml`, and `docs/break-glass-repair.md` before
   acting.
3. Collect the runbook's read-only evidence when the installed CLI remains usable. Preserve exact
   diagnostic codes and command failures.
4. Continue only when the defect affects codex-issue-loop's ability to accept, schedule, execute,
   publish, update, or safely recover the same repair. If the evidence shows that the ordinary
   Issue loop can safely perform the work, stop and report that break-glass is not eligible.

Do not infer direct-implementation authority from a failure alone. Explicit invocation and a
qualifying self-hosting failure are both required.

## Quiesce the affected runtime

Follow `docs/break-glass-repair.md` as the repository authority.

- Record the initial delivery, supervisor, worker, and worktree state.
- Wait for `active_workers=0`; never terminate an active implementation worker to begin repair.
- Confirm the exact impact with the user before stopping a controller or supervisor unless that
  stop was already explicitly requested.
- Prefer the typed CLI stop. Use `scripts/break-glass-stop.sh` only when the installed CLI is
  unusable and its exact repository-ID and LaunchAgent checks succeed.
- Never hand-edit or delete durable state, registry entries, active execution, continuations,
  sessions, managed worktrees, delivery configuration, or backups.

## Implement the repair

1. Inspect the worktree before editing. Preserve unrelated user changes and do not proceed from a
   dirty or ambiguous checkout that cannot be isolated safely.
2. Work on a clean `codex/*` branch or clean dedicated worktree.
3. Reproduce the failure and add or identify a test that fails for the demonstrated cause.
4. Make the smallest coherent change that fixes that cause. Do not add speculative recovery,
   compatibility, retry, fallback, persistence, configuration, or unrelated refactoring.
5. Run the focused test first, then the repository checks required by the runbook for the
   authorized delivery scope.
6. Review the final diff once and remove changes that are not required by the repair.

Do not commit, push, create or merge a Pull Request, tag a release, apply an assignment, or restart
services unless the user requested that scope or separately authorized it.

## Complete or block the goal

- If the goal covers only a tested patch or Pull Request, complete it when that deliverable and
  its required verification are finished.
- If the goal covers operational restoration, complete it only after the requested release and
  assignment steps succeed, scoped doctor/status/assignment verification passes, and controllers
  are returned to the intended state.
- Call `update_goal` with `complete` only when every stated completion condition is satisfied.
- Call `update_goal` with `blocked` only after the same blocking condition has repeated for at
  least three consecutive goal turns and no safe in-scope progress remains.
- When blocked or awaiting authorization, preserve the current state and report the exact
  evidence, completed work, remaining action, and safe resumption point.
