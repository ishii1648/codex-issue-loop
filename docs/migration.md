# 永続state schema / semantic migration runbook

開発中の snapshot 永続化契約は単一 `version=6`。旧 `(5,4,2.0/2.1)` を v6 として自動受理・読み替えしない。v6 に `semantic_contract_version` または `issue_lifecycle_api_version` が残る入力も拒否する。config/registry は v5、CLI/monitor の schema は従来どおりである。契約本体と version 更新規則は [architecture §10](architecture.md#10-issue-lifecycle-apiと互換性) を参照する。

**#575→#536→#537 の統合・検証が完了するまで v6 のタグ発行・通常配布は禁止する。** 現在の migration は既存 v4→v5 および v5 semantic v4 までの経路を維持する。以下はその旧経路の手順であり、v6 への移行完了や v6 runtime の起動許可を意味しない。`ValidateLegacy` はこの旧出力専用であり、v6 の Store は拒否する。release metadata の migration-from=5 は v6 の移行元を識別し、移行済みの保証ではない。v6 向け起動前 migration と旧 cancel 正規化は #537 で実装する。

v6 移行では、旧3項目を照合し、全 snapshot と prepared transaction を共通契約で検証する。移行不能状態が1件でもあれば、原本を変更せず移行全体を中止する。停止下で移行前 backup と対応 binary を対で保全し、rollback は停止したままその対へ戻す。旧 binary に v6 を直接読ませない。この文書の更新は、本番停止・migration・配布・rollback の実行承認ではない。

## Read-only preview

新しい検証済みartifactで、loopを停止する前にもpreviewできる。既定の`migrate`はstate、event、label、worktree、backup、journalを一切変更しない。

```sh
agent-loop migrate --json
```

`report.semantic_findings`はIssueごとに`repo_id`、`issue_number`、`status`、`field`、stable `code`、`migratable`、`reason`、`migration_rule`を返す。`report.unsupported`と`report.non_migratable`と`loaded_repositories`が空で、`loaded_webhook_broker`がfalseの場合だけ`apply_allowed`がtrueになる。

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

旧releaseがIssue lifecycle API不一致だけを`recovery_blocked`として隔離した場合も手動copyしない。repositoryをunloadし、`recover-lifecycle-quarantine --dry-run`でmarkerのexact reason、source/target version、recorded backup、digest、revision、snapshot/eventと任意のprepared transactionの整合性を確認してから、`--confirm-exact-backup`でbyte-exactに復元する。

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

schema変更を伴う`update`、`migrate --apply`、`migrate --rollback`、旧schemaへの`rollback`は、全登録LaunchAgentに加えて共有webhook brokerの停止も要求する。`update`出力の`webhook_broker_restarted`はbrokerを再起動したかを示し、schema変更時はfalseになる。

applyは全登録LaunchAgentと共有webhook brokerの停止を確認し、対象config/registry/state/active eventをchecksum付きbackupへ保存してから`migration.json`を`prepared`にする。stateの`semantic_contract_version`と同じtransaction boundaryを表す`semantic_migration_applied` eventにはmigration ID、authority、source、before/after、`operator_confirmation.apply=true`、`provenance_synthesized=false`を記録する。GitHub labelとworktreeはmigration対象外である。

fileごとの置換はatomicで、同じprepared journalを使う再実行は同じmigration ID/event IDへ収束する。completed後の再applyは`changed:false`である。process crash後は同じartifactでpreviewしてからapplyを再実行し、別backupや別identityを作らない。fault後に旧versionへ戻す場合は下記rollbackを使う。

v4→v5 migrationは11 Issue・14 legacy recovery substateのproduction由来matrixで、Issue、request/answer、execution generation、session、publication/PR auditの件数とidentityを保存する。state本体とprepared transaction内のnested snapshotへ同じ変換を適用し、domain の旧 v5 専用 aggregate validatorを通す。

## Paired rollback

```sh
agent-loop stop --repo /absolute/path/to/repository
agent-loop migrate --rollback --backup '/absolute/path/from-apply' --json
agent-loop rollback --backup '/absolute/install-backup' --json
agent-loop doctor --json
```

rollbackは管理対象backup、restore先、全SHA-256を検証して全artifactを復元する。migrationが新規作成した空state用event logは削除する。active executionまたは未完了continuationがあれば拒否する。最新journalと異なるbackup、移行直後のstate revisionを記録していないbackup、現在のstate revisionが記録値と異なる場合も拒否する。prepared journalからの障害復旧では、backupと同一byteの未移行stateも復元できる。運用再開後にstateが更新された場合の強制rollbackは提供しない。storage versionを跨ぐ場合はschema backupを先、install backupを後に戻す。途中失敗、backup不足、version不一致では片方だけを推測で戻さず停止を維持する。

旧外部配送用`notification-token`はmigration/backup/rollback対象外であり、暗黙削除しない。
