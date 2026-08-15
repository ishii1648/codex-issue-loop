# テストマトリクス

## 実行方法

```sh
make test
make fault-test
make test-race
```

`make test`は通常suite、`make fault-test`は `TestFault` prefixを持つ障害注入・復旧suite、`make test-race`は全packageをrace detector付きで実行する。GitHub Actionsでは3つを独立stepとして実行する。

## 仕様17.2との対応

| 統合テスト要件 | 主なテストケース |
|---|---|
| fake GitHub adapter + fake Codex process | `TestFaultStandardWorkerCompletesWithoutAdditionalRun`、`TestFaultFakeCodexProcessProducesStructuredResult` |
| worktree作成、再利用、異常終了 | `TestFaultWorktreeCreateReuseAndPartialCreation` |
| supervisor二重起動防止 | `TestFaultSecondSupervisorCannotAcquireLock` |
| snapshot途中書き込みからの復旧 | `TestFaultSnapshotWriteCrashRecoversEveryTransactionPoint`、`TestFaultPartialEventTailIsTruncatedAndRecorded` |
| worker kill後のreconciliation | `TestFaultWorkerKillReturnsRecoverableProcessError`、`TestFaultWorkerAndGitHubStateReconciliationDecisions` |
| watchの接続、切断、複数接続 | `TestFaultDisconnectedEventChannelsFallBackToTimer`、`TestFaultMultipleWatchConnectionsObserveSameRevision` |
| event通知を破棄した場合のreconciliation | `TestFaultDroppedEventReconcilesAttention` |
| read-subscribe-read間に状態が変わるrace | `TestFaultReadSubscribeReadRace` |
| attention状態と`state_revision`の永続化 | `TestFaultAttentionRevisionPersistsSnapshotAndEvent`、`TestFaultAttentionRemainsStickyUntilAnswered` |
| standard workerが追加runなしで完了 | `TestFaultStandardWorkerCompletesWithoutAdditionalRun` |
| extended workerだけが設定上限内でresume | `TestFaultExtendedWorkerResumesOnlyWithinConfiguredLimit` |

## 追加の部分障害と境界

| 対象 | 主なテストケース |
|---|---|
| supervisor kill後の永続状態再利用 | `TestFaultSupervisorRestartResumesWithDurableAnswers` |
| GitHub label/comment同期の途中停止 | `TestFaultPartialLabelCommentSyncCanBeRetried`、`TestFaultGitHubSyncPartialFailureIsRetried` |
| push後に未記録のPR | `TestFaultStartupReconciliationPersistsDiscoveredPullRequest` |
| registry add/resolve/remove | `TestFaultRegistryAddResolveRemoveAndAmbiguity` |
| atomic fileとmarshal失敗 | `TestFaultAtomicWriteReplacesContentAndPreservesMode`、`TestFaultJSONMarshalFailureDoesNotCreateDestination` |
| layout isolationとpermission | `TestFaultLayoutUsesIsolatedRootsAndPrivateDirectories` |
| GitHub CLI response破損 | `TestFaultGitHubAdapterRejectsMalformedResponse` |
| 回復不能なsnapshot/event不整合 | `TestFaultRevisionMismatchIsQuarantined`、`TestFaultCorruptSnapshotIsQuarantined` |

障害注入suiteは外部GitHubやCodex認証を必要とせず、一時directory、fake executable、local Git repositoryだけを使用する。固定sleepで順序を作らず、hook、channel、context、永続状態の予定時刻を使って同期する。
