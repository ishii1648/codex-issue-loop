# ADR-0008: runtimeの確定SnapshotをUnix socketで公開する

- Status: Accepted
- Date: 2026-09-13

## Context

GitHubのrunningラベルだけでは、実行枠の所有者とresume_pendingを区別できない。独立monitorが保存ファイル・transaction・validatorを直接実装すると、runtimeの状態契約を二重管理することになる。

## Decision

repository runtimeにGo標準HTTP serverを組み込み、同一ユーザー用Unix socketの `GET /v1/status` で確定Snapshotの限定projectionを返す。通信契約の正本は [Runtime status API v1](../runtime-status-api.md)。state adapterが共有lockと未確定transactionを確認し、既存canonical readとvalidatorを使う。履歴配信、状態変更、TCP listener、独立daemon、別の永続化やキャッシュは追加しない。

[monitor ADR-0001](../../monitor/docs/adr/0001-independent-github-black-box-monitor.md) のGitHubだけを入力にする制約は、今後のruntime状態取得には適用しない。独立monitor processとruntimeの責務分離は維持する。monitorの移行自体はこの変更に含めず、runtime実装の検証・main統合と通信契約の確定後に実施する。既存の配布保留は解除しない。

[ADR-0003](0003-event-notification.md) のUnix socket不採用はevent通知・broadcast方式の判断であり、今回の要求ごとの状態読み取りとは別である。既存watchのfsnotify契約は維持する。

## Consequences

monitorはruntimeの状態機械を再実装せず取得できる。GitHub起動時同期待ちでも保存状態と今回の起動を区別できる。一方、runtime停止時はsocketへ接続できないため、その失敗と業務停止を区別する必要がある。API成功やrevision停滞だけではworkerの正常進行を判定できない。API障害はログに記録し、schedulerやworkerの終了理由にしない。
