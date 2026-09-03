# Worktree保持・cleanup・purge runbook

Issue worktreeはworker終了、`stop`、`unregister`、`uninstall`では削除しない。長期運用時の整理は通常処理から分離した`cleanup`と`purge`だけで行う。

## 既定保持ポリシー

保持期間は安全な内部既定値であり、repositoryの`.agent-loop.yaml`には記載しない。completedは7日、failedは30日、blockedとneeds-inputは無期限に保持する。continuationを持つ待機状態とrunning、claimed、retry中などの非terminal状態は期間にかかわらず保持する。期間の起点は永続stateのIssue `updated_at`である。

## Cleanup

最初にread-only previewを実行する。

```sh
agent-loop cleanup --repo /absolute/path/to/repository --json
```

各`entries[]`はIssue、status、path、経過時間、適用予定action、保持理由、Git安全性、復元元、`purge_confirmation`を返す。期限切れでも次のどれかに該当すれば`eligible: false`となり、自動削除しない。

- dirty worktree
- local HEADとremote branchが一致しない、またはbaseより先行する未push commit
- open PR
- 未回答request
- blocked、needs-input、resume-pendingまたは非terminal状態
- path不在、不正なworktree、`updated_at`欠落、保持期間内

previewに問題がなければloopを停止し、同じ条件を再検査して適用する。

```sh
agent-loop stop --repo /absolute/path/to/repository --json
agent-loop cleanup --repo /absolute/path/to/repository --apply --json
agent-loop start --repo /absolute/path/to/repository --json
```

`--apply`はLaunchAgentが停止していない場合に拒否する。削除対象は`git worktree remove`で外し、local branchを残したまま`git worktree prune`を実行する。削除前に`worktree_cleanup_started`、成功後に`worktree_cleaned`をevent logへ記録し、stateのworktree pathだけを空にする。

## Purge

dirtyや未push、open PR、未回答requestを含むworktreeを強制的に外す必要がある場合だけ使う。`cleanup`が対象Issueごとに返した完全一致tokenを指定する。

```sh
agent-loop stop --repo /absolute/path/to/repository --json
agent-loop purge \
  --repo /absolute/path/to/repository \
  --issue 42 \
  --confirm '<repo-id>:issue-42' \
  --json
```

確認tokenの省略・不一致、loop稼働中、対象worktree不在、検査失敗はすべて拒否する。purgeは`git worktree remove --force`を使うため、未commit変更は復元できない。local branchは削除しないのでcommit済み内容は再作成できる。`recoverable: false`の場合は完全復元できない情報があることを示す。実行前に必要なfileを別pathへ退避し、PR、remote branch、local branch、backupを確認する。

監査eventは`worktree_purge_started`、`worktree_purged`、失敗時は`worktree_purge_failed`である。cleanup/purgeがlocal branchやremote branchを削除することはなく、force pushも行わない。
