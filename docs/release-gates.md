# Release gates

Snapshot v6 の配布保留（#575）: merge 順は #575 → #536 → #537。auto_merge は source 統合だけを許可し、release を許可しない。既存 `release.yml` の `verify-attestation-and-manifest` は旧条件（state=5、semantic=4/minimum=1、lifecycle=2.1）を維持する。v6 manifest はここで失敗し、`promotion-evidence` の `needs`、続く `promote-stable` により通常配布を止める。build・isolated canary・candidate integrity は公開許可ではない。新しい job・設定・配布機構は加えず、統合前の非互換版のタグは発行しない。

解除は #537 の PR で、#575/#536 を含む統合 SHA に対する証拠を確認した後、同じ PR で manifest 条件を v6 に更新する。必要な証拠は、契約変更/version CI、旧 `(5,4,2.0/2.1)` からの停止下 migration、旧 cancel 正規化、移行不能時の全体中止と原本 byte 一致、snapshot/event/prepared transaction の整合性、#439 の回答履歴と別 checkpoint の正常受理、未知版と新旧 reader の非破壊拒否、移行前 backup と旧 binary を対に戻す rollback fixture、必須品質 gate の成功。証拠を対象 SHA と結び付け、通常の release gate も維持する。本番操作・タグ発行・配布・rollback の実行は別の承認対象とする。

ローカルの`make ci`と通常PR CIは、Go 1.25.13でStaticcheck v0.7.0のSA系（`make staticcheck`）とerrcheck v1.10.0の`-blank`（`make errcheck`）を必須とする。errcheckは`internal/platform/fsutil`、`internal/adapter/state`、`internal/adapter/publish`をテスト込みで検査し、限定除外とその理由は`scripts/errcheck-excludes.txt`で管理する。lint導入を戻す場合は導入commitをrevertし、`make ci`で従来の品質ゲートを再検証する。この導入によるruntime依存、永続schema、公開APIの変更はなく、state migrationやproduction stateの書き換えは不要である。

外形監視のcursor/snapshot境界変更（Issue #282）はmonitor schema version 1と既存のstate配置を維持する。rollback evidenceは`monitor/internal/github`の固定page上限fixture、`monitor/internal/model`のlabel更新順序・terminal境界fixture、`monitor/internal/monitor`のsnapshot競合retry・非重複interval・観測gap優先順位fixtureと、`go test ./...`、monitor race、`go vet ./...`、`scripts/check-release.sh`の検証結果で確認する。配備前は変更commitのrevertで戻せる。配備後はmonitorを停止して専用stateを保全し、旧releaseのmonitor binaryでinstall/register/restartする（`monitor/docs/runbook.md`）。永続schema migrationやsupervisor stateの変更はない。旧版へ戻すとfail-closed保証も旧挙動へ戻るため、既存UNKNOWN区間を再分類せず保持する。productionでのrollback drill実施済みを意味する証拠ではなく、release公開には下記の既存gateを引き続き要求する。

Stable Releaseはsuffixのないannotated `vMAJOR.MINOR.PATCH` tagだけを起点にし、alpha/beta/RC suffixをstableへ昇格しない。stableが唯一の正式releaseであり、GAを別の段階として定義しない。tag pushだけでは公開されない。`build-candidate`が作成したbinary、SBOM、manifest、checksumsを唯一の配布正本とし、比較用buildを配布へ使わない。

Release公開とproduction assignmentは別のtransactionである。Release workflowはstable公開と同一artifact readbackで完了し、repositoryを更新しない。production rolloutはproduction hostのCLIと`scripts/production-assignment-health.sh`で、対象repositoryのhealthを検証する。delivery・状態互換性の変更時はtyped rollback drillと対象外repositoryの保全も検証する。rollout失敗はstableを未公開・未GAへ戻さず、対象assignmentの失敗として扱う。時間経過だけを目的とするEnvironment waitやartifact再取得waitはgateとして扱わない。

Release workflowは次の依存関係で実行する。各jobの失敗はstable公開を拒否する。

| 段階 | jobと依存関係 |
| --- | --- |
| 並列開始 | `release-quality`、`build-candidate`、`replay-production-fixtures`、`lifecycle-conformance`、`cli-surface-contract` |
| candidate検証 | `build-candidate`後に`verify-reproducibility`と`verify-attestation-and-manifest`を並列実行 |
| 隔離検証 | `build-candidate`と`cli-surface-contract`後に`credentialless-isolated-canary`を実行し、canonical bytesからcandidate prereleaseを作成 |
| 公開前照合 | candidate prerelease作成後に`candidate-integrity` |
| 公開許可 | 上記すべての成功後に`promotion-evidence` |
| 公開・確認 | `promote-stable` → `verify-stable-release` |

`release-quality`はformat、schema、module、workflow/shell lint、全体test、race、vet、Staticcheck、errcheck、脆弱性検査を行う。`make ci`に含まれる追加のbuild・release checkとfault/conformanceの再実行は、このjobでは行わない。配布artifactのbuildはcanonicalと再現性比較の計2回とし、fault/conformanceは専用jobで確認する。

ローカルでは変更箇所のfocused testを行い、同じcommitの通常CIが成功していれば全量検証をローカルでも繰り返す必要はない。CIを使えない場合の全量確認は`make ci`を1回実行する。releaseはtagged sourceを独立に検証するため、PRのmerge前headに対するCI成功だけを公開根拠にはしない。

