# 永続state schema / semantic migration runbook

現行artifactのstorage schemaはv5、semantic contractはv3である。storageはv4からv5へのforward migrationと、v5 semantic v2からv3へのin-schema migrationをサポートする。binaryの`version --json`、install manifest、`migrate --json`はstorage schemaのcurrent/migration-fromとsemantic contractのcurrent/minimumを表示する。

contract v3はfieldを`optional`、`observational`、`execution_required_provenance`へ分類する。`issues[].workspace`と実行statusの`issues[].execution_lease`を検査し、terminal capacityと中断provenanceを`ExecutionLease`、`ContinuationCheckpoint`、`Suspension`へ分離する。v2からはgeneric checkpointのstageだけを`resume|publish|checks|conflict`へ正規化し、evidenceとsuspensionを保持する。宣言、対象status、validator、migration ruleは`internal/domain/statecontract`を単一のversioned sourceとする。execution-required fieldにruleを付けない変更はCIとrelease checkが失敗する。

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
| `EXECUTION_REQUIRED_LEASE_MISSING` | 実行statusにfenced ExecutionLeaseがない |
| `PREPARED_TRANSACTION_REQUIRES_OLD_RUNTIME_RECOVERY` | 旧runtimeでprepared transactionを完了してから再previewする |

unknown storage/contract version、decode error、non-migratable findingがある場合はapplyしない。versionやWorkspaceを手編集しない。

semantic contract mismatchは破損ではなくversioned migration要求なので、runtime readはstate/eventをquarantineせず副作用なしで拒否する。v0.11.2以前のreadによって、対応可能な旧semantic snapshotが既に`recovery_blocked`へ隔離された場合だけ、exact markerとbackupを指定して専用previewを行う。

```sh
agent-loop migrate --repo /absolute/path/to/repository \
  --quarantined-backup '/exact/managed/recovery-backup' --json
agent-loop migrate --repo /absolute/path/to/repository \
  --quarantined-backup '/exact/managed/recovery-backup' --apply --json
```

この経路は、markerが記録したexact backup、理由が`semantic_contract_version`の旧supported versionからcurrentへの不一致だけであること、全LaunchAgent停止、worker/PID/PGID/ExecutionLease 0、prepared transaction不在、repository identity、source digest、migrated snapshot/event chain、Issue/request件数を検証する。applyは元backupを変更せずrecovery markerも別backupへ保持し、`quarantined_backup=true`のsemantic migration auditとmigration済みsnapshot/eventをcrash-safe transactionで一括置換する。別の破損理由、minimum未満/current以上、active execution、digest変化には流用しない。

## v4 recovery recordの変換

v4のscenario別recovery fieldは、status、lease/park、workspace、session、PR、request/answer、generationを同じsnapshotから読み、決定的に`ContinuationCheckpoint`と`Suspension`へfoldする。event件数・順序はauthorityにしない。実行再開を一意に証明できないIssueだけを`recoverability=ambiguous`かつ`suspension.status=quarantined`にし、他Issueのmigrationとqueue進行は継続する。

migration後のoperator操作は共通CLIだけを使う。

```sh
agent-loop issue plan --repo /absolute/path/to/repository --issue 123 --json
agent-loop issue resolve --repo /absolute/path/to/repository --issue 123 --action resume --json
```

planがworkspace、git、GitHub、processの不一致を返した場合はstate/labelを手編集しない。外部状態を修復して再planするか、`cancel`でそのIssueだけを収束させる。

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

v4→v5 migrationは11 Issue・14 legacy recovery substateのproduction由来matrixで、Issue、request/answer、lease generation、session、publication/PR auditの件数とidentityを保存する。state本体とprepared transaction内のnested snapshotへ同じ変換を適用し、current aggregate validatorを通す。

## Paired rollback

```sh
agent-loop stop --repo /absolute/path/to/repository
agent-loop migrate --rollback --backup '/absolute/path/from-apply' --json
agent-loop rollback --backup '/absolute/install-backup' --json
agent-loop doctor --json
```

rollbackは管理対象backup、restore先、全SHA-256を検証して全artifactを復元する。migrationが新規作成した空state用event logは削除する。active leaseまたはparked continuationがあれば拒否する。storage versionを跨ぐ場合はschema backupを先、install backupを後に戻す。途中失敗、backup不足、version不一致では片方だけを推測で戻さず停止を維持する。

旧外部配送用`notification-token`はmigration/backup/rollback対象外であり、暗黙削除しない。
