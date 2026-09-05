# 永続state schema / semantic migration runbook

現行artifactのstorage schemaはv5、semantic contractはv4、Issue lifecycle APIはv2.0である。storageはv4からv5へのforward migrationをサポートし、v5に旧runtime fieldまたは旧statusが残る入力は拒否する。binaryの`version --json`、install manifest、`migrate --json`はstorage schema、semantic contract、Issue lifecycle APIのcurrent/minimumを表示する。

contract v4はfieldを`optional`、`observational`、`execution_required_provenance`へ分類する。実行statusではroot `active_execution`、Issue `generation`、`workspace`の一致を要求し、待機時の再開材料をIssue-localな`continuation`、`continuation.evidence`、`suspension`へ分離する。宣言、対象status、validator、migration ruleは`internal/domain/statecontract`を単一のversioned sourceとする。execution-required fieldにruleを付けない変更はCIとrelease checkが失敗する。

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
| `EXECUTION_REQUIRED_ACTIVE_EXECUTION_MISSING` | 実行statusに一致するroot active executionがない |
| `PREPARED_TRANSACTION_REQUIRES_OLD_RUNTIME_RECOVERY` | 旧runtimeでprepared transactionを完了してから再previewする |

unknown storage/contract version、decode error、non-migratable findingがある場合はapplyしない。versionやWorkspaceを手編集しない。

旧releaseがsemantic contract不一致を`recovery_blocked`として隔離済みの場合、state/eventを手でcopyしない。repositoryをunloadしたまま`recover-semantic-quarantine --dry-run`でcurrent marker記載のexact backupを確認し、`--confirm-exact-backup`で1段ずつ戻す。`restored_recovery_marker=false`かつ元revision/Issue件数へ戻ったら通常の`status`を挟まず、全repository停止を確認してこの章の`migrate --json`へ進む。

## v4 recovery recordの変換

v4のscenario別recovery fieldは、status、旧lease/park、workspace、session、PR、request/answer、generationを同じsnapshotから読み、決定的にroot `active_execution`とIssue-local `continuation`、`suspension`へfoldする。event件数・順序はauthorityにしない。実行再開を一意に証明できないIssueだけを`recoverability=ambiguous`かつ`suspension.status=quarantined`にし、他Issueのmigrationとqueue進行は継続する。

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

v4→v5 migrationは11 Issue・14 legacy recovery substateのproduction由来matrixで、Issue、request/answer、execution generation、session、publication/PR auditの件数とidentityを保存する。state本体とprepared transaction内のnested snapshotへ同じ変換を適用し、current aggregate validatorを通す。

## Paired rollback

```sh
agent-loop stop --repo /absolute/path/to/repository
agent-loop migrate --rollback --backup '/absolute/path/from-apply' --json
agent-loop rollback --backup '/absolute/install-backup' --json
agent-loop doctor --json
```

rollbackは管理対象backup、restore先、全SHA-256を検証して全artifactを復元する。migrationが新規作成した空state用event logは削除する。active executionまたは未完了continuationがあれば拒否する。storage versionを跨ぐ場合はschema backupを先、install backupを後に戻す。途中失敗、backup不足、version不一致では片方だけを推測で戻さず停止を維持する。

旧外部配送用`notification-token`はmigration/backup/rollback対象外であり、暗黙削除しない。
