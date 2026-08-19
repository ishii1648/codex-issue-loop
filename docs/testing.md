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
| repository phase gateの失敗・cancel解放、同一base SHAからの複数実worktree、PR作成後retryの冪等性 | `TestGateReleasesAfterFailureAndCancelsWaiter`、`TestMultipleRealWorktreesUseOneImmutableDispatchBase`、`TestPublishRetryReusesPullRequestCreatedBeforeCommandFailure` |
| supervisor二重起動防止 | `TestFaultSecondSupervisorCannotAcquireLock` |
| snapshot途中書き込みからの復旧 | `TestFaultSnapshotWriteCrashRecoversEveryTransactionPoint`、`TestFaultPartialEventTailIsTruncatedAndRecorded` |
| worker kill後のreconciliation | `TestFaultWorkerKillReturnsRecoverableProcessError`、`TestFaultWorkerAndGitHubStateReconciliationDecisions` |
| timeoutの段階的終了とprocess group回収 | `TestFaultWorkerTimeoutUsesGracefulProcessGroupTermination`、`TestFaultWorkerTimeoutForceKillsEntireProcessGroupAfterGrace`、`TestWorkerTimeoutStageIsPersistedForRetry` |
| 複数workerのstop・orphan回収 | `TestSchedulerCancellationStopsAllWorkers`、`TestStopWorkersTerminatesAndRecordsEveryIssueIndependently`、`TestStopWorkersRejectsUnownedProcessGroupWithoutMutatingIssue` |
| concurrency 2の同時result barrier | `TestFaultSchedulerConcurrentResultBarrier` |
| terminal Issueの通常reconciliationとworker継続 | `TestTerminalPullRequestReconciliationRequiresAuthoritativeSavedMerge`、`TestPeriodicTerminalReconciliationCompletesAndIsIdempotent`、`TestFaultSchedulerReconcilesTerminalIssueWithoutStoppingRunningWorker` |
| same-resourceの同時予約競合 | `TestFaultConcurrentLeaseReservationsNeverOverlapResources` |
| 実processのstop・restart・orphan回収 | `TestFaultRealProcessStopRestartLeavesNoOrphanAndRetainsLeases` |
| watchの接続、切断、複数接続 | `TestFaultDisconnectedEventChannelsFallBackToTimer`、`TestFaultMultipleWatchConnectionsObserveSameRevision`、`TestFSNotifyMultipleWatchersWakeAndCanReconnect` |
| 複数requestの表示・個別回答 | `TestWatchReturnsEveryPendingRequestInRequestIDOrder`、`TestAnswerChangesOnlyTheRequestAndIssueNamedByRequestID`、`TestStatusSummarizesMultipleWorkersResourcesAndRequests` |
| Desktop監視の即時再表示・payload保持・冪等回答・watch再開 | `TestWatchAnswerReconnectRoundTripPreservesQuestionContract` |
| event通知を破棄した場合のreconciliation | `TestFaultDroppedEventReconcilesAttention` |
| watcher作成・購読失敗時のpolling-only fallback | `TestFaultWatcherSubscriptionFailureFallsBackToReconciliation` |
| fsnotify自己wakeとbacklogのcoalesce | `TestSchedulerFsnotifyWakeCannotBypassSupervisorRetryDeadline`、`TestSchedulerCoalescesSelfGeneratedWakeBacklog` |
| primary rate-limitの共有cooldown | `TestCLIPrimaryGraphQLRateLimitUsesRESTRateLimitReset`、`TestSchedulerSharesPrimaryRateLimitCooldownAcrossRepositories`、`TestStoreSharesAndAtomicallyCountsSuppressedRetries` |
| E2E LaunchAgent cleanup | `TestE2ESupervisorCleanupOnSuccessFailureSignalAndTimeout` |
| read-subscribe-read間に状態が変わるrace | `TestFaultReadSubscribeReadRace` |
| attention状態と`state_revision`の永続化 | `TestFaultAttentionRevisionPersistsSnapshotAndEvent`、`TestFaultAttentionRemainsStickyUntilAnswered` |
| standard workerが追加runなしで完了 | `TestFaultStandardWorkerCompletesWithoutAdditionalRun` |
| extended workerだけが設定上限内でresume | `TestFaultExtendedWorkerResumesOnlyWithinConfiguredLimit` |
| fake App ServerのGoal・approval・token・time budget・input・steer契約 | `TestCodexAppServerExtendedContract`、`TestCodexAppServerGoalTimeBudgetIsPersistedAsTerminal`、`TestCodexAppServerConvertsRequestUserInput`、`TestCodexAppServerUsesSteerForRejoinedActiveTurn` |
| App Server切断時のstate保全と非対応fallback | `TestCodexAppServerDisconnectAfterTurnStartDoesNotFallback`、`TestCodexAppServerConnectionFailureFallsBackToExecResume`、`TestBackendFactoryEnablesGoalOnlyWhenConfiguredAndSupported` |
| event rotation後のsequence復旧 | `TestFaultEventRotationKeepsCheckpointAndRecoverySequence` |
| disk容量reserveでのblocked化 | `TestFaultDiskSafetyReserveBlocksSupervisor` |
| localhost-only configの閉じたallowlistと不整合拒否 | `TestLoadLocalhostOnlyCommandNetworkIsClosedAndOptIn` |
| Codex proxy/tool隔離argvとcapability検出 | `TestCodexLocalhostNetworkArgumentsAreFailClosed`、`TestCodexProbeDetectsLocalhostNetworkProxyCapability` |
| worker環境blockedのlease park、continuation保持、後続`repo:*` queue継続 | `TestWorkerEnvironmentBlockParksLeaseAndPreservesContinuationState`、`TestWorkerEnvironmentBlockParkAllowsFollowingRepositoryIssue`、`TestParkedLeaseReleasesAdmissionAndResumeUsesNewGeneration` |
| 既存typed blockのstartup parkとGitHub block同期crash冪等性 | `TestStartupReconciliationParksExistingTypedEnvironmentBlock`、`TestFaultWorkerEnvironmentParkSurvivesGitHubSyncCrashIdempotently` |
| needs-input park、回答provenance、競合中のdurable answer、解放後の1回だけの再取得 | `TestRunOncePersistsQuestion`、`TestAnswerDurablyWaitsWithoutStealingConflictingLease`、`TestAnsweredNeedsInputClaimWaitsThenReacquiresOnce` |
| park済みoperator resumeの競合拒否・新generation・dirty/session/Goal/answer保持・GitHub同期crash冪等性 | `TestFaultResumeBlockedReacquiresParkedLeaseOnceAcrossGitHubSyncFailure`、`TestFaultConcurrentParkedLeaseResumeCreatesOneFencedOwner`、`TestEnvironmentResumeContinuesSameSessionAndWorktree` |
| park/legacy resumeのfail-closed・typed legacy lost lease回復 | `TestResourceParkValidationFailsClosed`、`TestResumeBlockedEnvironmentPreservesWorktreeBranchSessionAndDirtyChanges`、`TestTypedLegacyWorkerBlockRecoveryFromMissingLeaseFixture`、`TestTypedLegacyWorkerBlockRequiresExactDurableCause`、`TestLegacyWorkerBlockRecoveryRequiresSameRunLeaseWorktreeAndBranch`、`TestFaultResumeBlockedRecoversLeaseLostByInterruptedReconciliation`、`TestResumeBlockedRejectsUnconfirmedAndNonEnvironmentBlocks` |
| exact v0.6.14 missing-Workspace interrupted resume（全27 events、running status、owner/slotなしlegacy request、resume IDなし→ありsync）の限定回復・改変拒否・generation 2→3・並行/crash fence・same-worktree spawn | `TestInterruptedWorkspaceResumeEvidenceFromZeitreise442Full27EventFixture`、`TestInterruptedWorkspaceResumeEvidenceRetainsSyntheticShortFixtureCompatibility`、`TestInterruptedWorkspaceResumeCandidateFailsClosedForOtherSupervisorBlocks`、`TestInterruptedWorkspaceResumeEvidenceRejectsTamperedOrReorderedHistory`、`TestInterruptedWorkspaceResumeEvidenceRejectsCurrentStateMismatches`、`TestFaultZeitreise442Full27EventHistoryBackfillsAndSpawnsSameWorktree` |
| 手動merge済みPR adoptionのfail-closed検証・lease解放・冪等性 | `TestValidateMergedPullRequestAdoptionFailsClosed`、`TestAdoptMergedPullRequestReleasesLeaseAndIsIdempotent` |

