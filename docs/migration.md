# 永続state schema / semantic migration runbook

現行artifactのstorage schemaはv4、semantic contractはv1である。storageはv3からv4へのforward migrationを、v4 stateは暗黙contract v0から明示contract v1へのmigrationをサポートする。binaryの`version --json`、install manifest、`migrate --json`はstorage schemaのcurrent/migration-fromとsemantic contractのcurrent/minimumを表示する。

contract v1はfieldを`optional`、`observational`、`execution_required_provenance`へ分類する。`issues[].workspace`はworker実行境界を越えたactive、blocked、needs-input、retry、Pull Request / publication recovery stateでexecution-requiredである。宣言、対象status、validator、migration ruleは`internal/domain/statecontract`を単一のversioned sourceとする。execution-required fieldにruleを付けない変更はCIとrelease checkが失敗する。

## Read-only preview

新しい検証済みartifactで、loopを停止する前にもpreviewできる。既定の`migrate`はstate、event、label、worktree、backup、journalを一切変更しない。

```sh
agent-loop migrate --json
```

`report.semantic_findings`はIssueごとに`repo_id`、`issue_number`、`status`、`field`、stable `code`、`migratable`、`reason`、`migration_rule`を返す。`report.non_migratable`が空で、`loaded_repositories`も空の場合だけ`apply_allowed`がtrueになる。

主なcode:

| code | 意味 |
| --- | --- |
| `SEMANTIC_COMPATIBLE` | 現releaseの実行不変条件を満たす |
| `EXECUTION_REQUIRED_WORKSPACE_PROVENANCE_MISSING` | 実行済みrecovery stateにWorkspace authorityがない。自動合成しない |
| `EXECUTION_REQUIRED_WORKSPACE_PROVENANCE_INVALID` | 保存provenanceがIssue/repository identityと不整合 |
| `PREPARED_TRANSACTION_REQUIRES_OLD_RUNTIME_RECOVERY` | 旧runtimeでprepared transactionを完了してから再previewする |

unknown storage/contract version、decode error、non-migratable findingがある場合はapplyしない。versionやWorkspaceを手編集しない。

## non-migratable Workspace recovery

missing Workspace provenanceをsession、worktree path、lease、PR identityから推測して埋めない。v4の対応artifactと既存の限定recoveryを使い、全loop停止中に次の順で処理する。

1. v4 artifactのchecksum/provenanceを確認する。
2. v4の`resume-blocked --issue <number> --confirm-prerequisite-resolved --json`を実行する。
3. exact durable chain、GitHub、worktree/branch/HEAD、repository identityの検証が成功し、`environment_resume_recovered` eventへauthority、source、expected/actual、operator confirmationが記録されたことを確認する。
4. workerを再開する前に停止し、新artifactの`migrate --json`を再実行する。
5. findingが`SEMANTIC_COMPATIBLE`になった後だけcontract migrationをapplyする。

専用lifecycle recoveryに一致しないlegacy terminal recordは、全loop停止中に次のvalidation-only recoveryを使う。

```sh
agent-loop recover-workspace --repo /absolute/path/to/repository --issue 123 --dry-run --json
agent-loop recover-workspace --repo /absolute/path/to/repository --issue 123 --confirm-verified-workspace --json
```

これは保存worktreeからWorkspace identityを検証し、監査record/eventとともにbackfillするだけである。status、lease、resource park、session、GitHub、worktreeは変更しない。適用後に各Issueの`workspace_provenance_recovery.status=verified`と不変のlifecycle metadataを確認し、`migrate --json`の`report.non_migratable`が空になってからapplyする。

いずれのrecoveryも拒否されたstateはnon-migratableのまま保全し、state/labelを手編集しない。

## Apply、restart、idempotency

```sh
agent-loop stop --repo /absolute/path/to/repository
agent-loop migrate --json
agent-loop migrate --apply --json
agent-loop doctor --json
agent-loop start --repo /absolute/path/to/repository
```

applyは全登録LaunchAgentの停止を確認し、対象config/registry/state/active eventをchecksum付きbackupへ保存してから`migration.json`を`prepared`にする。stateの`semantic_contract_version`と同じtransaction boundaryを表す`semantic_migration_applied` eventにはmigration ID、authority、source、before/after、`operator_confirmation.apply=true`、`provenance_synthesized=false`を記録する。GitHub labelとworktreeはmigration対象外である。

fileごとの置換はatomicで、同じprepared journalを使う再実行は同じmigration ID/event IDへ収束する。completed後の再applyは`changed:false`である。process crash後は同じartifactでpreviewしてからapplyを再実行し、別backupや別identityを作らない。fault後に旧versionへ戻す場合は下記rollbackを使う。

v3→v4 migrationは廃止した外部配送設定/outboxを削除し、旧配送eventをsequence保持markerへ置換する。Issue、request、lease、session、publication stateは保持した上で同じsemantic validatorを通す。

## Paired rollback

```sh
agent-loop stop --repo /absolute/path/to/repository
agent-loop migrate --rollback --backup '/absolute/path/from-apply' --json
agent-loop rollback --backup '/absolute/install-backup' --json
agent-loop doctor --json
```

rollbackは管理対象backup、restore先、全SHA-256を検証して全artifactを復元する。migrationが新規作成した空state用event logは削除する。active leaseまたはparked continuationがあれば拒否する。storage versionを跨ぐ場合はschema backupを先、install backupを後に戻す。途中失敗、backup不足、version不一致では片方だけを推測で戻さず停止を維持する。

旧外部配送用`notification-token`はmigration/backup/rollback対象外であり、暗黙削除しない。
