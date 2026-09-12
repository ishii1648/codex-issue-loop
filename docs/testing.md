# テストマトリクス

## 実行方法

```sh
make test
make fault-test
make test-race
make install-shellcheck
make workflow-shell-check
scripts/check-release.sh
```

通常suite、`TestFault`障害注入suite、race detector、release gateを独立して成功させる。外部GitHub APIやCodex inferenceを使うcontract testは置かず、local bare Git remote、fake GitHub/worker、fixture replay、隔離したHOMEで再現する。

`make workflow-shell-check`は固定版のactionlintとShellCheckで全workflowの`run`と`scripts/*.sh`を検査し、通常PR CIと`make ci`でも実行する。初回または`make clean`後は`make install-shellcheck`で開発用の`bin/shellcheck`を取得する。未導入・version不一致・実行失敗は検査失敗になる。

## 中核ドメイン契約

| 契約 | 主な検証 |
| --- | --- |
| Issue lifecycle APIの許可遷移とmajor互換性 | `internal/domain/issue`、`internal/application/conformance` |
| root active executionが常に0/1件 | `TestConcurrentExecutionStartsHaveSingleWinner`、snapshot validator suite |
| Issue番号・run ID・generationのfence | `internal/adapter/state/execution_test.go` |
| waiting・terminal・quarantineで実行枠解放 | lifecycle boundary、supervisor reconciliation suite |
| needs-input回答後の同一continuation再開 | `TestRunOncePersistsQuestion`、`TestAnswerDurablyWaitsWithoutStealingActiveExecution` |
| 1 Issueの失敗・入力待ち・PR/check待ち・quarantine後もそのIssueを再admitせず後続を取得 | scheduler fault/conformance suite |
| 作成者がtrusted ownerであるIssueだけを受理 | `internal/adapter/github/author_test.go`、`internal/domain/queue`、scheduler author verification suite |
| root pending effectによるGitHub副作用の冪等性 | publication、GitHub sync、partial failure suite |
| 先行PR merge後も後続PRを同一intentで継続し、base履歴分岐は拒否 | `TestPublishAllowsExistingPullRequestWhenBaseBranchFastForwards`、`TestPublishRefusesExistingPullRequestWhenBaseHistoryDiverges` |
| Go formatterはoperator環境依存shimでなくself-containedなtoolchain実体を固定 | `TestRegistryPinsToolchainGofmtWhenDiscoveredCommandNeedsUserEnvironment`、`TestGofmtCapabilityProbeDoesNotInheritOperatorEnvironment` |
| 再登録で`gh`等がoperator環境依存shimへ退行せず、検証済みmulticall symlinkを維持し、canonical実体と入口symlinkをdrift扱いしない | `TestRegistryPinsAquaManagedCommandToResolvedExecutable`、`TestRegistryKeepsSelfContainedCommandWhenDiscoveredPathNeedsOperatorEnvironment`、`TestRegistryPreservesRuntimeProbedMulticallSymlink`、`TestRegistryRejectsEnvironmentDependentCommandWithoutSafeFallback`、`TestSameExecutableAcceptsSymlinkToRegisteredCanonicalPath` |
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
| 実機調査時のproduction state非変更（通常release条件外） | `scripts/production-state-isolation.sh` |
| candidate/stable同一artifact | release workflow candidate integrity・promotion evidence |
| repository別assignment、doctor、変更内容に応じたrollback drill | `scripts/production-assignment-health.sh` |
| active executionとworker上限 | production state/release/assignment health tests |

通常のproduction rolloutは[Release gates](release-gates.md)のローカル検証・証拠保存で完了し、キューが空なら検証Issueの投入や仕事の到着待ちを要求しない。別途productionでlifecycleの実測を行う場合は両repositoryに検証Issueを投入し、正常完了、needs-input中の後続進行、Issue-local failure中の後続進行、PR/check待ち中の後続進行を実測する。未trusted authorのskipと旧generation拒否は追加credentialを使わずfixture/fake serverで検証する。

## セキュリティ負テスト

Issue本文による権限拡張、secret永続化、path traversal、symlink、別repository worktree、stale generation、未知lifecycle API major、旧v5 runtime fieldを拒否する。通常suiteはmodel呼び出し、外部network、新規tokenを必要としない。

## 契約テストの変更レポート

`.github/protected-tests.tsv` は、種別（`test` / `control`）、リポジトリ相対の完全パス（末尾 `/` はディレクトリ）、理由をタブで区切る固定登録である。schedulerの隔離後の後続起動・取消、stateの単一実行権・fencing・永続transaction、共有fixture/helperと検査基盤を対象にする。これはレビューの注目箇所を検出する範囲であり、保護外の変更を安全と認定するものではない。

`Contract test changes` workflowは `pull_request_target` の固定baseをcheckoutし、headを実行せずGit objectとしてfetchする。検査処理と登録はbase版を使う。`BASE_SHA` / `HEAD_SHA` は完全commit SHAを必須とし、checkoutとbase、fetch結果とheadの一致を確認する。差分は一意なmerge-baseからheadまでの `git diff --no-renames --name-status -z`。移動は旧パスの削除と新パスの追加として記録する。head更新・base branch変更で再実行し、mainへのpushでもそのbaseを対象にopen PRを再検査する。

`review-artifacts/protected-tests.json` にbase/head/merge-baseと検出した変更種別・旧新パス・理由を出力する。既存consumer向けの `dedicated_pr_required` は常に `false` とし、専用PRを要求しない。検出した変更の有無は `changes`、検査失敗は `error` で区別する。参照不能・不正定義・差分取得失敗・artifact欠落は失敗になる。変更を検出しただけでは失敗しない。正常終了は検査完了を表し、仕様・保証の変更が妥当という判定ではない。

`test` の独立した新規 `_test.go` 追加だけはレポート対象から除外する。既存ファイルの編集・削除・mode/type変更、移動元の削除は検出する。`control` の保護定義・検査処理・workflow・Makefile・依存固定・実行スクリプト・fixtureは追加も検出する。

仕様・実装・テストは同じPRで変更・検証する。既存PRテンプレートの「仕様・保証への影響」に、必要な場合だけ変更前後の動作、要求・承認根拠、失われる保証、維持する保証と代替検証を記載する。テスト単独の先行マージや専用PRへの分離は要求しない。検査基盤自身の変更も信頼済みbaseで検出し、レビュー対象とする。

意味上の妥当性は元要求・変更前仕様・テスト・実装差分を合わせてレビューする。#341の独立レビューと#593の対象HEADの実行証拠検証は未完成であり、このレポートはそれらを代替しない。Quality gatesとHigh-risk reviewの既存チェックを維持し、レポートの成功を仕様承認・テスト実行成功として扱わない。必須レビューの本番有効化は、正当な変更の承認経路と古い結果・欠損・失敗の拒否を検証してから行う。
