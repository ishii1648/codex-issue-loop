# concurrency 2 rollout・rollback runbook

最終更新日: 2026-08-17

このrunbookは、単一host・単一repositoryのworker poolを`queue.concurrency: 1`から`2`へ段階的に切り替え、self-hosting repositoryでcanaryし、異常時にschemaやleaseを壊さず`1`へ戻す手順を定める。複数hostの排他は対象外である。

## 1. 自動検証gate

rollout対象commitで次をすべて成功させる。

```sh
make test
make fault-test
make test-race
```

GitHub Actionsの`Quality gates`は通常suite、`TestFault` suite、`go test -race ./...`を別stepで実行する。固定sleepでworkerの完了順を作らず、channel barrier、固定clock、永続transaction、実processのreadiness pipeを同期点にする。

| 境界 | 自動検証 |
|---|---|
| selector上限・safe backfill | `TestSelectTable`、`TestSchedulerBoundsWorkersAndAdmitsAfterSlotRelease` |
| same-resource非交差 | `TestRepositoryLeaseConflictsWithEveryResource`、`TestFaultConcurrentLeaseReservationsNeverOverlapResources` |
| unknown/exclusive Issueの単独実行 | `TestSelectTable/missing_metadata_is_exclusive`、`TestMetadataAndClaimFallbackPriority` |
| 2 worker同時完了・failure・片方needs-input | `TestFaultSchedulerConcurrentResultBarrier` |
| open PR lease・merge後の原子的解放 | `TestStartupReconciliationRetainsPRLeaseUntilMergeThenReleasesIt` |
| stop/restart・orphan process group | `TestFaultRealProcessStopRestartLeavesNoOrphanAndRetainsLeases`、`TestStartupReconciliationStopsAllOrphanGroupsBeforeInspectingIssues` |
| concurrency 1 fallback config | `TestSelfHostingCanaryAndConcurrencyOneFallbackConfigurations` |

障害注入点と期待する回復境界は次のとおりである。

| crash point | 保持するwrite-ahead state | 再開・冪等性の検証 |
|---|---|---|
| reserve transaction | `claiming`とresource leaseを同一transactionで保持 | `TestCrashPointsReplayPreparedLeaseTransaction`、`TestFaultSnapshotWriteCrashRecoversEveryTransactionPoint` |
| GitHub claim途中 | 同じrun IDの`claiming` lease | `TestFaultInterruptedClaimIsRecovered` |
| worktree作成途中 | partial directoryを再利用しない | `TestFaultWorktreeCreateReuseAndPartialCreation` |
| worker PID記録前後 | run IDでcallbackをfenceし、保存済みgroupを回収 | `TestWorkerProcessCallbackFencesRunAndPersistsProcessGroup`、`TestFaultRealProcessStopRestartLeavesNoOrphanAndRetainsLeases` |
| result永続化途中 | transaction replay後にIssueごとのstatusとleaseを復元 | `TestFaultSchedulerConcurrentResultBarrier`、`TestFaultSnapshotWriteCrashRecoversEveryTransactionPoint` |
| commit/push/PR publish途中 | resource audit、branch、既存PRを照合して重複publishしない | `TestPublishCommitsPushesAndCreatesDraftPullRequestIdempotently`、`TestFaultStartupReconciliationPersistsDiscoveredPullRequest` |

## 2. canary前preflight

依存変更が導入済みで、config/stateがv3であることを確認する。migrationが必要なら先に[migration runbook](migration.md)を完了し、migrationとconcurrency変更を同時に実施しない。

```sh
agent-loop status --repo /absolute/path/to/codex-issue-loop --json
agent-loop doctor --repo /absolute/path/to/codex-issue-loop --json
gh pr list --repo ishii1648/codex-issue-loop --state open
```

`status`で次を確認して記録する。

- `pending_requests`が0件である。未回答requestがあれば先に回答し、対応Issueのleaseを勝手に解放しない。
- worker PID/PGIDが想定どおりで、停止後に0になる。
- open PRごとのIssue、branch、lease、resourceが一意に対応する。
- `doctor`が`ok: true`であり、state recoveryやschemaの警告がない。
- canary用2 Issueにはvalidなmetadata、空または完了済みの依存、既知かつ非交差の`area:<resource>` labelがある。

open PRや`needs_input`があること自体は異常ではない。ただしcanary resourceと交差するleaseがあれば、mergeまたは回答による通常の収束を待つ。PR closeやlabel変更だけでleaseを手動削除しない。

## 3. resource budgetの測定

concurrency 1で通常Issueを1件処理したbaselineと、canaryで2件が同時に`running`になった時点を同じ観測方法で比較する。測定値やtokenをIssue本文・logへ貼らず、CPU、RSS、disk使用量、Codex利用枠の残量だけを運用記録へ残す。

