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
| timeoutの段階的終了とprocess group回収 | `TestFaultWorkerTimeoutUsesGracefulProcessGroupTermination`、`TestFaultWorkerTimeoutForceKillsEntireProcessGroupAfterGrace`、`TestWorkerTimeoutStageIsPersistedForRetry` |
| 複数workerのstop・orphan回収 | `TestSchedulerCancellationStopsAllWorkers`、`TestStopWorkersTerminatesAndRecordsEveryIssueIndependently`、`TestStopWorkersRejectsUnownedProcessGroupWithoutMutatingIssue` |
| concurrency 2の同時result barrier | `TestFaultSchedulerConcurrentResultBarrier` |
| same-resourceの同時予約競合 | `TestFaultConcurrentLeaseReservationsNeverOverlapResources` |
| 実processのstop・restart・orphan回収 | `TestFaultRealProcessStopRestartLeavesNoOrphanAndRetainsLeases` |
| watchの接続、切断、複数接続 | `TestFaultDisconnectedEventChannelsFallBackToTimer`、`TestFaultMultipleWatchConnectionsObserveSameRevision`、`TestFSNotifyMultipleWatchersWakeAndCanReconnect` |
| 複数requestの表示・個別回答 | `TestWatchReturnsEveryPendingRequestInRequestIDOrder`、`TestAnswerChangesOnlyTheRequestAndIssueNamedByRequestID`、`TestStatusSummarizesMultipleWorkersResourcesAndRequests` |
| Desktop監視の即時再表示・payload保持・冪等回答・watch再開 | `TestWatchAnswerReconnectRoundTripPreservesQuestionContract` |
| event通知を破棄した場合のreconciliation | `TestFaultDroppedEventReconcilesAttention` |
| watcher作成・購読失敗時のpolling-only fallback | `TestFaultWatcherSubscriptionFailureFallsBackToReconciliation` |
| read-subscribe-read間に状態が変わるrace | `TestFaultReadSubscribeReadRace` |
| attention状態と`state_revision`の永続化 | `TestFaultAttentionRevisionPersistsSnapshotAndEvent`、`TestFaultAttentionRemainsStickyUntilAnswered` |
| standard workerが追加runなしで完了 | `TestFaultStandardWorkerCompletesWithoutAdditionalRun` |
| extended workerだけが設定上限内でresume | `TestFaultExtendedWorkerResumesOnlyWithinConfiguredLimit` |
| fake App ServerのGoal・approval・token・time budget・input・steer契約 | `TestCodexAppServerExtendedContract`、`TestCodexAppServerGoalTimeBudgetIsPersistedAsTerminal`、`TestCodexAppServerConvertsRequestUserInput`、`TestCodexAppServerUsesSteerForRejoinedActiveTurn` |
| App Server切断時のstate保全と非対応fallback | `TestCodexAppServerDisconnectAfterTurnStartDoesNotFallback`、`TestCodexAppServerConnectionFailureFallsBackToExecResume`、`TestBackendFactoryEnablesGoalOnlyWhenConfiguredAndSupported` |
| event rotation後のsequence復旧 | `TestFaultEventRotationKeepsCheckpointAndRecoverySequence` |
| disk容量reserveでのblocked化 | `TestFaultDiskSafetyReserveBlocksSupervisor` |

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
| queue strategy・tie-break・pagination後sort | `TestOrderIssuesSupportsCreatedAtAndPriorityWithStableTieBreaks`、`TestListReadyOrdersAfterCollectingPaginatedFixture`、`TestSelectReadyAppliesChangedOrderOnlyToUnclaimedIssues` |
| GitHubラベルのpreview・冪等作成・部分成功 | `TestBootstrapLabelsPreviewsCreatesAndPreservesExistingMetadata`、`TestBootstrapLabelsIsIdempotentWhenEveryLabelExists`、`TestFaultBootstrapLabelsReportsPartialSuccessAndCanBeRerun` |
| doctorの安定code・認証・sleep・state・停止理由 | `TestDoctorOutputHasStableSchemaCodesAndSafeRemediations`、`TestFaultDoctorHostAuthAndSleepFixturesHaveUniqueCodes`、`TestFaultDoctorDetectsCorruptStateWithoutModifyingIt`、`TestDoctorCorrelatesBlockedAndStoppedStateWithEventAndLog` |
| 回復不能なsnapshot/event不整合 | `TestFaultRevisionMismatchIsQuarantined`、`TestFaultCorruptSnapshotIsQuarantined` |
| log世代上限とworker run保持 | `TestLongRunningWriterKeepsBoundedGenerations`、`TestWorkerRunLogPruningPreservesActiveAndAuditsDeletion` |
| install manifest・update backup・rollback | `TestInstallArtifactsAreIdempotentAndVersioned`、`TestUpdateBackupCanRestoreBinarySkillAndManifest`、`TestDoctorDetectsInstalledBinaryAndSkillMismatch` |
| SPDX SBOMの決定性 | `TestGenerateProducesDeterministicSPDXDocument` |
| v1→v2 migration・backup restore・途中停止再開 | `TestApplyMigratesV1FixturesAndRestoreRecoversOriginalBytes`、`TestInterruptedApplyReusesJournalAndConvergesIdempotently`、`TestUnsupportedVersionIsRejectedWithoutBackup`、`TestSchemaChangingUpdateRequiresStoppedMigrationAndPairedRollback` |
| worktree cleanup/purge・安全条件・監査 | `TestCleanupRetainsUnsafeWorktreesAndAuditsSafeRemoval`、`TestPurgeRequiresExactConfirmationAndCanRemoveDirtyWorktree` |
| push通知のoutbox・重複抑止・再送・本体分離 | `TestDispatcherDeduplicatesRetriesAndRedactsFailures`、`TestDispatcherCancelsAnsweredRequestBeforeDelivery`、`TestNotificationFailureDoesNotStopSupervisor` |

