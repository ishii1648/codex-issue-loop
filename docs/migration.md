# 永続schema migration runbook

現行binaryが扱うconfig、registry、state、active event log、prepared transactionのschemaはv3である。v2からv3だけをforward migrationとしてサポートし、未知versionは変更しない。worker resultとdoctor JSONのschema versionは別契約であり、本runbookの対象ではない。previewの`fallback_leases`には、v3で`repo:*`のexclusive leaseへ変換されるv2 active Issueが表示される。

## worker backend追加時のv2後方互換

`worker.backend`を持たない既存v2 manifestは`codex`として読み込む。既存stateの`session_id`はCodexだけが生成していたため、load時に`session: {"backend":"codex","id":"..."}`へin-memory正規化し、次のstate更新で併記する。この変更にschema versionの更新や一括書換えは不要である。

backendを変更する場合はloopを停止し、manifest更新後に`agent-loop register --repo <path>`を再実行する。active Issueに別backendのsessionが残っていても、そのIDを新backendへ渡さず、既存worktree・run state・回答履歴からfresh sessionを開始する。

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

v2が残るupdateは、稼働LaunchAgentが1件でもあれば拒否する。成功時は`schema_migration_required: true`を返し、自動再開しない。migration applyは全対象を`~/Library/Application Support/codex-issue-loop/migrations/`へchecksum付きでbackupし、`migration.json`を先に`prepared`として保存する。

## 途中停止からの再開

apply途中でprocessやMacが停止した場合は、同じ新binaryで再度previewしてから`migrate --apply`を実行する。prepared journalがある場合は新しいbackupを増やさず、元のbackupを再利用してv2のfileだけを変換する。全fileが既にv3ならjournalだけを`completed`へ収束させる。

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

v3 stateにactive resource leaseが1件でもある間は、v2へrollbackできない。`needs_input`、retry待ち、checks/merge待ちもactive leaseである。対応Issueをterminalへ収束させてleaseが原子的に解放されたことをpreviewで確認してからrollbackする。