## 追加の部分障害と境界

| 対象 | 主なテストケース |
|---|---|
| supervisor kill後の永続状態再利用 | `TestFaultSupervisorRestartResumesWithDurableAnswers` |
| GitHub label/comment同期の途中停止 | `TestFaultPartialLabelCommentSyncCanBeRetried`、`TestFaultGitHubSyncPartialFailureIsRetried` |
| merged PR adoptionのdone同期前停止とsupervisor再起動 | `TestRestartCompletesRequestedMergedPullRequestAdoption` |
| environment resume保存とstartup/periodic reconciliationの競合 | `TestFaultStartupReconciliationDoesNotOverwriteConcurrentEnvironmentResume`、`TestFaultWebhookReconciliationDoesNotOverwriteConcurrentEnvironmentResume` |
| park resumeのresource/slot raceと二重owner防止 | `TestFaultConcurrentParkedLeaseResumeCreatesOneFencedOwner`、`TestStatusSummarizesMultipleWorkersResourcesAndRequests` |
| answered missing-Workspace exact chainのtransaction/GitHub faultと並行fence | `TestFaultRecoverAnsweredWorkspaceRetriesGitHubBoundaryWithoutRefencing`、`TestRecoverAnsweredWorkspaceParallelInvocationsFenceOnce` |
| checks retry exhaustion後の外部head修正・fail-closed復旧・merge時lease解放 | `TestRecoverChecksReusesExternallyFixedBranchAndIsIdempotent`、`TestRecoverChecksAuthoritativeStateValidationFailsClosed`、`TestPullRequestChecksRecoveryResumesSamePRAndReleasesLeaseOnlyAfterMerge` |
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
| host delivery config・release検証・drain・rollback | `TestConfigSecureAtomicWriteAndValidation`、`TestConfigRejectsSymlinkAndRelativeOverride`、`TestVerifierChecksEveryBoundaryBeforeExecutingCandidate`、`TestCompatibilityBlocksMajorSchemaDowngradeAndRetag`、`TestDeliveryMaintenanceFenceDrainsWithoutDispatchOrCancellation`、`TestFaultControllerApplyAndDoctorFailureRollback` |
| SPDX SBOMの決定性 | `TestGenerateProducesDeterministicSPDXDocument` |
| v1→v2 migration・backup restore・途中停止再開 | `TestApplyMigratesV1FixturesAndRestoreRecoversOriginalBytes`、`TestInterruptedApplyReusesJournalAndConvergesIdempotently`、`TestUnsupportedVersionIsRejectedWithoutBackup`、`TestSchemaChangingUpdateRequiresStoppedMigrationAndPairedRollback` |
| worktree cleanup/purge・安全条件・監査 | `TestCleanupRetainsUnsafeWorktreesAndAuditsSafeRemoval`、`TestPurgeRequiresExactConfirmationAndCanRemoveDirtyWorktree` |