障害注入suiteは外部GitHubやCodex認証を必要とせず、一時directory、fake executable、local Git repositoryだけを使用する。固定sleepで順序を作らず、hook、channel、context、永続状態の予定時刻を使って同期する。

Codex Desktopのquestion notification、macOS通知権限、Activityの回答待ち、pinした`codex-issue-loop` / `zeitreise`監視chatの責務分離はrepository内の自動testでは再現しない。[Codex Desktop監視task運用](codex-desktop-monitoring.md)の実機受け入れ手順で検証する。

concurrency 2のfault matrix、self-hosting canary、resource計測、concurrency 1 rollbackは[concurrency 2 rollout・rollback runbook](concurrency-rollout.md)を正本とする。

## セキュリティ負テスト

| 境界 | 主なテスト |
|---|---|
| Prompt injection・巨大入力・制御文字 | `TestPromptTreatsIssueInjectionAsBoundedData`、`TestIssueInputIsBoundedAndControlCharactersAreRemoved` |
| 既知token・設定secret・private key | `TestConfiguredSecretIsRedactedFromTextAndJSON`、`TestLineWriterRedactsPrivateKeyBlock`、`TestWorkerArtifactsNeverPersistSecrets` |
| state・event・file mode | `TestStateAndEventsNeverPersistSecrets`、`TestWritePlistUsesAbsoluteCommandsAndEscapesPaths` |
| GitHubコメント・CLI error | `TestGitHubCommentsAndErrorsRedactSecrets` |
| path traversal・symbolic link | `TestWorktreeRejectsTraversalAndSymbolicLink`、`TestLoadRejectsUnsafePathsRefsAndSecretNames` |
| 通知credential・本文最小化・外部error | `TestNotificationTokenCommandStoresPrivateCredentialWithoutOutputLeak`、`TestLoadNotificationTokenRejectsUnsafePermissions`、`TestNtfySendsAuthenticatedMinimalNotification`、`TestDispatcherDeduplicatesRetriesAndRedactsFailures` |

既知の到達可能な依存脆弱性は`make vuln-check`で検査し、Pull Requestと`main`のCIで必須にする。
