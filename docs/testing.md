# テストマトリクス

## 実行方法

```sh
make test
make fault-test
make test-race
scripts/check-release.sh
```

通常suite、`TestFault`障害注入suite、race detector、release gateを独立して成功させる。外部GitHub APIやCodex inferenceを使うcontract testは置かず、local bare Git remote、fake GitHub/worker、fixture replay、隔離したHOMEで再現する。

## 中核ドメイン契約

| 契約 | 主な検証 |
| --- | --- |
| Issue lifecycle APIの許可遷移とmajor互換性 | `internal/domain/issue`、`internal/application/conformance` |
| root active executionが常に0/1件 | `TestConcurrentExecutionStartsHaveSingleWinner`、snapshot validator suite |
| Issue番号・run ID・generationのfence | `internal/adapter/state/execution_test.go` |
| waiting・terminal・quarantineで実行枠解放 | lifecycle boundary、supervisor reconciliation suite |
| needs-input回答後の同一continuation再開 | `TestRunOncePersistsQuestion`、`TestAnswerDurablyWaitsWithoutStealingActiveExecution` |
| 1 Issueの失敗・入力待ち・PR/check待ち後も後続を取得 | scheduler fault/conformance suite |
| 作成者がtrusted ownerであるIssueだけを受理 | `internal/adapter/github/author_test.go`、`internal/domain/queue`、scheduler author verification suite |
| root pending effectによるGitHub副作用の冪等性 | publication、GitHub sync、partial failure suite |
| stop/restartとorphan process回収 | process controller、scheduler cancellation、fault suite |
| worktree provenance不一致をspawn前に拒否 | worktree validation、issue resolution suite |

## Migration・互換性

production由来のsanitized v4 fixtureはmigration decoderの入力としてだけ保持する。release gateは11 Issue・14旧substateをroot `active_execution`、Issue-local `continuation`、`continuation_evidence`、`suspension`へ変換し、Issue・answer・audit・generationの欠損/重複が0であることを検査する。

current v5入力に旧lease、resource park、scenario別status/sync/substateが残る場合は自動復旧せず拒否する。prepared transaction内のnested snapshotも同じdecoderとvalidatorを通す。event type/orderは監査の完全性確認に使うが、runtime authorityには使わない。

## Release・delivery

| 境界 | 主な検証 |
| --- | --- |
| 決定的build、manifest、checksum、SBOM | `scripts/check-release.sh` |
| CLI surfaceとcredential不使用 | `scripts/cli-surface-contract.sh` |
| lifecycle fixture replayと再起動 | `scripts/offline-release-contract.sh` |
| production state非変更 | `scripts/production-state-isolation.sh` |
| candidate/stable同一artifact | release workflow candidate integrity・promotion evidence |
| repository別assignment、doctor、rollback drill | `scripts/production-assignment-health.sh` |
| active executionとworker上限 | production state/release/assignment health tests |

production確認では両repositoryに検証Issueを投入し、正常完了、needs-input中の後続進行、Issue-local failure中の後続進行、PR/check待ち中の後続進行を実測する。未trusted authorのskipと旧generation拒否は追加credentialを使わずfixture/fake serverで検証する。

## セキュリティ負テスト

Issue本文による権限拡張、secret永続化、path traversal、symlink、別repository worktree、stale generation、未知lifecycle API major、旧v5 runtime fieldを拒否する。通常suiteはmodel呼び出し、外部network、新規tokenを必要としない。
