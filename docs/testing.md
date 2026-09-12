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

## 保護された契約テストの差分ゲート

`.github/protected-tests.tsv` は、種別（`test` / `control`）、リポジトリ相対の完全パス（末尾 `/` はディレクトリ）、理由をタブで区切る固定登録である。schedulerの隔離後の後続起動・取消、stateの単一実行権・fencing・永続transactionを初期対象とする。`supervisor_test.go` の `testLoop`、`execution_test.go` の `newStore`、`validator_test.go` のsnapshot生成helperとsupervisorのtestdataも含む。大きな既存ファイルを保護するため、同じファイル内の無関係なテスト整理も検出する。

`Protected contract tests` workflowは `pull_request_target` の固定baseをcheckoutし、headを実行せずGit objectとしてfetchする。検査処理と登録はbase版を使う。`BASE_SHA` / `HEAD_SHA` は完全commit SHAを必須とし、checkoutとbase、fetch結果とheadの一致を確認する。差分は一意なmerge-baseからheadまでの `git diff --no-renames --name-status -z`。移動は旧パスの削除と新パスの追加として記録し、類似度やGitのrename設定に依存しない。head更新・base branch変更で再実行し、mainへのpushでもそのbaseを対象にopen PRを再検査する。

`review-artifacts/protected-tests.json` にbase/head/merge-base、`dedicated_pr_required`、検出した変更種別・旧新パス・理由を出力する。参照不能・不正定義・差分取得失敗はerrorと失敗終了になり、workflowの取得・同一性確認失敗やartifact欠落もpassにしない。`test` の独立した新規 `_test.go` 追加だけは免除する。既存ファイルの編集・削除・mode/type変更、移動元の削除は免除しない。`control` の保護定義・検査処理・workflow・Makefile・依存固定・実行スクリプト・fixtureは追加も検出する。

専用PRが必要な場合、ゲートは現段階では常に失敗する。labelや本文で解除する入力はない。#581ではこのbase/headに紐付いた判定を受け、専用PRの説明・独立レビュー（#341）の対象版・承認済みテスト差分と関連実装PRの一致を検証してから統合を許可する必要がある。承認済み差分の統合による免除はまだ実装していない。古いbase/headの結果を流用せず、統合時の再検証も必要である。

これは検出とfail-closedなCI接続であり、専用PRの承認・統合やマージ防止の完成ではない。特にbase pushの再検査結果を各PRの最新の必須判定へ結び付ける手続き、trusted workflowの初回導入、本番ruleset・権限の設定は残る統合依存である。workflow自体の信頼はGitHub側のbase版実行に依存する。保護外のproductionコード・他のtest helper・build tag・`TestMain` 等からテストを弱体化する変更や、実行環境・外部action/toolchainの変更は、このパス検査では意味を判定しない。保護外の変更を仕様不変と認定せず、独立レビューと既存品質・実行証拠ゲートを維持する。