```sh
ps -axo pid,ppid,pgid,%cpu,rss,vsz,etime,command | grep -E '[a]gent-loop|[c]odex'
df -k /absolute/path/to/codex-issue-loop
du -sk '/absolute/path/to/worktree-root' '/Users/name/Library/Application Support/codex-issue-loop'
memory_pressure
```

canary開始前にhost固有の上限を決め、baseline、peak、canary終了後を同じ表へ記録する。

| 指標 | baseline (concurrency 1) | concurrency 2 peak | 事前に決めた停止上限 | 結果 |
|---|---:|---:|---:|---|
| worker合計CPU | 未計測 | 未計測 | 運用者が設定 | 未実施 |
| worker合計RSS | 未計測 | 未計測 | 運用者が設定 | 未実施 |
| worktree/state disk増分 | 未計測 | 未計測 | 運用者が設定 | 未実施 |
| Codex利用枠の消費 | 未計測 | 未計測 | 契約枠内 | 未実施 |

memory pressure、swap、disk safety reserve、Codex rate/quotaのいずれかが上限へ達した場合は新規Issueを追加せず、[rollback](#6-concurrency-1へのrollback)へ進む。資格情報や利用枠の詳細値は公開artifactへ残さない。

## 4. 段階切替

本repositoryの`.agent-loop.yaml`はself-hosting canaryの上限を`2`とし、resource taxonomyを明示する。`2`を超える値へ直接上げない。

1. loopを停止し、`status`でworker PID/PGIDが0になったことを確認する。stopはleaseとworktreeを保持する。
2. 新artifactと`.agent-loop.yaml`を配置し、`doctor`を実行する。
3. まず`queue.concurrency: 1`で起動し、既存lease・request・open PRが同じ状態へreconcileすることを確認する。
4. 再度停止し、resource definitionsを維持したまま`queue.concurrency: 2`へ変更する。
5. `doctor`後に開始し、canary用の独立Issueを2件だけreadyにする。

```sh
agent-loop stop --repo /absolute/path/to/codex-issue-loop --json
agent-loop doctor --repo /absolute/path/to/codex-issue-loop --json
agent-loop start --repo /absolute/path/to/codex-issue-loop --json
agent-loop status --repo /absolute/path/to/codex-issue-loop --json
```

## 5. canary合格条件と記録

2 Issueが同時に`running`である瞬間を確認し、worker slotが`0`と`1`、effective resource集合が非交差であることを記録する。その後、両方についてdraft PRが各1件だけ作成され、open PR中はleaseが保持され、merge確認と`completed`が同じ収束でleaseを解放することを確認する。

| 項目 | 記録 |
|---|---|
| 実施日時・artifact commit | 未実施 |
| canary Issue 2件 | 未実施 |
| slot/resource非交差 | 未実施 |
| 各Issueのworker/result/PRが1件 | 未実施 |
| open PR中のlease保持 | 未実施 |
| merge後のlease解放 | 未実施 |
| stop/restart後の重複なし | 未実施 |
| resource budget結果 | 未実施 |

この表の`未実施`を実測結果へ更新するまで、self-hosting live canaryは完了扱いにしない。自動test suiteの成功はlive canaryの代替ではない。

## 6. concurrency 1へのrollback

rollbackはschemaを戻さず、resource definitionsも削除しない。worker上限だけを`1`へ戻すため、active lease、open PR、needs-inputを失わない。

1. 新規ready投入を止める。
2. `status`を保存し、loopを`stop`する。
3. `.agent-loop.yaml`の`queue.concurrency`だけを`1`へ変更する。
4. `doctor`を実行し、同じv3 stateと全leaseが読めることを確認する。
5. loopを開始し、同時worker数が1以下で、既存Issueが重複worker/PRなしに収束することを確認する。

active leaseをv2へrollbackする必要はない。binary/schema rollbackも必要な場合は、すべてのactive leaseが通常のterminal遷移で解放された後に[migration runbook](migration.md)のpaired rollbackを別作業として行う。

## 7. 既知制約

- resource leaseは単一host内だけで有効であり、複数hostから同じrepositoryを処理してはならない。
- `repo:*`へ縮退したunknown/exclusive Issueは単独実行され、concurrency 2でも並列化されない。
- `needs_input`、retry、checks、open PRはworker slotを解放してもresource leaseを保持する。
- GitHub publicationはrepository単位で直列化されるため、workerが2件完了してもcommit/push/PR操作は同時実行しない。
- throughputより安全性を優先し、resource claim不足はpublish前監査で`needs_input`へ遷移する。
- 実際のCPU、memory、disk、Codex利用枠はIssue内容とhost状態に依存し、自動testだけでは上限を決められない。
