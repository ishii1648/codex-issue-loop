# 永続schema migration runbook

現行binaryが扱うconfig、registry、state、active event log、prepared transactionのschemaはv4である。v3からv4だけをforward migrationとしてサポートし、未知versionは変更しない。worker resultとdoctor JSONのschema versionは別契約であり、本runbookの対象ではない。

v4 migrationは廃止した外部配送設定をconfigから、配送outboxをstateとprepared transactionから除去する。active event logの旧配送eventはsequenceを維持した監査用markerへ置換し、payloadを破棄する。Issue、pending request、resource lease、worker session、publication stateは変更しない。

## Read-only preflight

新しい検証済みartifactからpreviewする。既定の`migrate`はfileを変更しない。

```sh
./agent-loop_Darwin_arm64 migrate --json
```

`report.needs_migration`、対象pathとversion、`unsupported`、`loaded_repositories`を確認する。`apply_allowed: false`の場合は適用しない。unsupportedやinspection errorがある場合も、fileを削除・手修正せずbackupして対応binaryを確認する。

## Schema変更を伴うupdate

1. 全repositoryの状態を記録し、すべてのloopを停止する。
2. 新artifactで`update`し、install backupを記録する。
3. installed binaryでmigrationをapplyし、migration backupを記録する。
4. doctor後、1 repositoryずつstartする。

```sh
agent-loop stop --repo /absolute/path/to/repository
./agent-loop_Darwin_arm64 update --json
agent-loop migrate --json
agent-loop migrate --apply --json
agent-loop doctor --json
agent-loop start --repo /absolute/path/to/repository
```

v3が残るupdateは、稼働LaunchAgentが1件でもあれば拒否する。成功時は`schema_migration_required: true`を返し、自動再開しない。migration applyは全対象を`~/Library/Application Support/codex-issue-loop/migrations/`へchecksum付きでbackupし、`migration.json`を先に`prepared`として保存する。

## 途中停止からの再開

apply途中でprocessやMacが停止した場合は、同じ新binaryで再度previewしてから`migrate --apply`を実行する。prepared journalがある場合は新しいbackupを増やさず、元のbackupを再利用してv3のfileだけを変換する。全fileが既にv4ならjournalだけを`completed`へ収束させる。

手作業でversionだけを書き換えたり、`migration.json`、state、eventを削除して先へ進まない。

## Paired rollback

旧binaryがv3を要求する場合、必ずschemaを先、installationを後の順で戻す。

```sh
agent-loop stop --repo /absolute/path/to/repository
agent-loop migrate --rollback \
  --backup '/Users/name/Library/Application Support/codex-issue-loop/migrations/<migration-backup>' \
  --json
agent-loop rollback \
  --backup '/Users/name/Library/Application Support/codex-issue-loop/backups/<install-backup>' \
  --json
agent-loop doctor --json
agent-loop start --repo /absolute/path/to/repository
```

`migrate --rollback`は管理対象backupだけを受け付け、manifestのrestore先と全fileのSHA-256を検証する。schemaがinstall backupの対応versionと一致しない状態で`rollback`するとCLIは拒否する。migration backupとinstall backupのどちらかが欠ける場合は、片方だけを戻さず現versionで停止したまま復旧方針を決める。

v4 stateにactive resource leaseが1件でもある間は、v3へrollbackできない。`needs_input`、retry待ち、checks/merge待ちもactive leaseである。対応Issueをterminalへ収束させてleaseが原子的に解放されたことをpreviewで確認してからrollbackする。

## 旧credential file

旧外部配送用の`<repo-state-dir>/notification-token`はmigration、update、uninstallで暗黙削除せず、その場に0600のまま保持する。migration backupやstate/event/logへcopyせず、新binaryは読み込まない。これはv3 rollback時に旧binaryが同じfileを再利用できるようにするためである。

rollback不要と判断した後だけ、loop停止中に対象repositoryのstate directoryとfile modeを確認し、運用者が明示的に当該fileだけを削除する。pathを推測したglobやstate directory全体の削除は行わない。削除後は復元できないため、必要なら削除前に秘密情報用の承認済み保管先へ別途退避する。