障害注入suiteは外部GitHubやCodex認証を必要とせず、一時directory、fake executable、local Git repositoryだけを使用する。固定sleepで順序を作らず、hook、channel、context、永続状態の予定時刻を使って同期する。

`internal/recoveryfixture/testdata/zeitreise-442-full-history-v1.json`は、zeitreise #442のproduction recordを由来とする統合release-gate fixtureである。旧`internal/state/testdata/zeitreise-442-v0614-full-27-*`のexact historyを移行し、27 eventsのtype/order/payload shape、session null、original generation 1→legacy recovery generation 2、resume timestamp差、remote field evolution、GitHub marker cardinalityを一つのmanifest/hash付きfixtureで保持する。公式`resume-blocked` fault testはこのbundleを直接replayしてproduction predicateへ入力する。旧12-event synthetic short fixtureは互換性確認にだけ残し、その成功だけではrelease gateを満たさない。export、sanitization、review手順は[production recovery fixture runbook](recovery-fixtures.md)を参照する。

`internal/state/testdata/zeitreise-449-v0622-answered-missing-workspace-{state.json,events.jsonl}`は#449由来のsanitized exact fixtureであり、generation 1 lease、claim/worker、needs-input park、単一answerとgeneration 2再取得、全validator check成功のmissing-Workspace rejection、blocked同期までの11 eventsを固定する。テストではその直後に単一verified `workspace_provenance_recovered`を加えた12-event caseも構築し、専用dry-run/confirm、generation 3単発fence、generic audit保持、event/run/status/branch/repository/HEAD/fingerprint/validator fault、GitHub同期fault、競合、冪等性をreal linked-worktreeで検証する。fixture単体の成功だけをrelease authorityにしない。