`cli-surface-contract`は実際にinstallした`gh`とpinned Codex CLIの`--version`、`--help`、`features list`だけを検査し、GitHub API呼び出しもCodex inferenceも行わない。`credentialless-isolated-canary`はlocal bare Git remote、状態付きfake `gh`、状態付きfake Codex、隔離したstate rootを使い、外部networkを閉じて実candidate binaryを起動する。claimからmerge/terminalまでと、`needs_input`から停止・answer・true resume・merge/terminalまでの2 lifecycle、supervisor起動2回、5 crash boundary、webhook fixture replay、重複副作用と残存resourceが0であることを記録する。両scriptは`CANARY_GITHUB_TOKEN`または`OPENAI_API_KEY`が設定されている場合も失敗する。

本番snapshot比較と`production-state-report.json`の提出は通常releaseの条件にしない。状態保全は`credentialless-isolated-canary`内のrepository assignment分離・crash recoveryテストで検証し、本番hostへのアクセスやreport待機なしで公開する。実機特有の問題を調査する場合だけ`scripts/production-state-isolation.sh`を使い、その結果を通常releaseの成功条件には追加しない。raw evidenceはprivateに保持する。

`candidate-integrity`はcandidate prereleaseからbinaryを1回取得し、canonical candidateとのbyte一致とattestationを即時検査する。immutable artifactの再取得だけを目的とした時間待機は行わない。

このrepositoryは単一maintainer運用のため、外部collaboratorや自己承認不能なrequired reviewerをrelease authorityにしない。`High-risk review gate`は変更headに結び付いたmachine-readable reviewについて全check成功・finding 0件を必須とする。`promotion-evidence`はCLI surface、offline lifecycle、candidate integrityとdigestを即時再検証する。`production` Environmentはstable公開jobだけに付与し、wait timerは`0`とする。通常releaseのstable tagに加え、修正版workflowを実行するdefault branchを許可し、後者では入力tagとpeeled commitの一致をworkflow内でfail closedに検証する。未解決conversationはmain rulesetで引き続きmergeを拒否する。

tag push後にworkflow自体のrelease blockerを修正した場合は、tagを移動せず、default branchの修正版workflowを`workflow_dispatch`で実行する。入力したtagがannotated stable tagであること、そのpeeled commitが入力commitと一致すること、そのcommitが`main`のancestorであることを検査し、同じtagged sourceからcandidateを新規作成して全gateを再実行する。

manifestのsemantic contract predicateを変更する場合は、tag作成前なら変更commitをrevertして旧predicateへ戻せる。tag作成後はtagやcandidateを移動・再利用せず、修正版workflowをdefault branchへmergeして上記`workflow_dispatch`から同じtagged sourceの全gateを再実行する。

失敗したcandidateをstableへ昇格しない。candidate prereleaseは監査証拠として残し、修正は新しいcommitと新しいcandidateで全gateを再実行する。production rollout failure時は対象repositoryをprevious versionへrollbackし、state、active execution、continuation、request、worktreeを手編集しない。rollout failureだけを理由にRelease workflowを失敗へ戻したり、新patchを作成したりしない。

stable公開後はrepository別assignmentによる段階展開の後、production hostで5分間のhealth soakを行う。開始時、1分後、5分後に今回更新したrepositoryのassignment、scoped doctor、statusを採取し、全sampleが成功してから同じstable Releaseへreportを追加する。定期LaunchAgentの実行やEnvironment timerを待つ必要はない。

```sh
PRODUCTION_AGENT_LOOP_BINARY='/absolute/verified/stable/agent-loop' \
PRODUCTION_REPOSITORIES_FILE='/absolute/private/repositories.json' \
HEALTH_ARTIFACT_DIR='/absolute/evidence-directory' \
RELEASE_TAG='v0.9.0' \
RELEASE_COMMIT='<40-character-merge-commit>' \
STABLE_BINARY_SHA256='<stable-binary-sha256>' \
scripts/production-assignment-health.sh
gh release upload 'v0.9.0' '/absolute/evidence-directory/production-health-report.json'
```

privateな`repositories.json`には今回同じstableへ更新したrepositoryのIDとlocal pathを入力する。別versionを維持するrepositoryは含めず、公開reportへpathを出力しない。`rollback-drill.json`は先行repositoryのstable→previous→同じstableというtyped操作と、state、Issue、execution identity、worktree、対象外repositoryのassignment/PID/binary/state revision保全を記録する。`scripts/production-assignment-health.sh`は1件以上のrepositoryについてexact version/commit/digest、terminal transaction、fence不在、doctor成功、worker limit 1、active worker 1以下を各sampleで検証してreportを生成する。この検証が成功し、reportをstable Releaseへ追加した時点でrollout完了とする。検証失敗は対象assignmentの失敗として扱い、完了済みRelease workflowの結果を変更しない。

rollback drillは、前回割当commitから対象release commitまでの差分が次のいずれかに該当するとき、先行repositoryで実施する。

- deliveryのapply/rollback、slot検証、LaunchAgent切替、drain・checkpoint境界を変更する。
- state schema、semantic contract、Issue lifecycle API、migration、旧版による読み取り互換性を変更する。
- install/update/rollbackが割当runtimeや保存状態へ与える影響を変更する。

該当時はtyped rollbackと同じstableの再適用を検証し、`ROLLBACK_DRILL_FILE=/absolute/private/rollback-drill.json`を上のhealthコマンドへ渡す。旧版で読めない状態へのmigrationを伴う場合は、互換性runbookに従って復元可能性を先に確認し、通常のassignment rollbackを強行しない。該当しない変更ではdrillを省略し、reportの`rollback_drill`は`null`になる。これは未実施を表し、検証成功とは扱わない。指定されたdrill fileが存在しない・不正・失敗の場合はhealth scriptも失敗する。実施要否はoperatorが差分を確認して判断し、scriptによる変更範囲の自動判定は行わない。
