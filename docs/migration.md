# Snapshot migration runbook

Snapshot API の現行契約は単一 `version=6`。移行元は `(version, semantic_contract_version, issue_lifecycle_api_version)=(5,4,2.0/2.1)` に限る。config/registry は v5 のままである。v6 reader は旧版、未知版、旧2項目が残る v6 を拒否する。旧 binary に v6 を直接読ませない。

## 更新の確認と起動

更新前に、使用する検証済み binary で移行内容を確認する。

```sh
agent-loop migrate --json
```

preview は snapshot、event、worktree、GitHub、backup、journal を変更しない。対応しない契約、取消根拠の矛盾、実行権限、worker identity、未回答要求、未完了 effect があれば移行できない。旧 prepared transaction は対応する旧 binary で完了してから停止し、再度 preview する。supervisor PID が存在しないことだけでは停止確認にならない。

preview を確認した operator が、全対象 loop と共有 webhook broker を停止して検証済み binary の `update` を実行する。`update` は artifact の更新と移行要否の表示を行い、snapshot は起動時に変換する。更新後の `start`、手動 `run`、再起動では改めて承認を求めない。起動要求から排他取得、version 判定、必要な移行、新契約の検証、永続確定、通常処理の順に進む。

```sh
agent-loop update --json
agent-loop start --repo /absolute/path/to/repository
```

停止下で明示的に適用する場合は `agent-loop migrate --apply --json` を使える。repository assignment と自動 delivery の schema/major 更新拒否は維持する。互換性の異なる slot を通常 assignment で強制採用せず、停止下の更新手順を使う。新規 repository の登録は v6 の空 snapshot を作成する。既存旧 snapshot がある repository の再登録を migration の代用にしない。

## 旧 cancel の predicate

対象は blocked/failed かつ suspension が resolved/cancel の記録である。Cancellation が未作成で、保存済み `resolved_at` と `suspended_at` が存在し、取消時刻が停止時刻より前でないことを要求する。実行権限・worker identity・未回答要求・未完了 effect が残る snapshot、取消根拠不足・矛盾がある snapshot は原本を変更せず全体を拒否する。新契約の aggregate 検証も全件に適用する。

取消記録は `source=legacy_cancel_migration`、`previous_status=旧status`、`canceled_at=保存済みresolved_at`、`execution_release_result=not_present` とする。`not_present` は移行時の実行権限不在を表し、過去に解放処理を実行した証明ではない。欠けた operator/process 情報は補完しない。移行時刻は別の `snapshot_migration_applied` event と journal の `completed_at` に記録する。

既に canceled の記録は検証して保持し、別 resolution は取消変換しない。v6 は resolved/cancel と canceled/Cancellation/取消時刻の一致を要求する。再実行で取消記録を作り直さない。

PR参照、open PR、PR終了証拠不足、worktree欠損だけでは移行を拒否しない。移行は GitHub 操作も worker 起動も行わない。通常起動後の reconciliation が保存済みPRの repository identity と現在状態を確認し、open PR を close して readback する。closed/merged は保持する。GitHub 同期失敗では内部取消を巻き戻さず、既存 reconciliation の再実行で収束する。worktree の復元や実装 worker の再投入はしない。

## 排他・中断・再実行

移行は既存 `migration.lock` と全対象の `supervisor.lock` / `state.lock` を保持する。起動元自身の supervisor lock は呼び出し側が保持する。別 supervisor・CLI と排他的に全入力を検証し、checksum 付き backup と `migration.json` を再利用する。

journal の `prepared` 中は通常 Store の読み書きを拒否し、途中の snapshot/event を隔離しない。snapshot と version は同一の atomic file replacement で更新する。途中で終了した場合は同じ binary の `migrate --apply` または起動で、同じ backup・migration ID・event ID から変換を完了する。現在値が移行前または検証済み移行後のどちらでもない場合は、進捗を上書きせず拒否する。全 artifact の検証後にだけ journal を completed とする。

## Paired rollback

全 loop と broker を停止したまま、移行前 backup と対応する旧 binary を対で戻す。

```sh
agent-loop migrate --rollback --backup /absolute/path/to/migration-backup --json
agent-loop rollback --backup /absolute/path/to/install-backup --json
```

既存 rollback 検証は backup の checksum・対象範囲・最新 journal・移行直後 revision を確認する。active execution や保持中 continuation、移行後の進捗、backup の不足・不一致があれば拒否する。新しい snapshot を古い backup へ黙って戻す強制手段は提供しない。state の復元が失敗した場合は旧 binary を起動せず停止を維持する。

この手順書やテストの成功は、本番停止・migration 適用・タグ発行・配布・rollback 実行の承認ではない。release の公開境界は [release gates](release-gates.md) を参照する。