Codex Desktopのquestion notification、macOS通知権限、Activityの回答待ち、pinした`codex-issue-loop` / `zeitreise`監視chatの責務分離はrepository内の自動testでは再現しない。[Codex Desktop監視task運用](codex-desktop-monitoring.md)の実機受け入れ手順で検証する。

Codex network proxy、macOS sandbox、Deno listen/connect、spawnしたChrome CDP、親/子processのpublic/LAN/link-local拒否は認証済みrelease hostだけで[localhost-only command network](localhost-network.md)の実機受け入れを行う。通常suiteはmodel呼び出しや外部networkを使わない。

concurrency 2のfault matrix、self-hosting canary、resource計測、concurrency 1 rollbackは[concurrency 2 rollout・rollback runbook](concurrency-rollout.md)を正本とする。

## セキュリティ負テスト

| 境界 | 主なテスト |
|---|---|
| Prompt injection・巨大入力・制御文字 | `TestPromptTreatsIssueInjectionAsBoundedData`、`TestIssueInputIsBoundedAndControlCharactersAreRemoved` |
| 既知token・設定secret・private key | `TestConfiguredSecretIsRedactedFromTextAndJSON`、`TestLineWriterRedactsPrivateKeyBlock`、`TestWorkerArtifactsNeverPersistSecrets` |
| state・event・file mode | `TestStateAndEventsNeverPersistSecrets`、`TestWritePlistUsesAbsoluteCommandsAndEscapesPaths` |
| GitHubコメント・CLI error | `TestGitHubCommentsAndErrorsRedactSecrets` |
| path traversal・symbolic link | `TestWorktreeRejectsTraversalAndSymbolicLink`、`TestLoadRejectsUnsafePathsRefsAndSecretNames` |
| continuation worktree provenance・legacy backfill・dirty/behind main checkout隔離・spawn cwd監査 | `TestFaultWorktreeCreateReuseAndPartialCreation`、`TestRetryContinuationKeepsDirtyBehindMainCheckoutUntouched`、`TestContinuationFailsClosedWhenSavedWorkspaceProvenanceChanges`、`TestFaultResumeBlockedBackfillsMissingWorkspaceProvenanceForDirtyBehindManagedWorktree`、`TestWorkerProcessCallbackFencesRunAndPersistsProcessGroup` |
| schema v3の旧配送data除去・active lease/parked continuationのrollback拒否・旧credential保持 | `TestApplyMigratesV3FixturesAndRestoreRecoversOriginalBytes`、`TestV4ActiveLeaseAndParkedContinuationBlockRollback` |

既知の到達可能な依存脆弱性は`make vuln-check`で検査し、Pull Requestと`main`のCIで必須にする。
