# codex-issue-loop break-glass repair

この手順は`codex-issue-loop`自身のruntimeが壊れ、agent-loopによるIssue実装を安全に利用できない場合だけ使う。通常Codexセッションからclean worktreeへ直接patchし、stable patch Releaseを作る。

## 1. Read-only evidence

可能なら先に次を保存し、`active_workers`が0になるまで待つ。

```sh
agent-loop delivery assignment status --repo /absolute/path/to/codex-issue-loop --json
agent-loop status --repo /absolute/path/to/codex-issue-loop --json
agent-loop doctor --repo /absolute/path/to/codex-issue-loop --json
git -C /absolute/path/to/codex-issue-loop status --short
```

runtime停止が修復に必要な場合だけ以下を行う。workflowや手順の変更だけなら稼働状態を維持し、release公開・取得・検証のために停止しない。正常にdrainできるruntimeへの配備は`delivery assignment apply`の切替直前drainを使う。

## 2. 通常のtyped stop

delivery controllerを停止してから対象repositoryだけを停止する。通常の`agent-loop stop`はdeliveryと共通のdurable drain契約を使い、active workerへsignalを送らずcheckpointまで待つ。期限切れ時は通常運転へ戻るため、transactionやfenceを削除しない。

```sh
launchctl bootout "gui/$(id -u)/com.codex-issue-loop.delivery"
agent-loop stop --repo /absolute/path/to/codex-issue-loop --json
```

## 3. installed CLIが壊れている場合

fallbackはregistryに存在するexact repository IDと、Labelが一致するregular plistを再検証し、delivery LaunchAgentと対象LaunchAgentだけをbootoutする。

```sh
scripts/break-glass-stop.sh --repo-id '<exact-repository-id>'
```

このscriptはdurable state、registry、active execution、continuation、session、worktree、delivery configを変更しない。他repositoryのLaunchAgentを停止しない。入力ID、registry、plistのどれかが一致しなければ何も推測せず失敗する。

## 4. PatchとRelease

1. cleanな`codex/*` branchで原因と再現testを確定する。
2. focused testで修正を確認し、同じcommitのCIで全量検証を確認する。CIを使えない場合は`make ci`を1回実行する（全体test、race、fault、conformance、snapshot invariant、vet、release checkを含む）。各suiteを別途重複実行しない。
3. PRをmergeし、annotated stable patch tagを作る。
4. candidateとstableのSHA-256 byte一致、attestation、manifestを確認する。
5. `delivery assignment preview/apply`で`codex-issue-loop`だけをpatchへ更新する。
6. scoped doctor、status、assignment verifyの成功後にdelivery controllerを再開する。

state、registry、active execution、continuation、session、managed worktree、backupを手編集または削除しない。これらの修復が必要なら通常のtyped recovery runbookへ戻る。
