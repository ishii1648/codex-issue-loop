# 永続schema migration runbook

現行binaryが扱うconfig、registry、state、active event log、prepared transactionのschemaはv2である。v1からv2だけをforward migrationとしてサポートし、未知versionは変更しない。worker resultとdoctor JSONのschema versionは別契約であり、本runbookの対象ではない。

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

v1が残るupdateは、稼働LaunchAgentが1件でもあれば拒否する。成功時は`schema_migration_required: true`を返し、自動再開しない。migration applyは全対象を`~/Library/Application Support/codex-issue-loop/migrations/`へchecksum付きでbackupし、`migration.json`を先に`prepared`として保存する。

## 途中停止からの再開

apply途中でprocessやMacが停止した場合は、同じ新binaryで再度previewしてから`migrate --apply`を実行する。prepared journalがある場合は新しいbackupを増やさず、元のbackupを再利用してv1のfileだけを変換する。全fileが既にv2ならjournalだけを`completed`へ収束させる。

手作業でversionだけを書き換えたり、`migration.json`、state、eventを削除して先へ進まない。

## Paired rollback

旧binaryがv1を要求する場合、必ずschemaを先、installationを後の順で戻す。

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
